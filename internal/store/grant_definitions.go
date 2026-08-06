package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// ErrGrantDefinitionNotFound is returned when a grant definition lookup
// misses.
var ErrGrantDefinitionNotFound = errors.New("grant definition not found")

// ErrGrantDefinitionDuplicate is returned when an admin tries to create a
// definition whose name conflicts with an existing active definition.
var ErrGrantDefinitionDuplicate = errors.New("grant definition with this name already exists")

// ErrGrantDefinitionSlugDuplicate is returned when a definition's slug
// conflicts with another *live* definition's slug. Archived versions keep
// their slug and never conflict — one slug has exactly one live owner and any
// number of superseded ones.
var ErrGrantDefinitionSlugDuplicate = errors.New("grant definition with this slug already exists")

// ErrGrantDefinitionArchived is returned when an operation that only makes
// sense on the current version of a definition (editing it) is attempted on a
// version some later edit already superseded.
var ErrGrantDefinitionArchived = errors.New("grant definition version has been superseded")

// GrantDefinitionInUseError is returned when a definition cannot be hard
// deleted because rows still reference it. Deleting anyway would orphan
// grants, whose grant_definition_id is NOT NULL — a grant with no shape is
// exactly what this model exists to make impossible.
type GrantDefinitionInUseError struct {
	Grants   int64
	Requests int64
}

func (e *GrantDefinitionInUseError) Error() string {
	return fmt.Sprintf("grant definition is referenced by %d grant(s) and %d grant request(s)", e.Grants, e.Requests)
}

// CreateGrantDefinition inserts a new GrantDefinition. The unique-active-name
// index enforces no two active definitions share a name; deactivated
// definitions don't block reuse.
func (s *Store) CreateGrantDefinition(ctx context.Context, def *GrantDefinition) (*GrantDefinition, error) {
	controls := def.Controls
	if controls == nil {
		controls = []string{}
	}

	groupUIDs := def.GroupUIDs
	if groupUIDs == nil {
		groupUIDs = []uuid.UUID{}
	}

	databaseUIDs := def.DatabaseUIDs
	if databaseUIDs == nil {
		databaseUIDs = []uuid.UUID{}
	}

	// A brand-new definition is the root of its own lineage, so its uid has to
	// be known before the insert rather than defaulted by the database.
	uid := uuid.New()

	result := &GrantDefinition{
		UID:                 uid,
		LineageUID:          uid,
		Name:                def.Name,
		Slug:                def.Slug,
		Description:         def.Description,
		DurationSeconds:     def.DurationSeconds,
		Controls:            controls,
		MaxQueryCounts:      def.MaxQueryCounts,
		MaxBytesTransferred: def.MaxBytesTransferred,
		Priority:            def.Priority,
		AutoApprove:         def.AutoApprove,
		GroupUIDs:           groupUIDs,
		DatabaseUIDs:        databaseUIDs,
		ApprovalPatterns:    copyStrings(def.ApprovalPatterns),
		ApproverGroupUIDs:   copyUUIDs(def.ApproverGroupUIDs),
		IsActive:            true,
		CreatedBy:           def.CreatedBy,
		CreatedAt:           time.Now(),
	}

	_, err := s.db.NewInsert().
		Model(result).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, mapGrantDefinitionWriteError(err)
	}

	return result, nil
}

// mapGrantDefinitionWriteError translates the unique-index violations a
// definition insert can raise into typed sentinels, so the API returns a 409
// naming the offending field instead of an opaque 500.
func mapGrantDefinitionWriteError(err error) error {
	if isUniqueViolation(err, "grant_definitions_active_name_uniq") {
		return ErrGrantDefinitionDuplicate
	}

	// grant_definitions_slug_uniq was the pre-versioning global constraint;
	// grant_definitions_live_slug_uniq is the partial index that replaced it.
	// Both are checked so the mapping survives a database that has not run the
	// versioning migration yet.
	if isUniqueViolation(err, "grant_definitions_live_slug_uniq") ||
		isUniqueViolation(err, "grant_definitions_slug_uniq") {
		return ErrGrantDefinitionSlugDuplicate
	}

	return fmt.Errorf("write grant definition: %w", err)
}

// GetGrantDefinition fetches a definition by UID. Returns
// ErrGrantDefinitionNotFound if the row doesn't exist.
func (s *Store) GetGrantDefinition(ctx context.Context, uid uuid.UUID) (*GrantDefinition, error) {
	def := new(GrantDefinition)

	err := s.db.NewSelect().
		Model(def).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGrantDefinitionNotFound
		}

		return nil, fmt.Errorf("get grant definition: %w", err)
	}

	return def, nil
}

// GetGrantDefinitionBySlug fetches the **live** definition with that slug.
// Archived versions keep their slug, so resolving by slug has to be pinned to
// `archived_at IS NULL` — addressing a historical version is by uid, which is
// the only unambiguous handle for one. Returns ErrGrantDefinitionNotFound if
// no live definition has that slug — same sentinel as GetGrantDefinition, so
// callers can try one after the other without distinguishing the miss.
func (s *Store) GetGrantDefinitionBySlug(ctx context.Context, slug string) (*GrantDefinition, error) {
	def := new(GrantDefinition)

	err := s.db.NewSelect().
		Model(def).
		Where("slug = ?", slug).
		Where("archived_at IS NULL").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGrantDefinitionNotFound
		}

		return nil, fmt.Errorf("get grant definition by slug: %w", err)
	}

	return def, nil
}

// GetLiveGrantDefinition returns the current version of the lineage the given
// definition uid belongs to. Passing the live version's own uid returns it
// unchanged; passing an archived version walks forward to whatever superseded
// it. Used wherever a decision must apply *today's* policy — approving a
// request filed before an edit, for instance — rather than the version that
// happened to be current when the row was first referenced.
func (s *Store) GetLiveGrantDefinition(ctx context.Context, uid uuid.UUID) (*GrantDefinition, error) {
	def := new(GrantDefinition)

	err := s.db.NewSelect().
		Model(def).
		Where("archived_at IS NULL").
		Where("lineage_uid = (SELECT lineage_uid FROM grant_definitions WHERE uid = ?)", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGrantDefinitionNotFound
		}

		return nil, fmt.Errorf("get live grant definition: %w", err)
	}

	return def, nil
}

// ListGrantDefinitions returns live definitions matching the filter — archived
// versions never appear in a listing or a picker, since issuing from a
// superseded version is exactly what versioning exists to prevent. Admins want
// `ActiveOnly=false` to see deactivated entries; the request UI passes
// `ActiveOnly=true`.
//
// Each returned definition carries ActiveGrantCount for its whole lineage, so
// the UI can tell an operator how much access deactivating it would cut off.
// It is one grouped query, not one per row.
func (s *Store) ListGrantDefinitions(ctx context.Context, filter GrantDefinitionFilter) ([]GrantDefinition, error) {
	var defs []GrantDefinition

	q := s.db.NewSelect().Model(&defs).Where("archived_at IS NULL")

	if filter.ActiveOnly {
		q = q.Where("is_active = ?", true)
	}

	q = q.Order("created_at DESC")

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("list grant definitions: %w", err)
	}

	if err := s.attachActiveGrantCounts(ctx, defs); err != nil {
		return nil, err
	}

	return defs, nil
}

// attachActiveGrantCounts fills ActiveGrantCount on each definition with the
// number of currently-authorising grants across its lineage.
func (s *Store) attachActiveGrantCounts(ctx context.Context, defs []GrantDefinition) error {
	if len(defs) == 0 {
		return nil
	}

	lineages := make([]uuid.UUID, 0, len(defs))
	seen := make(map[uuid.UUID]struct{}, len(defs))

	for i := range defs {
		if _, dup := seen[defs[i].LineageUID]; dup {
			continue
		}

		seen[defs[i].LineageUID] = struct{}{}
		lineages = append(lineages, defs[i].LineageUID)
	}

	var rows []struct {
		LineageUID uuid.UUID `bun:"lineage_uid"`
		Count      int64     `bun:"count"`
	}

	err := s.db.NewSelect().
		TableExpr("access_grants AS ag").
		Join("JOIN grant_definitions AS gd ON gd.uid = ag.grant_definition_id").
		ColumnExpr("gd.lineage_uid AS lineage_uid").
		ColumnExpr("count(*) AS count").
		Where("gd.lineage_uid IN (?)", bun.In(lineages)).
		Where("ag.revoked_at IS NULL").
		Where("ag.expires_at > NOW()").
		GroupExpr("gd.lineage_uid").
		Scan(ctx, &rows)
	if err != nil {
		return fmt.Errorf("count grants per grant definition: %w", err)
	}

	counts := make(map[uuid.UUID]int64, len(rows))
	for _, r := range rows {
		counts[r.LineageUID] = r.Count
	}

	for i := range defs {
		defs[i].ActiveGrantCount = counts[defs[i].LineageUID]
	}

	return nil
}

// CountGrantsForLineage reports how many grants the lineage of the given
// definition uid is responsible for: every grant ever issued from any of its
// versions, and the subset still authorising access. Both numbers matter — the
// first is what blocks a hard delete, the second is what an operator needs to
// see before deactivating.
func (s *Store) CountGrantsForLineage(ctx context.Context, uid uuid.UUID) (total, active int64, err error) {
	var row struct {
		Total  int64 `bun:"total"`
		Active int64 `bun:"active"`
	}

	err = s.db.NewSelect().
		TableExpr("access_grants AS ag").
		Join("JOIN grant_definitions AS gd ON gd.uid = ag.grant_definition_id").
		ColumnExpr("count(*) AS total").
		ColumnExpr("count(*) FILTER (WHERE ag.revoked_at IS NULL AND ag.expires_at > NOW()) AS active").
		Where("gd.lineage_uid = (SELECT lineage_uid FROM grant_definitions WHERE uid = ?)", uid).
		Scan(ctx, &row)
	if err != nil {
		return 0, 0, fmt.Errorf("count grants for grant definition: %w", err)
	}

	return row.Total, row.Active, nil
}

// UpdateGrantDefinition edits a definition by **versioning** it: the current
// row is archived (`archived_at = now()`) and a successor carrying the change
// is inserted with the same lineage. It returns the new version.
//
// Nothing is mutated in place, and that is the whole point: grants pin the
// exact row they were issued from, so tightening — or loosening — a definition
// can never retroactively change access that is already live. The new version
// applies to everything issued from here on.
//
// A no-op edit (every editable field already equal) is not versioned; the
// existing row is returned untouched, so a targeted PATCH that happens to
// change nothing doesn't litter the history.
//
// Editing an already-archived version is refused with
// ErrGrantDefinitionArchived: history is immutable, and the caller means the
// live version.
func (s *Store) UpdateGrantDefinition(ctx context.Context, def *GrantDefinition) (*GrantDefinition, error) {
	if def.Controls == nil {
		def.Controls = []string{}
	}

	if def.GroupUIDs == nil {
		def.GroupUIDs = []uuid.UUID{}
	}

	if def.DatabaseUIDs == nil {
		def.DatabaseUIDs = []uuid.UUID{}
	}

	def.ApprovalPatterns = copyStrings(def.ApprovalPatterns)
	def.ApproverGroupUIDs = copyUUIDs(def.ApproverGroupUIDs)

	var result *GrantDefinition

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		current := new(GrantDefinition)
		if err := tx.NewSelect().Model(current).Where("uid = ?", def.UID).For("UPDATE").Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGrantDefinitionNotFound
			}

			return fmt.Errorf("select grant definition: %w", err)
		}

		if current.ArchivedAt != nil {
			return ErrGrantDefinitionArchived
		}

		if sameGrantDefinitionShape(current, def) {
			result = current

			return nil
		}

		now := time.Now()

		if _, err := tx.NewUpdate().
			Model((*GrantDefinition)(nil)).
			Set("archived_at = ?", now).
			Where("uid = ?", current.UID).
			Exec(ctx); err != nil {
			return fmt.Errorf("archive grant definition: %w", err)
		}

		next := &GrantDefinition{
			UID:        uuid.New(),
			LineageUID: current.LineageUID,
			Name:       def.Name,
			Slug:       def.Slug,
			// Description and the rest come from the merged definition the
			// caller assembled; lifecycle stays with the lineage.
			Description:         def.Description,
			DurationSeconds:     def.DurationSeconds,
			Controls:            def.Controls,
			MaxQueryCounts:      def.MaxQueryCounts,
			MaxBytesTransferred: def.MaxBytesTransferred,
			Priority:            def.Priority,
			AutoApprove:         def.AutoApprove,
			GroupUIDs:           def.GroupUIDs,
			DatabaseUIDs:        def.DatabaseUIDs,
			ApprovalPatterns:    def.ApprovalPatterns,
			ApproverGroupUIDs:   def.ApproverGroupUIDs,
			IsActive:            current.IsActive,
			CreatedBy:           current.CreatedBy,
			CreatedAt:           now,
		}

		if _, err := tx.NewInsert().Model(next).Returning("*").Exec(ctx); err != nil {
			return mapGrantDefinitionWriteError(err)
		}

		result = next

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// sameGrantDefinitionShape reports whether every editable field of two
// definitions already matches, i.e. whether an edit would be a no-op.
func sameGrantDefinitionShape(a, b *GrantDefinition) bool {
	return a.Name == b.Name &&
		a.Slug == b.Slug &&
		a.Description == b.Description &&
		a.DurationSeconds == b.DurationSeconds &&
		a.AutoApprove == b.AutoApprove &&
		slices.Equal(a.Controls, b.Controls) &&
		equalInt64Ptr(a.MaxQueryCounts, b.MaxQueryCounts) &&
		equalInt64Ptr(a.MaxBytesTransferred, b.MaxBytesTransferred) &&
		equalInt16Ptr(a.Priority, b.Priority) &&
		slices.Equal(a.GroupUIDs, b.GroupUIDs) &&
		slices.Equal(a.DatabaseUIDs, b.DatabaseUIDs) &&
		slices.Equal(a.ApprovalPatterns, b.ApprovalPatterns) &&
		slices.Equal(a.ApproverGroupUIDs, b.ApproverGroupUIDs)
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}

func equalInt16Ptr(a, b *int16) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}

// DeactivateGrantDefinition withdraws a definition across its **whole
// lineage** — every version, archived ones included.
//
// That reach is the point. An operator deactivates a policy, not one row of
// its version history; leaving older versions active would keep authorising
// the grants pinned to them and make deactivation a kill switch that doesn't
// kill. Auth checks is_active, so this fails those grants closed on their next
// connection.
func (s *Store) DeactivateGrantDefinition(ctx context.Context, uid uuid.UUID) error {
	return s.setGrantDefinitionActive(ctx, uid, false)
}

// ReactivateGrantDefinition is the inverse of DeactivateGrantDefinition, again
// across the whole lineage.
func (s *Store) ReactivateGrantDefinition(ctx context.Context, uid uuid.UUID) error {
	return s.setGrantDefinitionActive(ctx, uid, true)
}

func (s *Store) setGrantDefinitionActive(ctx context.Context, uid uuid.UUID, active bool) error {
	res, err := s.db.NewUpdate().
		Model((*GrantDefinition)(nil)).
		Set("is_active = ?", active).
		Where("lineage_uid = (SELECT lineage_uid FROM grant_definitions WHERE uid = ?)", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("set grant definition active: %w", err)
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrGrantDefinitionNotFound
	}

	return nil
}

// DeleteGrantDefinition hard-deletes a definition and every version of its
// lineage.
//
// It refuses — GrantDefinitionInUseError, carrying the blocking counts — as
// soon as anything references any version. Grants carry no shape of their own
// and their grant_definition_id is NOT NULL, so deleting a referenced
// definition would either be impossible (the FK says so) or would strand a
// grant whose behaviour nothing can describe. Deactivation is the operation
// for retiring a definition that has been used; deletion is only for one that
// never was.
func (s *Store) DeleteGrantDefinition(ctx context.Context, uid uuid.UUID) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var lineage uuid.UUID

		if err := tx.NewSelect().
			Model((*GrantDefinition)(nil)).
			Column("lineage_uid").
			Where("uid = ?", uid).
			Scan(ctx, &lineage); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGrantDefinitionNotFound
			}

			return fmt.Errorf("select grant definition lineage: %w", err)
		}

		grants, err := tx.NewSelect().
			TableExpr("access_grants AS ag").
			Join("JOIN grant_definitions AS gd ON gd.uid = ag.grant_definition_id").
			Where("gd.lineage_uid = ?", lineage).
			Count(ctx)
		if err != nil {
			return fmt.Errorf("count grants for grant definition: %w", err)
		}

		requests, err := tx.NewSelect().
			TableExpr("grant_requests AS gr").
			Join("JOIN grant_definitions AS gd ON gd.uid = gr.grant_definition_id").
			Where("gd.lineage_uid = ?", lineage).
			Count(ctx)
		if err != nil {
			return fmt.Errorf("count grant requests for grant definition: %w", err)
		}

		if grants > 0 || requests > 0 {
			return &GrantDefinitionInUseError{Grants: int64(grants), Requests: int64(requests)}
		}

		if _, err := tx.NewDelete().
			Model((*GrantDefinition)(nil)).
			Where("lineage_uid = ?", lineage).
			Exec(ctx); err != nil {
			return fmt.Errorf("delete grant definition: %w", err)
		}

		return nil
	})
}
