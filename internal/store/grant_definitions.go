package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrGrantDefinitionNotFound is returned when a grant definition lookup
// misses.
var ErrGrantDefinitionNotFound = errors.New("grant definition not found")

// ErrGrantDefinitionDuplicate is returned when an admin tries to create a
// definition whose name conflicts with an existing active definition.
var ErrGrantDefinitionDuplicate = errors.New("grant definition with this name already exists")

// ErrGrantDefinitionSlugDuplicate is returned when a definition's slug
// conflicts with an existing definition's slug (the slug is unique across
// every definition, active or not — unlike name, which is only unique
// among active definitions).
var ErrGrantDefinitionSlugDuplicate = errors.New("grant definition with this slug already exists")

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

	result := &GrantDefinition{
		Name:                def.Name,
		Slug:                def.Slug,
		Description:         def.Description,
		DurationSeconds:     def.DurationSeconds,
		Controls:            controls,
		MaxQueryCounts:      def.MaxQueryCounts,
		MaxBytesTransferred: def.MaxBytesTransferred,
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
		// A name collision against an existing active definition violates the
		// partial unique index; surface it as a typed sentinel so the API can
		// return 409 DUPLICATE_NAME instead of an opaque 500.
		if isUniqueViolation(err, "grant_definitions_active_name_uniq") {
			return nil, ErrGrantDefinitionDuplicate
		}
		if isUniqueViolation(err, "grant_definitions_slug_uniq") {
			return nil, ErrGrantDefinitionSlugDuplicate
		}
		return nil, fmt.Errorf("create grant definition: %w", err)
	}

	return result, nil
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

// GetGrantDefinitionBySlug fetches a definition by slug. Returns
// ErrGrantDefinitionNotFound if no definition has that slug — same sentinel
// as GetGrantDefinition, so callers can try one after the other without
// distinguishing the miss.
func (s *Store) GetGrantDefinitionBySlug(ctx context.Context, slug string) (*GrantDefinition, error) {
	def := new(GrantDefinition)

	err := s.db.NewSelect().
		Model(def).
		Where("slug = ?", slug).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGrantDefinitionNotFound
		}

		return nil, fmt.Errorf("get grant definition by slug: %w", err)
	}

	return def, nil
}

// ListGrantDefinitions returns definitions matching the filter. Admins want
// `ActiveOnly=false` to see soft-deleted entries; the request UI passes
// `ActiveOnly=true`.
func (s *Store) ListGrantDefinitions(ctx context.Context, filter GrantDefinitionFilter) ([]GrantDefinition, error) {
	var defs []GrantDefinition

	q := s.db.NewSelect().Model(&defs)

	if filter.ActiveOnly {
		q = q.Where("is_active = ?", true)
	}

	q = q.Order("created_at DESC")

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("list grant definitions: %w", err)
	}

	return defs, nil
}

// UpdateGrantDefinition mutates an existing definition. Only the
// admin-editable fields are touched; uid / created_by / created_at /
// is_active stay put. Use DeactivateGrantDefinition for the lifecycle flip.
func (s *Store) UpdateGrantDefinition(ctx context.Context, def *GrantDefinition) error {
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

	// Use Column-based update so bun applies the same array marshaling that
	// the model's `bun:"controls,array"` tag uses on Insert.
	res, err := s.db.NewUpdate().
		Model(def).
		Column("name", "slug", "description", "duration_seconds", "controls", "max_query_counts",
			"max_bytes_transferred", "auto_approve", "group_uids", "database_uids",
			"approval_patterns", "approver_group_uids").
		Where("uid = ?", def.UID).
		Exec(ctx)
	if err != nil {
		if isUniqueViolation(err, "grant_definitions_slug_uniq") {
			return ErrGrantDefinitionSlugDuplicate
		}
		return fmt.Errorf("update grant definition: %w", err)
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrGrantDefinitionNotFound
	}

	return nil
}

// DeactivateGrantDefinition flips is_active to false. We never hard-delete
// because grant_requests reference definitions and we want the historical
// trail to stay intact.
func (s *Store) DeactivateGrantDefinition(ctx context.Context, uid uuid.UUID) error {
	res, err := s.db.NewUpdate().
		Model((*GrantDefinition)(nil)).
		Set("is_active = ?", false).
		Where("uid = ?", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("deactivate grant definition: %w", err)
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrGrantDefinitionNotFound
	}

	return nil
}
