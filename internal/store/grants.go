package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// BuildGrantFromDefinition assembles an AccessGrant from a GrantDefinition
// + the requesting user/database, anchoring the time window to `now`. Used
// by both the grant-request approval path and any future admin shortcut
// that wants to materialize a definition into a concrete grant.
func BuildGrantFromDefinition(def *GrantDefinition, userID, databaseID, grantedBy uuid.UUID, now time.Time) *Grant {
	controls := append([]string(nil), def.Controls...)
	if controls == nil {
		controls = []string{}
	}

	return &Grant{
		UserID:              userID,
		DatabaseID:          databaseID,
		Controls:            controls,
		GrantedBy:           grantedBy,
		StartsAt:            now,
		ExpiresAt:           now.Add(time.Duration(def.DurationSeconds) * time.Second),
		MaxQueryCounts:      def.MaxQueryCounts,
		MaxBytesTransferred: def.MaxBytesTransferred,
	}
}

// CreateGrant creates a new access grant
func (s *Store) CreateGrant(ctx context.Context, grant *Grant) (*Grant, error) {
	// Ensure Controls is not nil
	controls := grant.Controls
	if controls == nil {
		controls = []string{}
	}

	result := &AccessGrant{
		UserID:              grant.UserID,
		DatabaseID:          grant.DatabaseID,
		Controls:            controls,
		GrantedBy:           grant.GrantedBy,
		StartsAt:            grant.StartsAt,
		ExpiresAt:           grant.ExpiresAt,
		MaxQueryCounts:      grant.MaxQueryCounts,
		MaxBytesTransferred: grant.MaxBytesTransferred,
		CreatedAt:           time.Now(),
	}

	_, err := s.db.NewInsert().
		Model(result).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create grant: %w", err)
	}

	result.QueryCount = 0
	result.BytesTransferred = 0
	return result, nil
}

// GetActiveGrant retrieves an active grant for a user and database
func (s *Store) GetActiveGrant(ctx context.Context, userID, databaseID uuid.UUID) (*Grant, error) {
	grant := new(AccessGrant)
	err := s.db.NewSelect().
		Model(grant).
		Where("user_id = ?", userID).
		Where("database_id = ?", databaseID).
		Where("revoked_at IS NULL").
		Where("starts_at <= NOW()").
		Where("expires_at > NOW()").
		Order("created_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoActiveGrant
		}
		return nil, fmt.Errorf("failed to get active grant: %w", err)
	}

	if err := s.populateGrantCounters(ctx, grant); err != nil {
		return nil, err
	}
	return grant, nil
}

// GetGrantByUID retrieves a grant by UID
func (s *Store) GetGrantByUID(ctx context.Context, uid uuid.UUID) (*Grant, error) {
	grant := new(AccessGrant)
	err := s.db.NewSelect().
		Model(grant).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGrantNotFound
		}
		return nil, fmt.Errorf("failed to get grant: %w", err)
	}

	if err := s.populateGrantCounters(ctx, grant); err != nil {
		return nil, err
	}
	return grant, nil
}

// ListGrants retrieves grants with optional filters
func (s *Store) ListGrants(ctx context.Context, filter GrantFilter) ([]Grant, error) {
	var grants []AccessGrant
	q := s.db.NewSelect().Model(&grants)

	if filter.UserID != nil {
		q = q.Where("user_id = ?", *filter.UserID)
	}

	if filter.DatabaseID != nil {
		q = q.Where("database_id = ?", *filter.DatabaseID)
	}

	if filter.ActiveOnly {
		q = q.Where("revoked_at IS NULL").
			Where("starts_at <= NOW()").
			Where("expires_at > NOW()")
	}

	err := q.Order("created_at DESC").Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list grants: %w", err)
	}

	if grants == nil {
		grants = []AccessGrant{}
	}
	for i := range grants {
		if err := s.populateGrantCounters(ctx, &grants[i]); err != nil {
			return nil, err
		}
	}
	return grants, nil
}

// populateGrantCounters fills the transient QueryCount and BytesTransferred
// fields of g by aggregating from the queries and connections tables within
// the grant's effective time window: [StartsAt, min(ExpiresAt, RevokedAt)).
func (s *Store) populateGrantCounters(ctx context.Context, g *AccessGrant) error {
	upper := g.ExpiresAt
	if g.RevokedAt != nil && g.RevokedAt.Before(upper) {
		upper = *g.RevokedAt
	}

	var queryCount int64
	err := s.db.NewSelect().
		ColumnExpr("COUNT(*)").
		TableExpr("queries AS q").
		Join("JOIN connections AS c ON q.connection_id = c.uid").
		Where("c.user_id = ?", g.UserID).
		Where("c.database_id = ?", g.DatabaseID).
		Where("q.executed_at >= ?", g.StartsAt).
		Where("q.executed_at < ?", upper).
		Scan(ctx, &queryCount)
	if err != nil {
		return fmt.Errorf("failed to aggregate grant query count: %w", err)
	}

	var bytesTransferred int64
	err = s.db.NewSelect().
		ColumnExpr("COALESCE(SUM(bytes_transferred), 0)").
		Model((*Connection)(nil)).
		Where("user_id = ?", g.UserID).
		Where("database_id = ?", g.DatabaseID).
		Where("connected_at >= ?", g.StartsAt).
		Where("connected_at < ?", upper).
		Scan(ctx, &bytesTransferred)
	if err != nil {
		return fmt.Errorf("failed to aggregate grant bytes transferred: %w", err)
	}

	g.QueryCount = queryCount
	g.BytesTransferred = bytesTransferred
	return nil
}

// RevokeGrant revokes a grant
func (s *Store) RevokeGrant(ctx context.Context, uid uuid.UUID, revokedBy uuid.UUID) error {
	now := time.Now()
	result, err := s.db.NewUpdate().
		Model((*AccessGrant)(nil)).
		Where("uid = ?", uid).
		Where("revoked_at IS NULL").
		Set("revoked_at = ?", now).
		Set("revoked_by = ?", revokedBy).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to revoke grant: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrGrantAlreadyRevoked
	}

	return nil
}
