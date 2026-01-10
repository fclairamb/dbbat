package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

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

	// Get query count from queries table within the grant's time window
	var queryCount int64
	err = s.db.NewSelect().
		ColumnExpr("COUNT(*) as query_count").
		TableExpr("queries q").
		Join("JOIN connections c ON q.connection_id = c.uid").
		Where("c.user_id = ?", userID).
		Where("c.database_id = ?", databaseID).
		Where("q.executed_at >= ?", grant.StartsAt).
		Where("q.executed_at < ?", grant.ExpiresAt).
		Scan(ctx, &queryCount)
	if err != nil {
		// Non-fatal, just set to 0
		queryCount = 0
	}

	grant.QueryCount = queryCount
	grant.BytesTransferred = 0 // Tracked in-session only

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
	return grants, nil
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

// IncrementQueryCount increments the query count for tracking quota usage.
// This is called from the connections/queries tracking.
func (s *Store) IncrementQueryCount(_ context.Context, _ uuid.UUID) error {
	// Note: In the current schema, we don't have a query_count column on access_grants
	// We calculate it dynamically from connections/queries
	// This function is a placeholder for future optimization
	return nil
}

// IncrementBytesTransferred increments the bytes transferred for tracking quota usage.
func (s *Store) IncrementBytesTransferred(_ context.Context, _ uuid.UUID, _ int64) error {
	// Note: Similar to IncrementQueryCount, this is calculated dynamically
	// This function is a placeholder for future optimization
	return nil
}
