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
	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "create")

	maxQ := int64(1000)

	def := &GrantDefinition{
		Name:                "Read-only 1h",
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
	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "autoapprove")

	def := &GrantDefinition{
		Name:            "Auto-approved read-only",
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
	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "dup")

	def := &GrantDefinition{
		Name:            "duplicate",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	}

	if _, err := store.CreateGrantDefinition(ctx, def); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Second active with same name → unique-violation mapped to the typed
	// ErrGrantDefinitionDuplicate sentinel (surfaced as 409 DUPLICATE_NAME).
	if _, err := store.CreateGrantDefinition(ctx, def); !errors.Is(err, ErrGrantDefinitionDuplicate) {
		t.Fatalf("expected ErrGrantDefinitionDuplicate on duplicate active name, got %v", err)
	}
}

func TestListGrantDefinitions_ActiveOnly(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "list")

	d1, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "active",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	d2, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "to-deactivate",
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
	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "update")

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "original",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	def.Name = "renamed"
	def.DurationSeconds = 120

	if err := store.UpdateGrantDefinition(ctx, def); err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	got, err := store.GetGrantDefinition(ctx, def.UID)
	if err != nil {
		t.Fatal(err)
	}

	if got.Name != "renamed" || got.DurationSeconds != 120 {
		t.Errorf("got = %+v, want renamed/120", got)
	}
}

func TestGetGrantDefinition_NotFound(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	_, err := store.GetGrantDefinition(ctx, uuid.New())
	if !errors.Is(err, ErrGrantDefinitionNotFound) {
		t.Errorf("err = %v, want ErrGrantDefinitionNotFound", err)
	}
}
