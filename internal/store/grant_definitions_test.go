package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func createTestAdmin(t *testing.T, ctx context.Context, store *Store, suffix string) *User {
	t.Helper()

	admin, err := store.CreateUser(ctx, "defadmin_"+suffix, "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	return admin
}

func TestCreateGrantDefinition(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "create")

	maxQ := int64(1000)

	def := &GrantDefinition{
		Name:                "Read-only 1h",
		Slug:                "read-only-1h",
		Description:         "Standard read access for an hour",
		DurationSeconds:     3600,
		Controls:            []string{ControlReadOnly},
		MaxQueryCounts:      &maxQ,
		MaxBytesTransferred: nil,
		CreatedBy:           admin.UID,
	}

	created, err := store.CreateGrantDefinition(ctx, def)
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	if created.UID == uuid.Nil {
		t.Error("UID = uuid.Nil")
	}

	if !created.IsActive {
		t.Error("IsActive should default to true")
	}

	if created.Name != "Read-only 1h" {
		t.Errorf("Name = %q, want Read-only 1h", created.Name)
	}

	if created.AutoApprove {
		t.Error("AutoApprove should default to false")
	}
}

func TestCreateGrantDefinition_AutoApprove(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "autoapprove")

	def := &GrantDefinition{
		Name:            "Auto-approved read-only",
		Slug:            "auto-approved-read-only",
		DurationSeconds: 3600,
		Controls:        []string{ControlReadOnly},
		AutoApprove:     true,
		CreatedBy:       admin.UID,
	}

	created, err := store.CreateGrantDefinition(ctx, def)
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	if !created.AutoApprove {
		t.Error("AutoApprove should be true")
	}

	fetched, err := store.GetGrantDefinition(ctx, created.UID)
	if err != nil {
		t.Fatalf("GetGrantDefinition() error = %v", err)
	}

	if !fetched.AutoApprove {
		t.Error("AutoApprove should round-trip through storage as true")
	}
}

func TestCreateGrantDefinition_DuplicateActiveName(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "dup")

	def := &GrantDefinition{
		Name:            "duplicate",
		Slug:            "duplicate",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	}

	if _, err := store.CreateGrantDefinition(ctx, def); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Second active with same name but a distinct slug, so only the name
	// constraint is in play — otherwise both constraints would be violated
	// and which sentinel comes back would depend on constraint-check order.
	dup := &GrantDefinition{
		Name:            "duplicate",
		Slug:            "duplicate-2",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	}

	// Second active with same name → unique-violation mapped to the typed
	// ErrGrantDefinitionDuplicate sentinel (surfaced as 409 DUPLICATE_NAME).
	if _, err := store.CreateGrantDefinition(ctx, dup); !errors.Is(err, ErrGrantDefinitionDuplicate) {
		t.Fatalf("expected ErrGrantDefinitionDuplicate on duplicate active name, got %v", err)
	}
}

func TestListGrantDefinitions_ActiveOnly(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "list")

	d1, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "active",
		Slug:            "active",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	d2, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "to-deactivate",
		Slug:            "to-deactivate",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DeactivateGrantDefinition(ctx, d2.UID); err != nil {
		t.Fatal(err)
	}

	all, err := store.ListGrantDefinitions(ctx, GrantDefinitionFilter{ActiveOnly: false})
	if err != nil {
		t.Fatal(err)
	}

	if len(all) < 2 {
		t.Errorf("len(all) = %d, want >= 2", len(all))
	}

	active, err := store.ListGrantDefinitions(ctx, GrantDefinitionFilter{ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range active {
		if d.UID == d2.UID {
			t.Error("deactivated definition leaked into active-only list")
		}
	}

	foundActive := false
	for _, d := range active {
		if d.UID == d1.UID {
			foundActive = true

			break
		}
	}
	if !foundActive {
		t.Error("active definition missing from active-only list")
	}
}

func TestUpdateGrantDefinition(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "update")

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "original",
		Slug:            "original",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	originalUID := def.UID

	def.Name = "renamed"
	def.Slug = "renamed"
	def.DurationSeconds = 120

	updated, err := store.UpdateGrantDefinition(ctx, def)
	if err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	// An edit versions the definition: a new row carrying the change, sharing
	// the lineage of the one it superseded.
	if updated.UID == originalUID {
		t.Fatal("UpdateGrantDefinition() mutated the row in place instead of versioning it")
	}

	if updated.LineageUID != def.LineageUID {
		t.Errorf("new version lineage = %s, want %s", updated.LineageUID, def.LineageUID)
	}

	if updated.Name != "renamed" || updated.Slug != "renamed" || updated.DurationSeconds != 120 {
		t.Errorf("got = %+v, want renamed/renamed/120", updated)
	}

	// The superseded row is still readable — grants issued from it have to be
	// able to render their shape — but it is archived and no longer live.
	previous, err := store.GetGrantDefinition(ctx, originalUID)
	if err != nil {
		t.Fatal(err)
	}

	if previous.ArchivedAt == nil {
		t.Error("the superseded version was not archived")
	}

	if previous.Name != "original" {
		t.Errorf("the superseded version changed: name = %q, want %q", previous.Name, "original")
	}

	// The slug now resolves to the live version only.
	live, err := store.GetGrantDefinitionBySlug(ctx, "renamed")
	if err != nil {
		t.Fatalf("GetGrantDefinitionBySlug() error = %v", err)
	}

	if live.UID != updated.UID {
		t.Errorf("slug resolved to %s, want the live version %s", live.UID, updated.UID)
	}

	// Editing a superseded version is refused rather than forking history.
	previous.Description = "late edit"

	if _, err := store.UpdateGrantDefinition(ctx, previous); !errors.Is(err, ErrGrantDefinitionArchived) {
		t.Fatalf("editing an archived version: err = %v, want ErrGrantDefinitionArchived", err)
	}
}

// A PATCH that changes nothing must not litter the version history.
func TestUpdateGrantDefinition_NoOpEditIsNotVersioned(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "noop_update")

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "stable",
		Slug:            "stable",
		DurationSeconds: 60,
		Controls:        []string{ControlReadOnly},
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.UpdateGrantDefinition(ctx, def)
	if err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	if updated.UID != def.UID {
		t.Errorf("a no-op edit created version %s, want the existing %s", updated.UID, def.UID)
	}

	if updated.ArchivedAt != nil {
		t.Error("a no-op edit archived the live version")
	}
}

func TestGetGrantDefinition_NotFound(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	_, err := store.GetGrantDefinition(ctx, uuid.New())
	if !errors.Is(err, ErrGrantDefinitionNotFound) {
		t.Errorf("err = %v, want ErrGrantDefinitionNotFound", err)
	}
}

func TestGetGrantDefinitionBySlug(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "slug_lookup")

	created, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "Read-only 1h",
		Slug:            "read-only-1h-lookup",
		DurationSeconds: 3600,
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	got, err := store.GetGrantDefinitionBySlug(ctx, "read-only-1h-lookup")
	if err != nil {
		t.Fatalf("GetGrantDefinitionBySlug() error = %v", err)
	}

	if got.UID != created.UID {
		t.Errorf("GetGrantDefinitionBySlug() UID = %v, want %v", got.UID, created.UID)
	}
}

func TestGetGrantDefinitionBySlug_NotFound(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	_, err := store.GetGrantDefinitionBySlug(ctx, "does-not-exist")
	if !errors.Is(err, ErrGrantDefinitionNotFound) {
		t.Errorf("err = %v, want ErrGrantDefinitionNotFound", err)
	}
}

func TestCreateGrantDefinition_DuplicateSlug(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "slug_dup")

	if _, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "first",
		Slug:            "shared-slug",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// A distinct name (so the active-name-uniq index doesn't also fire) but
	// the same slug → unique-violation mapped to ErrGrantDefinitionSlugDuplicate.
	_, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "second",
		Slug:            "shared-slug",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	})
	if !errors.Is(err, ErrGrantDefinitionSlugDuplicate) {
		t.Fatalf("expected ErrGrantDefinitionSlugDuplicate on duplicate slug, got %v", err)
	}
}

func TestUpdateGrantDefinition_DuplicateSlug(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "slug_update_dup")

	if _, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "taken",
		Slug:            "taken-slug",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	}); err != nil {
		t.Fatal(err)
	}

	other, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "other",
		Slug:            "other-slug",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	other.Slug = "taken-slug"

	if _, err := store.UpdateGrantDefinition(ctx, other); !errors.Is(err, ErrGrantDefinitionSlugDuplicate) {
		t.Fatalf("expected ErrGrantDefinitionSlugDuplicate, got %v", err)
	}
}

// TestGrantDefinition_SampleQueriesSurviveVersioning pins the resolved-open-
// question behavior from
// specs/todos/2026-08-06-06-sql-control-pattern-query-templates.md:
// sample_queries is just another column on grant_definitions, so it must
// round-trip through both CreateGrantDefinition and, critically, through the
// archive-and-reinsert versioning UpdateGrantDefinition performs on every
// edit — carried forward on a no-op-shape edit is not enough, since that path
// never versions at all.
func TestGrantDefinition_SampleQueriesSurviveVersioning(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "sample_queries")

	samples := []string{
		"DELETE FROM users WHERE id = 1",
		"SELECT * FROM accounts",
	}

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:             "with-samples",
		Slug:             "with-samples",
		DurationSeconds:  60,
		Controls:         []string{ControlReadOnly},
		ApprovalPatterns: []string{`(?i)^DELETE`},
		SampleQueries:    samples,
		CreatedBy:        admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	if !equalStringSlices(def.SampleQueries, samples) {
		t.Fatalf("SampleQueries on create = %v, want %v", def.SampleQueries, samples)
	}

	fetched, err := store.GetGrantDefinition(ctx, def.UID)
	if err != nil {
		t.Fatalf("GetGrantDefinition() error = %v", err)
	}

	if !equalStringSlices(fetched.SampleQueries, samples) {
		t.Fatalf("SampleQueries round-tripped through storage as %v, want %v", fetched.SampleQueries, samples)
	}

	// Editing an unrelated field forces a version bump — the archive +
	// reinsert path — and the samples must be carried onto the successor,
	// not dropped, since sample_queries versions along with the rest of the
	// definition's matching config.
	fetched.Description = "now with a description"

	updated, err := store.UpdateGrantDefinition(ctx, fetched)
	if err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	if updated.UID == def.UID {
		t.Fatal("expected the edit to version the definition (new uid), got the same row")
	}

	if !equalStringSlices(updated.SampleQueries, samples) {
		t.Fatalf("SampleQueries on the new version = %v, want %v (must survive versioning)", updated.SampleQueries, samples)
	}

	// The archived predecessor must keep describing exactly the samples it
	// was saved with — versioning must not retroactively rewrite history.
	archived, err := store.GetGrantDefinition(ctx, def.UID)
	if err != nil {
		t.Fatalf("GetGrantDefinition(archived) error = %v", err)
	}

	if !equalStringSlices(archived.SampleQueries, samples) {
		t.Fatalf("archived version SampleQueries = %v, want %v", archived.SampleQueries, samples)
	}

	// Clearing the samples on a further edit must itself version and stick —
	// an explicit empty slice is not "leave alone".
	updated.SampleQueries = []string{}
	updated.Description = "samples cleared"

	cleared, err := store.UpdateGrantDefinition(ctx, updated)
	if err != nil {
		t.Fatalf("UpdateGrantDefinition() (clearing) error = %v", err)
	}

	if len(cleared.SampleQueries) != 0 {
		t.Fatalf("SampleQueries after clearing = %v, want empty", cleared.SampleQueries)
	}
}

// TestGrantDefinition_PatternsStartingWithABracketSurviveStorage is the
// regression test for the text[] read-back bug: bun's pgdialect array parser
// treats an element starting with `(` or `[` as a range literal and splits it
// at the matching bracket, so `(?i)^DELETE` came back as two patterns — and
// `(?i)` on its own matches every statement, turning a targeted approval hold
// into a hold on everything the grant runs. See internal/store/array.go.
func TestGrantDefinition_PatternsStartingWithABracketSurviveStorage(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "bracket_patterns")

	patterns := []string{`(?i)^DELETE`, `[abc]`, `(a|b)`}
	samples := []string{`(SELECT 1)`, `[bracketed]`}

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:             "bracket-patterns",
		Slug:             "bracket-patterns",
		DurationSeconds:  60,
		Controls:         []string{ControlReadOnly},
		ApprovalPatterns: patterns,
		SampleQueries:    samples,
		CreatedBy:        admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	if !equalStringSlices(def.ApprovalPatterns, patterns) {
		t.Fatalf("ApprovalPatterns returned by the insert = %#v, want %#v", def.ApprovalPatterns, patterns)
	}

	fetched, err := store.GetGrantDefinition(ctx, def.UID)
	if err != nil {
		t.Fatalf("GetGrantDefinition() error = %v", err)
	}

	if len(fetched.ApprovalPatterns) != len(patterns) {
		t.Fatalf(
			"ApprovalPatterns read back with %d elements (%#v), want %d (%#v) — an element was split",
			len(fetched.ApprovalPatterns), fetched.ApprovalPatterns, len(patterns), patterns,
		)
	}

	if !equalStringSlices(fetched.ApprovalPatterns, patterns) {
		t.Fatalf("ApprovalPatterns read back as %#v, want %#v", fetched.ApprovalPatterns, patterns)
	}

	if !equalStringSlices(fetched.SampleQueries, samples) {
		t.Fatalf("SampleQueries read back as %#v, want %#v", fetched.SampleQueries, samples)
	}

	// A listing goes through a different query path than a by-uid fetch.
	defs, err := store.ListGrantDefinitions(ctx, GrantDefinitionFilter{})
	if err != nil {
		t.Fatalf("ListGrantDefinitions() error = %v", err)
	}

	var listed *GrantDefinition

	for i := range defs {
		if defs[i].UID == def.UID {
			listed = &defs[i]

			break
		}
	}

	if listed == nil {
		t.Fatal("the definition is missing from ListGrantDefinitions()")
	}

	if !equalStringSlices(listed.ApprovalPatterns, patterns) {
		t.Fatalf("ApprovalPatterns from the listing = %#v, want %#v", listed.ApprovalPatterns, patterns)
	}

	// An edit archives the row and reinserts a successor: the patterns must
	// survive that re-encode too, which is where a read-back bug turns a
	// display glitch into corrupted stored data.
	listed.Description = "edited"

	updated, err := store.UpdateGrantDefinition(ctx, listed)
	if err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	if !equalStringSlices(updated.ApprovalPatterns, patterns) {
		t.Fatalf("ApprovalPatterns after versioning = %#v, want %#v", updated.ApprovalPatterns, patterns)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
