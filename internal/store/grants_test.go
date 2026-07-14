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
			UserID:     user.UID,
			DatabaseID: database.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now,
			ExpiresAt:  now.Add(24 * time.Hour),
		}

		created, err := store.CreateGrant(ctx, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		if created.UID == uuid.Nil {
			t.Error("CreateGrant() grant.UID = uuid.Nil")
		}
		if !created.IsReadOnly() {
			t.Error("CreateGrant() grant should have read_only control")
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
			Controls:            []string{}, // Empty = full write access
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

		if len(created.Controls) != 0 {
			t.Errorf("CreateGrant() grant.Controls should be empty for full write access, got %v", created.Controls)
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
			UserID:     user.UID,
			DatabaseID: database.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(-1 * time.Hour), // Started 1 hour ago
			ExpiresAt:  now.Add(1 * time.Hour),  // Expires in 1 hour
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
			UserID:     user2.UID,
			DatabaseID: db2.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(-2 * time.Hour), // Started 2 hours ago
			ExpiresAt:  now.Add(-1 * time.Hour), // Expired 1 hour ago
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
			UserID:     user3.UID,
			DatabaseID: db3.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(1 * time.Hour), // Starts in 1 hour
			ExpiresAt:  now.Add(2 * time.Hour), // Expires in 2 hours
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
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now,
		ExpiresAt:  now.Add(time.Hour),
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

		if len(found.Controls) != 0 {
			t.Errorf("GetGrantByUID() grant.Controls should be empty for full write access, got %v", found.Controls)
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
		{UserID: user1.UID, DatabaseID: db1.UID, Controls: []string{ControlReadOnly}, GrantedBy: admin.UID, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{UserID: user1.UID, DatabaseID: db2.UID, Controls: []string{}, GrantedBy: admin.UID, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{UserID: user2.UID, DatabaseID: db1.UID, Controls: []string{ControlReadOnly}, GrantedBy: admin.UID, StartsAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}, // Expired
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
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{ControlReadOnly},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
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

func TestGrantCounters_PopulatedFromQueriesAndConnections(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "counters")
	admin, _ := store.CreateUser(ctx, "countersadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant, err := store.CreateGrant(ctx, &Grant{
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	const queryBytes = int64(100)
	for i := 0; i < 3; i++ {
		if _, err := store.CreateQuery(ctx, &Query{
			ConnectionID: conn.UID,
			SQLText:      "SELECT 1",
			ExecutedAt:   now,
		}); err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}
		if err := store.IncrementConnectionStats(ctx, conn.UID, queryBytes); err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}
	}

	got, err := store.GetGrantByUID(ctx, grant.UID)
	if err != nil {
		t.Fatalf("GetGrantByUID() error = %v", err)
	}
	if got.QueryCount != 3 {
		t.Errorf("QueryCount = %d, want 3", got.QueryCount)
	}
	if got.BytesTransferred != 3*queryBytes {
		t.Errorf("BytesTransferred = %d, want %d", got.BytesTransferred, 3*queryBytes)
	}

	active, err := store.GetActiveGrant(ctx, user.UID, database.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant() error = %v", err)
	}
	if active.QueryCount != 3 {
		t.Errorf("active QueryCount = %d, want 3", active.QueryCount)
	}
	if active.BytesTransferred != 3*queryBytes {
		t.Errorf("active BytesTransferred = %d, want %d", active.BytesTransferred, 3*queryBytes)
	}
}

func TestGrantCounters_TimeWindowExcludesOutOfRange(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "window")
	admin, _ := store.CreateUser(ctx, "windowadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	// Grant window is entirely in the future relative to the activity below.
	grant, err := store.CreateGrant(ctx, &Grant{
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(time.Hour),
		ExpiresAt:  now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}
	if _, err := store.CreateQuery(ctx, &Query{
		ConnectionID: conn.UID,
		SQLText:      "SELECT 1",
		ExecutedAt:   now,
	}); err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}
	if err := store.IncrementConnectionStats(ctx, conn.UID, 500); err != nil {
		t.Fatalf("IncrementConnectionStats() error = %v", err)
	}

	got, err := store.GetGrantByUID(ctx, grant.UID)
	if err != nil {
		t.Fatalf("GetGrantByUID() error = %v", err)
	}
	if got.QueryCount != 0 {
		t.Errorf("QueryCount = %d, want 0 (activity is before grant.StartsAt)", got.QueryCount)
	}
	if got.BytesTransferred != 0 {
		t.Errorf("BytesTransferred = %d, want 0 (activity is before grant.StartsAt)", got.BytesTransferred)
	}
}

func TestGrantCounters_BoundedByRevokedAt(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "revokebound")
	admin, _ := store.CreateUser(ctx, "revokeboundadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant, err := store.CreateGrant(ctx, &Grant{
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	// Pre-revocation activity (should be counted).
	preConn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.3")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}
	if _, err := store.CreateQuery(ctx, &Query{
		ConnectionID: preConn.UID,
		SQLText:      "SELECT pre",
		ExecutedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}
	if err := store.IncrementConnectionStats(ctx, preConn.UID, 100); err != nil {
		t.Fatalf("IncrementConnectionStats() error = %v", err)
	}

	// Ensure RevokedAt > pre-activity timestamps and < post-activity timestamps.
	time.Sleep(20 * time.Millisecond)
	if err := store.RevokeGrant(ctx, grant.UID, admin.UID); err != nil {
		t.Fatalf("RevokeGrant() error = %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	// Post-revocation activity (should NOT be counted).
	postConn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.4")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}
	if _, err := store.CreateQuery(ctx, &Query{
		ConnectionID: postConn.UID,
		SQLText:      "SELECT post",
		ExecutedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}
	if err := store.IncrementConnectionStats(ctx, postConn.UID, 999); err != nil {
		t.Fatalf("IncrementConnectionStats() error = %v", err)
	}

	got, err := store.GetGrantByUID(ctx, grant.UID)
	if err != nil {
		t.Fatalf("GetGrantByUID() error = %v", err)
	}
	if got.QueryCount != 1 {
		t.Errorf("QueryCount = %d, want 1 (only pre-revocation query counts)", got.QueryCount)
	}
	if got.BytesTransferred != 100 {
		t.Errorf("BytesTransferred = %d, want 100 (only pre-revocation connection counts)", got.BytesTransferred)
	}
}

// TestGrantBytesRecompute_IncludesAbortedQueryBytes covers the core scenario of
// spec 2026-07-14-09: bytes from a query aborted mid-stream by a grant limit are
// flushed to the connection via IncrementConnectionBytes (no query log row / no
// query-count bump). The grant's recomputed BytesTransferred — the value a fresh
// reconnect enforces against — must include them, otherwise the cumulative cap
// could be bypassed across short-lived reconnects.
func TestGrantBytesRecompute_IncludesAbortedQueryBytes(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "abortedbytes")
	admin, _ := store.CreateUser(ctx, "abortedbytesadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant, err := store.CreateGrant(ctx, &Grant{
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.9")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	// One completed query (row + stats), then a mid-stream-aborted query whose
	// streamed bytes are flushed bytes-only (no query row, no query-count bump).
	if _, err := store.CreateQuery(ctx, &Query{
		ConnectionID: conn.UID,
		SQLText:      "SELECT ok",
		ExecutedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}
	if err := store.IncrementConnectionStats(ctx, conn.UID, 300); err != nil {
		t.Fatalf("IncrementConnectionStats() error = %v", err)
	}
	if err := store.IncrementConnectionBytes(ctx, conn.UID, 700); err != nil {
		t.Fatalf("IncrementConnectionBytes() error = %v", err)
	}

	got, err := store.GetGrantByUID(ctx, grant.UID)
	if err != nil {
		t.Fatalf("GetGrantByUID() error = %v", err)
	}
	// Query count reflects only logged queries (1); bytes include the aborted
	// query's flushed bytes (300 + 700).
	if got.QueryCount != 1 {
		t.Errorf("QueryCount = %d, want 1", got.QueryCount)
	}
	if got.BytesTransferred != 1000 {
		t.Errorf("BytesTransferred = %d, want 1000 (must include the aborted query's flushed bytes)", got.BytesTransferred)
	}
}

func TestListGrants_PopulatesCountersForEach(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, db1 := createTestUserAndDatabase(t, ctx, store, "listcountersA")
	_, db2 := createTestUserAndDatabase(t, ctx, store, "listcountersB")
	admin, _ := store.CreateUser(ctx, "listcountersadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	g1, err := store.CreateGrant(ctx, &Grant{
		UserID:     user.UID,
		DatabaseID: db1.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}
	g2, err := store.CreateGrant(ctx, &Grant{
		UserID:     user.UID,
		DatabaseID: db2.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	// 1 query / 100 bytes on g1; 2 queries / 500 bytes on g2.
	conn1, _ := store.CreateConnection(ctx, user.UID, db1.UID, "10.0.0.5")
	if _, err := store.CreateQuery(ctx, &Query{ConnectionID: conn1.UID, SQLText: "x", ExecutedAt: now}); err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}
	if err := store.IncrementConnectionStats(ctx, conn1.UID, 100); err != nil {
		t.Fatalf("IncrementConnectionStats() error = %v", err)
	}

	conn2, _ := store.CreateConnection(ctx, user.UID, db2.UID, "10.0.0.6")
	for i := 0; i < 2; i++ {
		if _, err := store.CreateQuery(ctx, &Query{ConnectionID: conn2.UID, SQLText: "y", ExecutedAt: now}); err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}
		if err := store.IncrementConnectionStats(ctx, conn2.UID, 250); err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}
	}

	listed, err := store.ListGrants(ctx, GrantFilter{UserID: &user.UID})
	if err != nil {
		t.Fatalf("ListGrants() error = %v", err)
	}

	byUID := map[uuid.UUID]Grant{}
	for _, g := range listed {
		byUID[g.UID] = g
	}
	got1, ok1 := byUID[g1.UID]
	got2, ok2 := byUID[g2.UID]
	if !ok1 || !ok2 {
		t.Fatalf("ListGrants() missing one of the grants: ok1=%v ok2=%v", ok1, ok2)
	}
	if got1.QueryCount != 1 || got1.BytesTransferred != 100 {
		t.Errorf("g1 QueryCount=%d BytesTransferred=%d, want 1 / 100", got1.QueryCount, got1.BytesTransferred)
	}
	if got2.QueryCount != 2 || got2.BytesTransferred != 500 {
		t.Errorf("g2 QueryCount=%d BytesTransferred=%d, want 2 / 500", got2.QueryCount, got2.BytesTransferred)
	}
}
