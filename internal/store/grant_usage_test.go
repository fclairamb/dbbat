package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// persistQueries writes n statements of bytesEach on conn, the way a proxy's
// asynchronous completion path does.
func persistQueries(t *testing.T, ctx context.Context, s *Store, conn uuid.UUID, n int, bytesEach int64) {
	t.Helper()

	for range n {
		if _, err := s.CreateQuery(ctx, &Query{ConnectionID: conn, SQLText: "SELECT 1", ExecutedAt: time.Now()}); err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}

		if err := s.IncrementConnectionStats(ctx, conn, bytesEach); err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}
	}
}

func TestRefreshGrantUsage_PicksUpAnotherReplicaWithoutDoubleCounting(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, s, "grantusage")
	admin, _ := s.CreateUser(ctx, "grantusageadmin", "hash", []string{RoleAdmin, RoleConnector})

	maxQueries, maxBytes := int64(100), int64(1<<20)

	grant, err := createGrantWithShape(t, ctx, s, testGrantSpec{
		UserID:              user.UID,
		DatabaseID:          database.UID,
		GrantedBy:           admin.UID,
		MaxQueryCounts:      &maxQueries,
		MaxBytesTransferred: &maxBytes,
		StartsAt:            time.Now().Add(-time.Hour),
		ExpiresAt:           time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("createGrantWithShape() error = %v", err)
	}

	// The local session: connect-time snapshot, then 2 statements / 300 bytes
	// that it counts in memory and that also reach the store.
	snapshot, err := s.GetGrantByUID(ctx, grant.UID)
	if err != nil {
		t.Fatalf("GetGrantByUID() error = %v", err)
	}

	local, err := s.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1", WithGrantUID(grant.UID))
	if err != nil {
		t.Fatalf("CreateConnection(local) error = %v", err)
	}

	var to atomic.Int64

	h := s.Usage().Register(grant.UID, snapshot.QueryCount, snapshot.BytesTransferred, nil, &to)
	h.SetConnection(local.UID)

	defer h.Release()

	h.AddQueries(2)
	to.Add(300)
	persistQueries(t, ctx, s, local.UID, 2, 150)

	// "Another replica": a connection on the same grant, written directly.
	remote, err := s.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2", WithGrantUID(grant.UID))
	if err != nil {
		t.Fatalf("CreateConnection(remote) error = %v", err)
	}

	persistQueries(t, ctx, s, remote.UID, 5, 200)

	if q, b := h.Queries(), h.Bytes(); q != 2 || b != 300 {
		t.Fatalf("before refresh: (%d, %d), want the local (2, 300) only", q, b)
	}

	for i := range 2 {
		if err := s.RefreshGrantUsage(ctx); err != nil {
			t.Fatalf("RefreshGrantUsage() #%d error = %v", i, err)
		}

		if q, b := h.Queries(), h.Bytes(); q != 7 || b != 1300 {
			t.Fatalf("after refresh #%d: (%d, %d), want (7, 1300): remote picked up, local counted once", i, q, b)
		}
	}

	if q, b := h.OtherQueries(), h.OtherBytes(); q != 5 || b != 1000 {
		t.Fatalf("others = (%d, %d), want the remote's (5, 1000)", q, b)
	}

	// GrantUsage itself: with no exclusion it is the plain aggregate.
	q, b, err := s.GrantUsage(ctx, grant.UID, nil)
	if err != nil {
		t.Fatalf("GrantUsage() error = %v", err)
	}

	if q != 7 || b != 1300 {
		t.Fatalf("GrantUsage(no exclusion) = (%d, %d), want (7, 1300)", q, b)
	}

	if _, _, err := s.GrantUsage(ctx, uuid.New(), nil); err == nil {
		t.Fatal("GrantUsage(unknown grant) succeeded")
	}
}
