package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// createTestUserAndDatabase creates a user and database for grant testing.
func createTestUserAndDatabase(t *testing.T, ctx context.Context, store *Store, suffix string) (*User, *Server) {
	t.Helper()
	key := testEncryptionKey()

	user, err := store.CreateUser(ctx, "grantuser_"+suffix, "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	db := &Server{
		Name:         "grantdb_" + suffix,
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "pass",
		SSLMode:      "disable",
	}
	database, err := store.CreateServer(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateServer() error = %v", err)
	}

	return user, database
}

// newTestGrantDefinition persists a grant definition carrying the given shape.
// Grants have no shape of their own any more, so every test that used to write
// controls or quotas onto a grant literal now goes through a definition — this
// keeps that one line long.
//
// Only the shape fields of `shape` are read; name and slug are generated so
// callers never have to invent unique ones, and a missing duration defaults to
// an hour.
func newTestGrantDefinition(t *testing.T, ctx context.Context, s *Store, createdBy uuid.UUID, shape GrantDefinition) *GrantDefinition {
	t.Helper()

	suffix := uuid.NewString()[:8]

	if shape.Name == "" {
		shape.Name = "test-def-" + suffix
	}

	if shape.Slug == "" {
		shape.Slug = "test-def-" + suffix
	}

	if shape.DurationSeconds == 0 {
		shape.DurationSeconds = 3600
	}

	shape.CreatedBy = createdBy

	def, err := s.CreateGrantDefinition(ctx, &shape)
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	return def
}

// newTestGrant materializes a definition into a grant with an explicit window,
// which is what most tests need (BuildGrantFromDefinition derives the window
// from the definition's duration).
func newTestGrant(
	t *testing.T,
	ctx context.Context,
	s *Store,
	def *GrantDefinition,
	userID, databaseID, grantedBy uuid.UUID,
	startsAt, expiresAt time.Time,
) *Grant {
	t.Helper()

	grant := BuildGrantFromDefinition(def, userID, databaseID, grantedBy, startsAt)
	grant.ExpiresAt = expiresAt

	created, err := s.CreateGrant(ctx, grant)
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	return created
}

// testGrantSpec is the shape-plus-window description a test used to write
// straight onto a Grant literal. Since a grant no longer holds any shape, the
// spec is split by createGrantWithShape into a definition (the shape) and a
// grant (the instance).
type testGrantSpec struct {
	UserID              uuid.UUID
	DatabaseID          uuid.UUID
	GrantedBy           uuid.UUID
	Controls            []string
	MaxQueryCounts      *int64
	MaxBytesTransferred *int64
	ApprovalPatterns    []string
	ApproverGroupUIDs   []uuid.UUID
	StartsAt            time.Time
	ExpiresAt           time.Time
	Priority            int16
}

// createGrantWithShape persists a definition carrying spec's shape and a grant
// instantiating it over spec's window.
func createGrantWithShape(t *testing.T, ctx context.Context, s *Store, spec testGrantSpec) (*Grant, error) {
	t.Helper()

	def := newTestGrantDefinition(t, ctx, s, spec.GrantedBy, GrantDefinition{
		Controls:            spec.Controls,
		MaxQueryCounts:      spec.MaxQueryCounts,
		MaxBytesTransferred: spec.MaxBytesTransferred,
		ApprovalPatterns:    spec.ApprovalPatterns,
		ApproverGroupUIDs:   spec.ApproverGroupUIDs,
	})

	grant := BuildGrantFromDefinition(def, spec.UserID, spec.DatabaseID, spec.GrantedBy, spec.StartsAt)
	grant.ExpiresAt = spec.ExpiresAt

	if spec.Priority != 0 {
		grant.Priority = spec.Priority
	}

	return s.CreateGrant(ctx, grant)
}

func TestCreateGrant(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "create")

	// Create admin user for granted_by
	admin, err := store.CreateUser(ctx, "grantadmin", "hash", []string{RoleAdmin, RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("create read grant", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		grant := testGrantSpec{
			UserID:     user.UID,
			DatabaseID: database.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now,
			ExpiresAt:  now.Add(24 * time.Hour),
		}

		created, err := createGrantWithShape(t, ctx, store, grant)
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
		t.Parallel()

		user2, db2 := createTestUserAndDatabase(t, ctx, store, "quotas")

		now := time.Now()
		maxQueryCounts := int64(100)
		maxBytesTransferred := int64(1024 * 1024)
		grant := testGrantSpec{
			UserID:              user2.UID,
			DatabaseID:          db2.UID,
			Controls:            []string{}, // Empty = full write access
			GrantedBy:           admin.UID,
			StartsAt:            now,
			ExpiresAt:           now.Add(7 * 24 * time.Hour),
			MaxQueryCounts:      &maxQueryCounts,
			MaxBytesTransferred: &maxBytesTransferred,
		}

		created, err := createGrantWithShape(t, ctx, store, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		if len(created.Controls()) != 0 {
			t.Errorf("CreateGrant() grant.Controls should be empty for full write access, got %v", created.Controls())
		}
		if created.MaxQueryCounts() == nil || *created.MaxQueryCounts() != 100 {
			t.Errorf("CreateGrant() grant.MaxQueryCounts = %v, want %d", created.MaxQueryCounts(), 100)
		}
		if created.MaxBytesTransferred() == nil || *created.MaxBytesTransferred() != 1024*1024 {
			t.Errorf("CreateGrant() grant.MaxBytesTransferred = %v, want %d", created.MaxBytesTransferred(), 1024*1024)
		}
	})
}

func TestGetActiveGrant(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "active")
	admin, _ := store.CreateUser(ctx, "activeadmin", "hash", []string{RoleAdmin, RoleConnector})

	t.Run("active grant exists", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		grant := testGrantSpec{
			UserID:     user.UID,
			DatabaseID: database.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(-1 * time.Hour), // Started 1 hour ago
			ExpiresAt:  now.Add(1 * time.Hour),  // Expires in 1 hour
		}
		created, err := createGrantWithShape(t, ctx, store, grant)
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
		t.Parallel()

		user2, db2 := createTestUserAndDatabase(t, ctx, store, "expired")

		now := time.Now()
		grant := testGrantSpec{
			UserID:     user2.UID,
			DatabaseID: db2.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(-2 * time.Hour), // Started 2 hours ago
			ExpiresAt:  now.Add(-1 * time.Hour), // Expired 1 hour ago
		}
		_, err := createGrantWithShape(t, ctx, store, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		_, err = store.GetActiveGrant(ctx, user2.UID, db2.UID)
		if !errors.Is(err, ErrNoActiveGrant) {
			t.Errorf("GetActiveGrant() error = %v, want %v", err, ErrNoActiveGrant)
		}
	})

	t.Run("no active grant - not started", func(t *testing.T) {
		t.Parallel()

		user3, db3 := createTestUserAndDatabase(t, ctx, store, "future")

		now := time.Now()
		grant := testGrantSpec{
			UserID:     user3.UID,
			DatabaseID: db3.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(1 * time.Hour), // Starts in 1 hour
			ExpiresAt:  now.Add(2 * time.Hour), // Expires in 2 hours
		}
		_, err := createGrantWithShape(t, ctx, store, grant)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		_, err = store.GetActiveGrant(ctx, user3.UID, db3.UID)
		if !errors.Is(err, ErrNoActiveGrant) {
			t.Errorf("GetActiveGrant() error = %v, want %v", err, ErrNoActiveGrant)
		}
	})

	t.Run("no grant exists", func(t *testing.T) {
		t.Parallel()

		_, err := store.GetActiveGrant(ctx, uuid.New(), uuid.New())
		if !errors.Is(err, ErrNoActiveGrant) {
			t.Errorf("GetActiveGrant() error = %v, want %v", err, ErrNoActiveGrant)
		}
	})
}

func TestGetGrantByUID(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "byid")
	admin, _ := store.CreateUser(ctx, "byidadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant := testGrantSpec{
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now,
		ExpiresAt:  now.Add(time.Hour),
	}
	created, err := createGrantWithShape(t, ctx, store, grant)
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	t.Run("existing grant", func(t *testing.T) {
		t.Parallel()

		found, err := store.GetGrantByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetGrantByUID() error = %v", err)
		}

		if len(found.Controls()) != 0 {
			t.Errorf("GetGrantByUID() grant.Controls should be empty for full write access, got %v", found.Controls())
		}
	})

	t.Run("non-existing grant", func(t *testing.T) {
		t.Parallel()

		_, err := store.GetGrantByUID(ctx, uuid.New())
		if !errors.Is(err, ErrGrantNotFound) {
			t.Errorf("GetGrantByUID() error = %v, want %v", err, ErrGrantNotFound)
		}
	})
}

func TestListGrants(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user1, db1 := createTestUserAndDatabase(t, ctx, store, "list1")
	user2, db2 := createTestUserAndDatabase(t, ctx, store, "list2")
	admin, _ := store.CreateUser(ctx, "listadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()

	// Create grants
	// Use a time in the past for StartsAt to avoid race conditions with database NOW()
	grants := []testGrantSpec{
		{UserID: user1.UID, DatabaseID: db1.UID, Controls: []string{ControlReadOnly}, GrantedBy: admin.UID, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{UserID: user1.UID, DatabaseID: db2.UID, Controls: []string{}, GrantedBy: admin.UID, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{UserID: user2.UID, DatabaseID: db1.UID, Controls: []string{ControlReadOnly}, GrantedBy: admin.UID, StartsAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}, // Expired
	}

	for _, g := range grants {
		_, err := createGrantWithShape(t, ctx, store, g)
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}
	}

	t.Run("list all", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListGrants(ctx, GrantFilter{})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 3 {
			t.Errorf("ListGrants() len = %d, want 3", len(result))
		}
	})

	t.Run("filter by user", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListGrants(ctx, GrantFilter{UserID: &user1.UID})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListGrants() len = %d, want 2", len(result))
		}
	})

	t.Run("filter by database", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListGrants(ctx, GrantFilter{DatabaseID: &db1.UID})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListGrants() len = %d, want 2", len(result))
		}
	})

	t.Run("active only", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListGrants(ctx, GrantFilter{ActiveOnly: true})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListGrants() len = %d, want 2", len(result))
		}
	})

	t.Run("combined filters", func(t *testing.T) {
		t.Parallel()

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
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	// One grant per subtest, on a user/database pair of its own: revoking is a
	// write, and "revoke already revoked" needs a grant it revoked itself rather
	// than one a sibling happened to leave behind.
	activeGrant := func(t *testing.T, suffix string) (*Grant, *User, *Server, uuid.UUID) {
		t.Helper()

		user, database := createTestUserAndDatabase(t, ctx, store, "revoke-"+suffix)

		admin, err := store.CreateUser(ctx, "revokeadmin-"+suffix, "hash", []string{RoleAdmin, RoleConnector})
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		now := time.Now()
		created, err := createGrantWithShape(t, ctx, store, testGrantSpec{
			UserID:     user.UID,
			DatabaseID: database.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(-time.Hour),
			ExpiresAt:  now.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		return created, user, database, admin.UID
	}

	t.Run("revoke active grant", func(t *testing.T) {
		t.Parallel()

		created, user, database, admin := activeGrant(t, "active")

		err := store.RevokeGrant(ctx, created.UID, admin)
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
		if found.RevokedBy == nil || *found.RevokedBy != admin {
			t.Errorf("grant.RevokedBy = %v, want %s", found.RevokedBy, admin)
		}

		// Should no longer appear as active
		_, err = store.GetActiveGrant(ctx, user.UID, database.UID)
		if !errors.Is(err, ErrNoActiveGrant) {
			t.Errorf("GetActiveGrant() error = %v, want %v", err, ErrNoActiveGrant)
		}
	})

	t.Run("revoke already revoked grant", func(t *testing.T) {
		t.Parallel()

		created, _, _, admin := activeGrant(t, "twice")

		if err := store.RevokeGrant(ctx, created.UID, admin); err != nil {
			t.Fatalf("RevokeGrant() error = %v", err)
		}

		err := store.RevokeGrant(ctx, created.UID, admin)
		if !errors.Is(err, ErrGrantAlreadyRevoked) {
			t.Errorf("RevokeGrant() error = %v, want %v", err, ErrGrantAlreadyRevoked)
		}
	})

	t.Run("revoke non-existing grant", func(t *testing.T) {
		t.Parallel()

		_, _, _, admin := activeGrant(t, "missing")

		err := store.RevokeGrant(ctx, uuid.New(), admin)
		if !errors.Is(err, ErrGrantAlreadyRevoked) {
			t.Errorf("RevokeGrant() error = %v, want %v", err, ErrGrantAlreadyRevoked)
		}
	})
}

func TestGrantCounters_PopulatedFromQueriesAndConnections(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "counters")
	admin, _ := store.CreateUser(ctx, "countersadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant, err := createGrantWithShape(t, ctx, store, testGrantSpec{
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
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "window")
	admin, _ := store.CreateUser(ctx, "windowadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	// Grant window is entirely in the future relative to the activity below.
	grant, err := createGrantWithShape(t, ctx, store, testGrantSpec{
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
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "revokebound")
	admin, _ := store.CreateUser(ctx, "revokeboundadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant, err := createGrantWithShape(t, ctx, store, testGrantSpec{
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

// TestGrantCounters_StampedConnectionExcludedFromOtherGrantsWindow is the
// quota-attribution half of spec 2026-08-06-03: two grants active on the same
// (user, database) at overlapping times, but the traffic ran through a
// connection stamped with only one of them. Only that grant's counters must
// move — the other grant's window-based fallback must not also pick it up
// just because the user/database/time window all match.
func TestGrantCounters_StampedConnectionExcludedFromOtherGrantsWindow(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "stampedquota")
	admin, _ := store.CreateUser(ctx, "stampedquotaadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()

	// Grant A: lower priority (read_only), created first.
	grantA, err := createGrantWithShape(t, ctx, store, testGrantSpec{
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{ControlReadOnly},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() grantA error = %v", err)
	}

	// Grant B: fully overlapping window, same user/database, higher priority
	// (full write) — the kind of grant GetActiveGrant would now pick, and
	// exactly what the pre-stamp window heuristic could not tell apart from A.
	grantB, err := createGrantWithShape(t, ctx, store, testGrantSpec{
		UserID:     user.UID,
		DatabaseID: database.UID,
		Controls:   []string{},
		GrantedBy:  admin.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() grantB error = %v", err)
	}

	// All traffic runs through a connection stamped with grant A.
	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.42", WithGrantUID(grantA.UID))
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	const queries = 3
	const bytesPerQuery = int64(200)
	for i := 0; i < queries; i++ {
		if _, err := store.CreateQuery(ctx, &Query{
			ConnectionID: conn.UID,
			SQLText:      "SELECT 1",
			ExecutedAt:   now,
		}); err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}
		if err := store.IncrementConnectionStats(ctx, conn.UID, bytesPerQuery); err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}
	}

	gotA, err := store.GetGrantByUID(ctx, grantA.UID)
	if err != nil {
		t.Fatalf("GetGrantByUID(grantA) error = %v", err)
	}
	if gotA.QueryCount != queries {
		t.Errorf("grantA QueryCount = %d, want %d", gotA.QueryCount, queries)
	}
	if gotA.BytesTransferred != queries*bytesPerQuery {
		t.Errorf("grantA BytesTransferred = %d, want %d", gotA.BytesTransferred, queries*bytesPerQuery)
	}

	gotB, err := store.GetGrantByUID(ctx, grantB.UID)
	if err != nil {
		t.Fatalf("GetGrantByUID(grantB) error = %v", err)
	}
	if gotB.QueryCount != 0 {
		t.Errorf("grantB QueryCount = %d, want 0 (traffic was stamped to grantA, not grantB)", gotB.QueryCount)
	}
	if gotB.BytesTransferred != 0 {
		t.Errorf("grantB BytesTransferred = %d, want 0 (traffic was stamped to grantA, not grantB)", gotB.BytesTransferred)
	}
}

// TestGrantBytesRecompute_IncludesAbortedQueryBytes covers the core scenario of
// spec 2026-07-14-09: bytes from a query aborted mid-stream by a grant limit are
// flushed to the connection via IncrementConnectionBytes (no query log row / no
// query-count bump). The grant's recomputed BytesTransferred — the value a fresh
// reconnect enforces against — must include them, otherwise the cumulative cap
// could be bypassed across short-lived reconnects.
func TestGrantBytesRecompute_IncludesAbortedQueryBytes(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "abortedbytes")
	admin, _ := store.CreateUser(ctx, "abortedbytesadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	grant, err := createGrantWithShape(t, ctx, store, testGrantSpec{
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
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, db1 := createTestUserAndDatabase(t, ctx, store, "listcountersA")
	_, db2 := createTestUserAndDatabase(t, ctx, store, "listcountersB")
	admin, _ := store.CreateUser(ctx, "listcountersadmin", "hash", []string{RoleAdmin, RoleConnector})

	now := time.Now()
	g1, err := createGrantWithShape(t, ctx, store, testGrantSpec{
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
	g2, err := createGrantWithShape(t, ctx, store, testGrantSpec{
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

// TestGetActiveGrant_DefinitionLifecycle pins down the two lifecycle states a
// definition can be in and the deliberately different answers auth gives them.
// Getting this backwards is a policy hole in one direction and a broken
// deployment in the other, so both halves are asserted together.
func TestGetActiveGrant_DefinitionLifecycle(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()

	t.Run("deactivating a definition fails its grants closed", func(t *testing.T) {
		t.Parallel()

		user, database := createTestUserAndDatabase(t, ctx, s, "lifecycle_deactivate")

		admin, err := s.CreateUser(ctx, "lifecycleadmin1", "hash", []string{RoleAdmin})
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		def := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{})
		newTestGrant(t, ctx, s, def, user.UID, database.UID, admin.UID,
			now.Add(-time.Hour), now.Add(time.Hour))

		if _, err := s.GetActiveGrant(ctx, user.UID, database.UID); err != nil {
			t.Fatalf("fixture: GetActiveGrant() error = %v", err)
		}

		if err := s.DeactivateGrantDefinition(ctx, def.UID); err != nil {
			t.Fatalf("DeactivateGrantDefinition() error = %v", err)
		}

		if _, err := s.GetActiveGrant(ctx, user.UID, database.UID); !errors.Is(err, ErrNoActiveGrant) {
			t.Fatalf("GetActiveGrant() after deactivation = %v, want ErrNoActiveGrant", err)
		}
	})

	t.Run("deactivation reaches grants pinned to an older version", func(t *testing.T) {
		t.Parallel()

		user, database := createTestUserAndDatabase(t, ctx, s, "lifecycle_lineage")

		admin, err := s.CreateUser(ctx, "lifecycleadmin2", "hash", []string{RoleAdmin})
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		def := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{})
		newTestGrant(t, ctx, s, def, user.UID, database.UID, admin.UID,
			now.Add(-time.Hour), now.Add(time.Hour))

		// Edit it, so the grant is now pinned to an *archived* version...
		def.Description = "edited"
		updated, err := s.UpdateGrantDefinition(ctx, def)
		if err != nil {
			t.Fatalf("UpdateGrantDefinition() error = %v", err)
		}

		// ...which must still authorize: archival is supersession, not
		// withdrawal.
		got, err := s.GetActiveGrant(ctx, user.UID, database.UID)
		if err != nil {
			t.Fatalf("GetActiveGrant() after an edit = %v, want the grant to keep working", err)
		}

		if got.GrantDefinitionID != def.UID {
			t.Errorf("grant now points at %s, want the version it was issued from %s",
				got.GrantDefinitionID, def.UID)
		}

		// Deactivating the *live* version withdraws the whole lineage, so the
		// grant pinned to the archived one stops authorizing too. Anything
		// else would make deactivation a kill switch that doesn't kill.
		if err := s.DeactivateGrantDefinition(ctx, updated.UID); err != nil {
			t.Fatalf("DeactivateGrantDefinition() error = %v", err)
		}

		if _, err := s.GetActiveGrant(ctx, user.UID, database.UID); !errors.Is(err, ErrNoActiveGrant) {
			t.Fatalf("GetActiveGrant() after lineage deactivation = %v, want ErrNoActiveGrant", err)
		}
	})

	t.Run("an edit never changes a live grant's shape", func(t *testing.T) {
		t.Parallel()

		user, database := createTestUserAndDatabase(t, ctx, s, "lifecycle_snapshot")

		admin, err := s.CreateUser(ctx, "lifecycleadmin3", "hash", []string{RoleAdmin})
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		def := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{Controls: []string{}})
		newTestGrant(t, ctx, s, def, user.UID, database.UID, admin.UID,
			now.Add(-time.Hour), now.Add(time.Hour))

		// Loosening *or* tightening the definition must not reach the grant:
		// snapshot safety comes from the version pin.
		def.Controls = []string{ControlReadOnly}
		if _, err := s.UpdateGrantDefinition(ctx, def); err != nil {
			t.Fatalf("UpdateGrantDefinition() error = %v", err)
		}

		got, err := s.GetActiveGrant(ctx, user.UID, database.UID)
		if err != nil {
			t.Fatalf("GetActiveGrant() error = %v", err)
		}

		if got.IsReadOnly() {
			t.Error("editing the definition retroactively tightened a live grant")
		}
	})
}

// TestListGrants_ActiveOnlyFiltersDeactivatedDefinition proves ListGrants
// agrees with GetActiveGrant about what "active" means. Before this test,
// ListGrants(ActiveOnly: true) only checked the grant's own window and
// revocation, so a database.uid the proxy had already stopped authorizing
// (via GetActiveGrant's definition-active filter) still showed up as
// accessible everywhere ListGrants(ActiveOnly) is used: the grants UI and
// the non-admin database-visibility checks in servers.go/keys.go.
func TestListGrants_ActiveOnlyFiltersDeactivatedDefinition(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()

	user, database := createTestUserAndDatabase(t, ctx, s, "listgrants_deactivate")

	admin, err := s.CreateUser(ctx, "listgrantsadmin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	def := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{})
	grant := newTestGrant(t, ctx, s, def, user.UID, database.UID, admin.UID,
		now.Add(-time.Hour), now.Add(time.Hour))

	assertListed := func(t *testing.T, want bool) {
		t.Helper()

		grants, err := s.ListGrants(ctx, GrantFilter{UserID: &user.UID, ActiveOnly: true})
		if err != nil {
			t.Fatalf("ListGrants() error = %v", err)
		}

		found := false
		for i := range grants {
			if grants[i].UID == grant.UID {
				found = true
			}
		}

		if found != want {
			t.Fatalf("ListGrants(ActiveOnly: true) contains grant = %v, want %v", found, want)
		}
	}

	assertListed(t, true)

	if _, err := s.GetActiveGrant(ctx, user.UID, database.UID); err != nil {
		t.Fatalf("fixture: GetActiveGrant() error = %v", err)
	}

	if err := s.DeactivateGrantDefinition(ctx, def.UID); err != nil {
		t.Fatalf("DeactivateGrantDefinition() error = %v", err)
	}

	// Both paths must agree the grant is no longer active.
	if _, err := s.GetActiveGrant(ctx, user.UID, database.UID); !errors.Is(err, ErrNoActiveGrant) {
		t.Fatalf("GetActiveGrant() after deactivation = %v, want ErrNoActiveGrant", err)
	}
	assertListed(t, false)
}

// TestCreateGrant_RequiresADefinition proves the model's core invariant: there
// is no way to store a grant that carries no shape.
func TestCreateGrant_RequiresADefinition(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, s, "nodefinition")

	admin, err := s.CreateUser(ctx, "nodefadmin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	now := time.Now()

	_, err = s.CreateGrant(ctx, &Grant{
		UserID:     user.UID,
		DatabaseID: database.UID,
		GrantedBy:  admin.UID,
		StartsAt:   now,
		ExpiresAt:  now.Add(time.Hour),
	})
	if !errors.Is(err, ErrGrantDefinitionRequired) {
		t.Fatalf("CreateGrant() without a definition = %v, want ErrGrantDefinitionRequired", err)
	}
}

// TestDeleteGrantDefinition covers the "hard deletion is forbidden while
// anything references it" rule, and its inverse.
func TestDeleteGrantDefinition(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()

	user, database := createTestUserAndDatabase(t, ctx, s, "defdelete")

	admin, err := s.CreateUser(ctx, "defdeleteadmin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	unused := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{})
	if err := s.DeleteGrantDefinition(ctx, unused.UID); err != nil {
		t.Fatalf("DeleteGrantDefinition() on an unused definition error = %v", err)
	}

	if _, err := s.GetGrantDefinition(ctx, unused.UID); !errors.Is(err, ErrGrantDefinitionNotFound) {
		t.Errorf("the deleted definition is still readable: %v", err)
	}

	used := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{})
	newTestGrant(t, ctx, s, used, user.UID, database.UID, admin.UID, now, now.Add(time.Hour))

	var inUse *GrantDefinitionInUseError
	if err := s.DeleteGrantDefinition(ctx, used.UID); !errors.As(err, &inUse) {
		t.Fatalf("DeleteGrantDefinition() on a referenced definition = %v, want GrantDefinitionInUseError", err)
	}

	if inUse.Grants != 1 {
		t.Errorf("blocking grant count = %d, want 1", inUse.Grants)
	}

	// An archived version cannot be deleted out from under the grants pinned
	// to it either — deletion acts on the lineage, so the whole history is
	// protected by the one live reference.
	used.Description = "edited"
	if _, err := s.UpdateGrantDefinition(ctx, used); err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	if err := s.DeleteGrantDefinition(ctx, used.UID); !errors.As(err, &inUse) {
		t.Fatalf("DeleteGrantDefinition() on an archived version = %v, want GrantDefinitionInUseError", err)
	}
}
