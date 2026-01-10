package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreateUser(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	t.Run("create connector user", func(t *testing.T) {
		user, err := store.CreateUser(ctx, "testuser", "hashedpassword", []string{RoleConnector})
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		if user.UID == uuid.Nil {
			t.Error("CreateUser() user.UID = uuid.Nil")
		}
		if user.Username != "testuser" {
			t.Errorf("CreateUser() user.Username = %q, want %q", user.Username, "testuser")
		}
		if user.PasswordHash != "hashedpassword" {
			t.Errorf("CreateUser() user.PasswordHash = %q, want %q", user.PasswordHash, "hashedpassword")
		}
		if !user.HasRole(RoleConnector) {
			t.Error("CreateUser() user should have connector role")
		}
		if user.HasRole(RoleAdmin) {
			t.Error("CreateUser() user should not have admin role")
		}
		if user.CreatedAt.IsZero() {
			t.Error("CreateUser() user.CreatedAt is zero")
		}
		if user.UpdatedAt.IsZero() {
			t.Error("CreateUser() user.UpdatedAt is zero")
		}
	})

	t.Run("create admin user", func(t *testing.T) {
		user, err := store.CreateUser(ctx, "adminuser", "hashedpassword", []string{RoleAdmin, RoleConnector})
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		if !user.IsAdmin() {
			t.Error("CreateUser() user.IsAdmin() = false, want true")
		}
		if !user.IsConnector() {
			t.Error("CreateUser() user.IsConnector() = false, want true")
		}
	})

	t.Run("create viewer user", func(t *testing.T) {
		user, err := store.CreateUser(ctx, "vieweruser", "hashedpassword", []string{RoleViewer})
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		if !user.IsViewer() {
			t.Error("CreateUser() user.IsViewer() = false, want true")
		}
		if user.IsAdmin() {
			t.Error("CreateUser() user.IsAdmin() = true, want false")
		}
	})

	t.Run("create user with default role", func(t *testing.T) {
		user, err := store.CreateUser(ctx, "defaultuser", "hashedpassword", nil)
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		if !user.IsConnector() {
			t.Error("CreateUser() user should have default connector role")
		}
	})

	t.Run("duplicate username", func(t *testing.T) {
		_, err := store.CreateUser(ctx, "duplicate", "hash1", []string{RoleConnector})
		if err != nil {
			t.Fatalf("CreateUser() first call error = %v", err)
		}

		_, err = store.CreateUser(ctx, "duplicate", "hash2", []string{RoleConnector})
		if err == nil {
			t.Error("CreateUser() expected error for duplicate username")
		}
	})
}

func TestGetUserByUsername(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create a test user with admin role
	created, err := store.CreateUser(ctx, "findme", "myhash", []string{RoleAdmin, RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("existing user", func(t *testing.T) {
		user, err := store.GetUserByUsername(ctx, "findme")
		if err != nil {
			t.Fatalf("GetUserByUsername() error = %v", err)
		}

		if user.UID != created.UID {
			t.Errorf("GetUserByUsername() user.UID = %s, want %s", user.UID, created.UID)
		}
		if user.Username != "findme" {
			t.Errorf("GetUserByUsername() user.Username = %q, want %q", user.Username, "findme")
		}
		if user.PasswordHash != "myhash" {
			t.Errorf("GetUserByUsername() user.PasswordHash = %q, want %q", user.PasswordHash, "myhash")
		}
		if !user.IsAdmin() {
			t.Error("GetUserByUsername() user.IsAdmin() = false, want true")
		}
	})

	t.Run("non-existing user", func(t *testing.T) {
		_, err := store.GetUserByUsername(ctx, "nonexistent")
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("GetUserByUsername() error = %v, want %v", err, ErrUserNotFound)
		}
	})
}

func TestGetUserByUID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create a test user
	created, err := store.CreateUser(ctx, "byid", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("existing user", func(t *testing.T) {
		user, err := store.GetUserByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetUserByUID() error = %v", err)
		}

		if user.Username != "byid" {
			t.Errorf("GetUserByUID() user.Username = %q, want %q", user.Username, "byid")
		}
	})

	t.Run("non-existing user", func(t *testing.T) {
		_, err := store.GetUserByUID(ctx, uuid.New())
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("GetUserByUID() error = %v, want %v", err, ErrUserNotFound)
		}
	})
}

func TestListUsers(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	t.Run("empty list", func(t *testing.T) {
		users, err := store.ListUsers(ctx)
		if err != nil {
			t.Fatalf("ListUsers() error = %v", err)
		}
		if len(users) != 0 {
			t.Errorf("ListUsers() len = %d, want 0", len(users))
		}
	})

	// Create some users
	_, err := store.CreateUser(ctx, "alice", "hash1", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	_, err = store.CreateUser(ctx, "bob", "hash2", []string{RoleAdmin, RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("with users", func(t *testing.T) {
		users, err := store.ListUsers(ctx)
		if err != nil {
			t.Fatalf("ListUsers() error = %v", err)
		}
		if len(users) != 2 {
			t.Errorf("ListUsers() len = %d, want 2", len(users))
		}

		// Should be ordered by username
		if users[0].Username != "alice" {
			t.Errorf("ListUsers()[0].Username = %q, want %q", users[0].Username, "alice")
		}
		if users[1].Username != "bob" {
			t.Errorf("ListUsers()[1].Username = %q, want %q", users[1].Username, "bob")
		}
	})
}

func TestUpdateUser(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create a test user
	created, err := store.CreateUser(ctx, "toupdate", "oldhash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("update password", func(t *testing.T) {
		newHash := "newhash"
		err := store.UpdateUser(ctx, created.UID, UserUpdate{PasswordHash: &newHash})
		if err != nil {
			t.Fatalf("UpdateUser() error = %v", err)
		}

		user, err := store.GetUserByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetUserByUID() error = %v", err)
		}
		if user.PasswordHash != "newhash" {
			t.Errorf("user.PasswordHash = %q, want %q", user.PasswordHash, "newhash")
		}
	})

	t.Run("update roles", func(t *testing.T) {
		roles := []string{RoleAdmin, RoleViewer, RoleConnector}
		err := store.UpdateUser(ctx, created.UID, UserUpdate{Roles: roles})
		if err != nil {
			t.Fatalf("UpdateUser() error = %v", err)
		}

		user, err := store.GetUserByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetUserByUID() error = %v", err)
		}
		if !user.IsAdmin() {
			t.Error("user.IsAdmin() = false, want true")
		}
		if !user.IsViewer() {
			t.Error("user.IsViewer() = false, want true")
		}
		if !user.IsConnector() {
			t.Error("user.IsConnector() = false, want true")
		}
	})

	t.Run("update both fields", func(t *testing.T) {
		newHash := "finalhash"
		roles := []string{RoleConnector}
		err := store.UpdateUser(ctx, created.UID, UserUpdate{PasswordHash: &newHash, Roles: roles})
		if err != nil {
			t.Fatalf("UpdateUser() error = %v", err)
		}

		user, err := store.GetUserByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetUserByUID() error = %v", err)
		}
		if user.PasswordHash != "finalhash" {
			t.Errorf("user.PasswordHash = %q, want %q", user.PasswordHash, "finalhash")
		}
		if user.IsAdmin() {
			t.Error("user.IsAdmin() = true, want false")
		}
	})

	t.Run("non-existing user", func(t *testing.T) {
		newHash := "hash"
		err := store.UpdateUser(ctx, uuid.New(), UserUpdate{PasswordHash: &newHash})
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("UpdateUser() error = %v, want %v", err, ErrUserNotFound)
		}
	})
}

func TestDeleteUser(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create a test user
	created, err := store.CreateUser(ctx, "todelete", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("delete existing user", func(t *testing.T) {
		err := store.DeleteUser(ctx, created.UID)
		if err != nil {
			t.Fatalf("DeleteUser() error = %v", err)
		}

		// Verify user is deleted
		_, err = store.GetUserByUID(ctx, created.UID)
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("GetUserByUID() error = %v, want %v", err, ErrUserNotFound)
		}
	})

	t.Run("delete non-existing user", func(t *testing.T) {
		err := store.DeleteUser(ctx, uuid.New())
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("DeleteUser() error = %v, want %v", err, ErrUserNotFound)
		}
	})
}

func TestEnsureDefaultAdmin(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	t.Run("creates admin when no users exist", func(t *testing.T) {
		err := store.EnsureDefaultAdmin(ctx, "adminhash")
		if err != nil {
			t.Fatalf("EnsureDefaultAdmin() error = %v", err)
		}

		user, err := store.GetUserByUsername(ctx, "admin")
		if err != nil {
			t.Fatalf("GetUserByUsername() error = %v", err)
		}
		if !user.IsAdmin() {
			t.Error("user.IsAdmin() = false, want true")
		}
		if !user.IsConnector() {
			t.Error("user.IsConnector() = false, want true")
		}
		if user.PasswordHash != "adminhash" {
			t.Errorf("user.PasswordHash = %q, want %q", user.PasswordHash, "adminhash")
		}
	})

	t.Run("does not create when users exist", func(t *testing.T) {
		// Clean up and add a non-admin user
		_, err := store.db.ExecContext(ctx, "DELETE FROM users")
		if err != nil {
			t.Fatalf("cleanup error = %v", err)
		}

		_, err = store.CreateUser(ctx, "existing", "hash", []string{RoleConnector})
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		err = store.EnsureDefaultAdmin(ctx, "adminhash")
		if err != nil {
			t.Fatalf("EnsureDefaultAdmin() error = %v", err)
		}

		// Should not create admin user
		_, err = store.GetUserByUsername(ctx, "admin")
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("GetUserByUsername() error = %v, want %v", err, ErrUserNotFound)
		}
	})
}

func TestUserHasRole(t *testing.T) {
	t.Run("HasRole returns correct values", func(t *testing.T) {
		user := &User{
			Roles: []string{RoleAdmin, RoleConnector},
		}

		if !user.HasRole(RoleAdmin) {
			t.Error("HasRole(RoleAdmin) = false, want true")
		}
		if !user.HasRole(RoleConnector) {
			t.Error("HasRole(RoleConnector) = false, want true")
		}
		if user.HasRole(RoleViewer) {
			t.Error("HasRole(RoleViewer) = true, want false")
		}
	})

	t.Run("helper methods work correctly", func(t *testing.T) {
		user := &User{
			Roles: []string{RoleAdmin, RoleViewer},
		}

		if !user.IsAdmin() {
			t.Error("IsAdmin() = false, want true")
		}
		if !user.IsViewer() {
			t.Error("IsViewer() = false, want true")
		}
		if user.IsConnector() {
			t.Error("IsConnector() = true, want false")
		}
	})
}
