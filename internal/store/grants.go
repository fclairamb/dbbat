package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Auto-calculated grant priority tiers. A user holding several active grants
// on one database gets the highest-priority one at auth time, so the default
// ranking has to encode "the grant that lets me do the most wins" — otherwise
// creation order decides, which nobody controls deliberately.
//
// The gaps between tiers are the point: an operator can slot a manual override
// between two tiers (say 75) without renumbering anything.
const (
	// PriorityFullWrite ranks a writable grant carrying no controls at all.
	PriorityFullWrite int16 = 100
	// PriorityRestrictedWrite ranks a still-writable grant that carries
	// controls (block_copy / block_ddl).
	PriorityRestrictedWrite int16 = 50
	// PriorityReadOnly ranks a read_only grant, whatever else it carries —
	// read_only is the most restrictive control, so it loses to anything
	// writable.
	PriorityReadOnly int16 = 10
)

// AutoPriority computes the default selection priority for a grant with the
// given controls. It is the single source of truth for the tiering: the API,
// the definition-materialization path and the SQL backfill in
// 20260806000000_grants_priority.up.sql all mirror this exact formula.
func AutoPriority(controls []string) int16 {
	for _, c := range controls {
		if c == ControlReadOnly {
			return PriorityReadOnly
		}
	}

	if len(controls) == 0 {
		return PriorityFullWrite
	}

	return PriorityRestrictedWrite
}

// ResolvePriority returns the priority a grant carrying these controls should
// be stored with. Zero — the column default, the Go zero value and the value a
// caller that never heard of priorities leaves behind — reads as "unset" and
// falls back to AutoPriority. Every creation path funnels through here, so no
// path can silently insert a grant ranked below every tier. An operator who
// genuinely wants a grant that always loses sets 1 (or a negative value;
// smallint goes down to -32768).
func ResolvePriority(explicit int16, controls []string) int16 {
	if explicit != 0 {
		return explicit
	}

	return AutoPriority(controls)
}

// BuildGrantFromDefinition assembles an AccessGrant from a GrantDefinition
// + the requesting user/database, anchoring the time window to `now`. Used
// by both the grant-request approval path and any future admin shortcut
// that wants to materialize a definition into a concrete grant.
func BuildGrantFromDefinition(def *GrantDefinition, userID, databaseID, grantedBy uuid.UUID, now time.Time) *Grant {
	controls := append([]string(nil), def.Controls...)
	if controls == nil {
		controls = []string{}
	}

	// A definition's priority is optional: nil means "whatever the controls
	// earn", which is what every definition predating the column wants.
	var explicitPriority int16
	if def.Priority != nil {
		explicitPriority = *def.Priority
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
		Priority:            ResolvePriority(explicitPriority, controls),
		// Mirrored, not joined: the proxy session holds only a *Grant, and
		// resolving patterns off the definition at query time would mean a
		// join on the hot path plus zero coverage for direct admin grants.
		ApprovalPatterns:  copyStrings(def.ApprovalPatterns),
		ApproverGroupUIDs: copyUUIDs(def.ApproverGroupUIDs),
	}
}

// copyStrings returns a non-nil copy so a mutation of the definition's slice
// can never reach a materialized grant (and so bun writes '{}' not NULL).
func copyStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)

	return out
}

// copyUUIDs is copyStrings for uuid slices.
func copyUUIDs(in []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, len(in))
	copy(out, in)

	return out
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
		ApprovalPatterns:    copyStrings(grant.ApprovalPatterns),
		ApproverGroupUIDs:   copyUUIDs(grant.ApproverGroupUIDs),
		Priority:            ResolvePriority(grant.Priority, controls),
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
		// Highest priority wins. Ties go to the grant that lasts longest (a
		// session is pinned to the grant it was admitted under for its whole
		// life, so the longer window is the more useful pick), then to the
		// newest — which is the pre-priority behavior, preserved as the last
		// tie-break so nothing changes for users holding a single grant.
		Order("priority DESC", "expires_at DESC", "created_at DESC").
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

	// Same ordering as GetActiveGrant, so the UI lists grants in the order the
	// proxy would pick them: whichever active grant is on top of a given
	// (user, database) group is the one a new session gets.
	err := q.Order("priority DESC", "expires_at DESC", "created_at DESC").Scan(ctx)
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
//
// A connection stamped with this grant's UID (connections.grant_uid) is
// attributed to it directly and unconditionally — that is the auth-time pick
// recorded by CreateConnection, not a guess. A connection with no stamp
// (grant_uid IS NULL: a legacy row, or the owning grant was deleted) falls
// back to the pre-stamp heuristic of matching (user_id, database_id) within
// the time window.
//
// The two conditions are deliberately exclusive rather than OR'd together
// unconditionally: a *stamped* connection only ever matches the grant named
// by its own grant_uid, never another grant's window, even if both grants
// cover the same user/database and overlap in time. That is what makes two
// overlapping grants attribute traffic separately instead of double-counting
// it — see TestGrantCounters_StampedConnectionExcludedFromOtherGrantsWindow.
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
		Where("(c.grant_uid = ? OR (c.grant_uid IS NULL AND c.user_id = ? AND c.database_id = ?))",
			g.UID, g.UserID, g.DatabaseID).
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
		Where("(grant_uid = ? OR (grant_uid IS NULL AND user_id = ? AND database_id = ?))",
			g.UID, g.UserID, g.DatabaseID).
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
