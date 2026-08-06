package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
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
// + the target user/database, anchoring the time window to `now`. It is the
// only way a grant comes into existence: the grant-request approval path and
// the admin direct-assign path both go through here.
//
// Nothing about the definition's shape is copied onto the grant — the grant
// pins the definition's uid and reads its shape back through the accessors on
// AccessGrant. The only value derived from the shape is Priority, which is a
// selection rank rather than policy and has to be stored so the ordering can
// happen in SQL.
func BuildGrantFromDefinition(def *GrantDefinition, userID, databaseID, grantedBy uuid.UUID, now time.Time) *Grant {
	// A definition's priority is optional: nil means "whatever the controls
	// earn", which is what every definition predating the column wants.
	var explicitPriority int16
	if def.Priority != nil {
		explicitPriority = *def.Priority
	}

	return &Grant{
		UserID:            userID,
		DatabaseID:        databaseID,
		GrantDefinitionID: def.UID,
		Definition:        def,
		GrantedBy:         grantedBy,
		StartsAt:          now,
		ExpiresAt:         now.Add(time.Duration(def.DurationSeconds) * time.Second),
		Priority:          ResolvePriority(explicitPriority, def.Controls),
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

// CreateGrant creates a new access grant. The grant must name the definition
// it is an instance of: a grant with no definition would carry no shape at
// all, and there is deliberately no code path that produces one.
func (s *Store) CreateGrant(ctx context.Context, grant *Grant) (*Grant, error) {
	if grant.GrantDefinitionID == uuid.Nil {
		return nil, ErrGrantDefinitionRequired
	}

	def := grant.Definition
	if def == nil || def.UID != grant.GrantDefinitionID {
		loaded, err := s.GetGrantDefinition(ctx, grant.GrantDefinitionID)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve grant definition: %w", err)
		}

		def = loaded
	}

	result := &AccessGrant{
		UserID:            grant.UserID,
		DatabaseID:        grant.DatabaseID,
		GrantDefinitionID: def.UID,
		GrantedBy:         grant.GrantedBy,
		StartsAt:          grant.StartsAt,
		ExpiresAt:         grant.ExpiresAt,
		Priority:          ResolvePriority(grant.Priority, def.Controls),
		CreatedAt:         time.Now(),
	}

	_, err := s.db.NewInsert().
		Model(result).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create grant: %w", err)
	}

	result.Definition = def
	result.QueryCount = 0
	result.BytesTransferred = 0
	return result, nil
}

// GetActiveGrant retrieves an active grant for a user and database.
//
// "Active" now includes the definition's own state: a grant whose definition
// was **deactivated** is not returned, so deactivation fails closed for every
// grant issued from that definition — including grants pinned to an older
// version, since deactivation applies to the whole lineage. Being
// **archived** (superseded by an edit) is explicitly not deactivation and is
// never consulted here: a grant keeps authorizing under the exact version it
// was issued from.
func (s *Store) GetActiveGrant(ctx context.Context, userID, databaseID uuid.UUID) (*Grant, error) {
	grant := new(AccessGrant)
	err := s.db.NewSelect().
		Model(grant).
		Where("user_id = ?", userID).
		Where("database_id = ?", databaseID).
		Where("revoked_at IS NULL").
		Where("starts_at <= NOW()").
		Where("expires_at > NOW()").
		Where("grant_definition_id IN (SELECT uid FROM grant_definitions WHERE is_active)").
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

	// A grant whose shape cannot be attached is unusable by definition, and
	// the caller is the proxy admitting a session — so report it as "no active
	// grant" rather than handing back something whose behavior is unknown.
	if err := s.attachDefinitions(ctx, []*AccessGrant{grant}); err != nil {
		return nil, err
	}

	if grant.Definition == nil {
		return nil, ErrNoActiveGrant
	}

	if err := s.populateGrantCounters(ctx, grant); err != nil {
		return nil, err
	}
	return grant, nil
}

// attachDefinitions loads the grant definitions the given grants point at and
// hangs them off each grant, in one batched query regardless of how many
// grants are involved.
//
// Deliberately not a bun relation: the auth path is the wrong place to depend
// on ORM join-alias behavior, and archived definitions must stay attachable
// (a grant issued from version 1 still has to render its shape after version 2
// exists), which a filtered join would make easy to get wrong.
func (s *Store) attachDefinitions(ctx context.Context, grants []*AccessGrant) error {
	if len(grants) == 0 {
		return nil
	}

	seen := make(map[uuid.UUID]struct{}, len(grants))
	uids := make([]uuid.UUID, 0, len(grants))

	for _, g := range grants {
		if g.GrantDefinitionID == uuid.Nil {
			continue
		}

		if _, dup := seen[g.GrantDefinitionID]; dup {
			continue
		}

		seen[g.GrantDefinitionID] = struct{}{}
		uids = append(uids, g.GrantDefinitionID)
	}

	if len(uids) == 0 {
		return nil
	}

	var defs []GrantDefinition
	if err := s.db.NewSelect().
		Model(&defs).
		Where("uid IN (?)", bun.List(uids)).
		Scan(ctx); err != nil {
		return fmt.Errorf("failed to load grant definitions: %w", err)
	}

	byUID := make(map[uuid.UUID]*GrantDefinition, len(defs))
	for i := range defs {
		byUID[defs[i].UID] = &defs[i]
	}

	for _, g := range grants {
		g.Definition = byUID[g.GrantDefinitionID]
	}

	return nil
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

	// Unfiltered on purpose: a lookup by uid is a history/detail read, so it
	// must still resolve a grant whose definition has since been deactivated
	// or superseded.
	if err := s.attachDefinitions(ctx, []*AccessGrant{grant}); err != nil {
		return nil, err
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

	refs := make([]*AccessGrant, len(grants))
	for i := range grants {
		refs[i] = &grants[i]
	}

	if err := s.attachDefinitions(ctx, refs); err != nil {
		return nil, err
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
