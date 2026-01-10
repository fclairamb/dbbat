package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// createTestUserAndDatabase creates a user and database for grant testing.
func createTestUserAndDatabase(t *testing.T, ctx context.Context, store *Store, suffix string) (*User, *Database) {
	t.Helper()
	key := testEncryptionKey()

	user, err := store.CreateUser(ctx, "grantuser_"+suffix, "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	db := &Database{
		Name:         "grantdb_" + suffix,
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "pass",
		SSLMode:      "disable",
	}
	database, err := store.CreateDatabase(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateDatabase() error = %v", err)
	}

	return user, database
}

func TestCreateGrant(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "create")

	// Create admin user for granted_by
	admin, err := store.CreateUser(ctx, "grantadmin", "hash", []string{RoleAdmin, RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("create read grant", func(t *testing.T) {
		now := time.Now()
		grant := &Grant{
			UserID:      user.UID,
			DatabaseID:  database.UID,
			AccessLevel: "read",
			GrantedBy:   admin.UID,
			StartsAt:    now,
			ExpiresAt:   now.Add(24 * time.Hour),
		}

		created, err := store.CreateGrant(ctx, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		if created.UID == uuid.Nil {
			t.Error("CreateGrant() grant.UID = uuid.Nil")
		}
		if created.AccessLevel != "read" {
			t.Errorf("CreateGrant() grant.AccessLevel = %q, want %q", created.AccessLevel, "read")
		}
		if created.RevokedAt != nil {
			t.Error("CreateGrant() grant.RevokedAt should be nil")
		}
	})

	t.Run("create write grant with quotas", func(t *testing.T) {
		user2, db2 := createTestUserAndDatabase(t, ctx, store, "quotas")

		now := time.Now()
		maxQueryCounts := int64(100)
		maxBytesTransferred := int64(1024 * 1024)
		grant := &Grant{
			UserID:              user2.UID,
			DatabaseID:          db2.UID,
			AccessLevel:         "write",
			GrantedBy:           admin.UID,
			StartsAt:            now,
			ExpiresAt:           now.Add(7 * 24 * time.Hour),
			MaxQueryCounts:      &maxQueryCounts,
			MaxBytesTransferred: &maxBytesTransferred,
		}

		created, err := store.CreateGrant(ctx, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		if created.AccessLevel != "write" {
			t.Errorf("CreateGrant() grant.AccessLevel = %q, want %q", created.AccessLevel, "write")
		}
		if created.MaxQueryCounts == nil || *created.MaxQueryCounts != 100 {
			t.Errorf("CreateGrant() grant.MaxQueryCounts = %v, want %d", created.MaxQueryCounts, 100)
		}
		if created.MaxBytesTransferred == nil || *created.MaxBytesTransferred != 1024*1024 {
			t.Errorf("CreateGrant() grant.MaxBytesTransferred = %v, want %d", created.MaxBytesTransferred, 1024*1024)
		}
	})
}

func TestGetActiveGrant(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "active")
	admin, _ := store.CreateUser(ctx, "activeadmin", "hash", []string{RoleAdmin, RoleConnector})

	t.Run("active grant exists", func(t *testing.T) {
		now := time.Now()
		grant := &Grant{
			UserID:      user.UID,
			DatabaseID:  database.UID,
			AccessLevel: "read",
			GrantedBy:   admin.UID,
			StartsAt:    now.Add(-1 * time.Hour), // Started 1 hour ago
			ExpiresAt:   now.Add(1 * time.Hour),  // Expires in 1 hour
		}
		created, err := store.CreateGrant(ctx, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		found, err := store.GetActiveGrant(ctx, user.UID, database.UID)
		if err != nil {
			t.Fatalf("GetActiveGrant() error = %v", err)
		}

		if found.UID != created.UID {
			t.Errorf("GetActiveGrant() grant.ID = %d, want %d", found.UID, created.UID)
		}
	})

	t.Run("no active grant - expired", func(t *testing.T) {
		user2, db2 := createTestUserAndDatabase(t, ctx, store, "expired")

		now := time.Now()
		grant := &Grant{
			UserID:      user2.UID,
			DatabaseID:  db2.UID,
			AccessLevel: "read",
			GrantedBy:   admin.UID,
			StartsAt:    now.Add(-2 * time.Hour), // Started 2 hours ago
			ExpiresAt:   now.Add(-1 * time.Hour), // Expired 1 hour ago
		}
		_, err := store.CreateGrant(ctx, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		_, err = store.GetActiveGrant(ctx, user2.UID, db2.UID)
		if !errors.Is(err, ErrNoActiveGrant) {
			t.Errorf("GetActiveGrant() error = %v, want %v", err, ErrNoActiveGrant)
		}
	})

	t.Run("no active grant - not started", func(t *testing.T) {
		user3, db3 := createTestUserAndDatabase(t, ctx, store, "future")

		now := time.Now()
		grant := &Grant{
			UserID:      user3.UID,
			DatabaseID:  db3.UID,
			AccessLevel: "read",
			GrantedBy:   admin.UID,
			StartsAt:    now.Add(1 * time.Hour), // Starts in 1 hour
			ExpiresAt:   now.Add(2 * time.Hour), // Expires in 2 hours
		}
		_, err := store.CreateGrant(ctx, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		_, err = store.GetActiveGrant(ctx, user3.UID, db3.UID)
		if !errors.Is(err, ErrNoActiveGrant) {
			t.Errorf("GetActiveGrant() error = %v, want %v", err, ErrNoActiveGrant)
		}
	})

	t.Run("no grant exists", func(t *testing.T) {
		_, err := store.GetActiveGrant(ctx, uuid.New(), uuid.New())
		if !errors.Is(err, ErrNoActiveGrant) {
			t.Errorf("GetActiveGrant() error = %v, want %v", err, ErrNoActiveGrant)
		}
	})
}

func TestGetGrantByUID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "byid")
	admin, _ := store.CreateUser(ctx, "byidadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant := &Grant{
		UserID:      user.UID,
		DatabaseID:  database.UID,
		AccessLevel: "write",
		GrantedBy:   admin.UID,
		StartsAt:    now,
		ExpiresAt:   now.Add(time.Hour),
	}
	created, err := store.CreateGrant(ctx, grant)
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	t.Run("existing grant", func(t *testing.T) {
		found, err := store.GetGrantByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetGrantByUID() error = %v", err)
		}

		if found.AccessLevel != "write" {
			t.Errorf("GetGrantByUID() grant.AccessLevel = %q, want %q", found.AccessLevel, "write")
		}
	})

	t.Run("non-existing grant", func(t *testing.T) {
		_, err := store.GetGrantByUID(ctx, uuid.New())
		if !errors.Is(err, ErrGrantNotFound) {
			t.Errorf("GetGrantByUID() error = %v, want %v", err, ErrGrantNotFound)
		}
	})
}

func TestListGrants(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user1, db1 := createTestUserAndDatabase(t, ctx, store, "list1")
	user2, db2 := createTestUserAndDatabase(t, ctx, store, "list2")
	admin, _ := store.CreateUser(ctx, "listadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()

	// Create grants
	// Use a time in the past for StartsAt to avoid race conditions with database NOW()
	grants := []*Grant{
		{UserID: user1.UID, DatabaseID: db1.UID, AccessLevel: "read", GrantedBy: admin.UID, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{UserID: user1.UID, DatabaseID: db2.UID, AccessLevel: "write", GrantedBy: admin.UID, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{UserID: user2.UID, DatabaseID: db1.UID, AccessLevel: "read", GrantedBy: admin.UID, StartsAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}, // Expired
	}

	for _, g := range grants {
		_, err := store.CreateGrant(ctx, g)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}
	}

	t.Run("list all", func(t *testing.T) {
		result, err := store.ListGrants(ctx, GrantFilter{})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 3 {
			t.Errorf("ListGrants() len = %d, want 3", len(result))
		}
	})

	t.Run("filter by user", func(t *testing.T) {
		result, err := store.ListGrants(ctx, GrantFilter{UserID: &user1.UID})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListGrants() len = %d, want 2", len(result))
		}
	})

	t.Run("filter by database", func(t *testing.T) {
		result, err := store.ListGrants(ctx, GrantFilter{DatabaseID: &db1.UID})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListGrants() len = %d, want 2", len(result))
		}
	})

	t.Run("active only", func(t *testing.T) {
		result, err := store.ListGrants(ctx, GrantFilter{ActiveOnly: true})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListGrants() len = %d, want 2", len(result))
		}
	})

	t.Run("combined filters", func(t *testing.T) {
		result, err := store.ListGrants(ctx, GrantFilter{UserID: &user1.UID, DatabaseID: &db1.UID})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 1 {
			t.Errorf("ListGrants() len = %d, want 1", len(result))
		}
	})
}

func TestRevokeGrant(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "revoke")
	admin, _ := store.CreateUser(ctx, "revokeadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant := &Grant{
		UserID:      user.UID,
		DatabaseID:  database.UID,
		AccessLevel: "read",
		GrantedBy:   admin.UID,
		StartsAt:    now.Add(-time.Hour),
		ExpiresAt:   now.Add(time.Hour),
	}
	created, err := store.CreateGrant(ctx, grant)
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	t.Run("revoke active grant", func(t *testing.T) {
		err := store.RevokeGrant(ctx, created.UID, admin.UID)
		if err != nil {
			t.Fatalf("RevokeGrant() error = %v", err)
		}

		// Verify grant is revoked
		found, err := store.GetGrantByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetGrantByUID() error = %v", err)
		}
		if found.RevokedAt == nil {
			t.Error("grant.RevokedAt should not be nil after revoke")
		}
		if found.RevokedBy == nil || *found.RevokedBy != admin.UID {
			t.Errorf("grant.RevokedBy = %v, want %s", found.RevokedBy, admin.UID)
		}

		// Should no longer appear as active
		_, err = store.GetActiveGrant(ctx, user.UID, database.UID)
		if !errors.Is(err, ErrNoActiveGrant) {
			t.Errorf("GetActiveGrant() error = %v, want %v", err, ErrNoActiveGrant)
		}
	})

	t.Run("revoke already revoked grant", func(t *testing.T) {
		err := store.RevokeGrant(ctx, created.UID, admin.UID)
		if !errors.Is(err, ErrGrantAlreadyRevoked) {
			t.Errorf("RevokeGrant() error = %v, want %v", err, ErrGrantAlreadyRevoked)
		}
	})

	t.Run("revoke non-existing grant", func(t *testing.T) {
		err := store.RevokeGrant(ctx, uuid.New(), admin.UID)
		if !errors.Is(err, ErrGrantAlreadyRevoked) {
			t.Errorf("RevokeGrant() error = %v, want %v", err, ErrGrantAlreadyRevoked)
		}
	})
}

func TestIncrementQueryCount(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// This is a placeholder function, just verify it doesn't error
	err := store.IncrementQueryCount(ctx, uuid.New())
	if err != nil {
		t.Errorf("IncrementQueryCount() error = %v", err)
	}
}

func TestIncrementBytesTransferred(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// This is a placeholder function, just verify it doesn't error
	err := store.IncrementBytesTransferred(ctx, uuid.New(), 1024)
	if err != nil {
		t.Errorf("IncrementBytesTransferred() error = %v", err)
	}
}
