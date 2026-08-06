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
