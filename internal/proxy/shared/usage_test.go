package shared

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

// quotaSession is one session's state as a proxy holds it: its own copy of the
// grant (connect-time snapshot plus its own counters), its wire counters, its
// guard and its usage handle.
type quotaSession struct {
	grant    *store.Grant
	from, to atomic.Int64
	usage    *cache.UsageHandle
	guard    *LimitGuard
}

func newQuotaSession(t *testing.T, registry *cache.UsageRegistry, grantUID uuid.UUID, def *store.GrantDefinition) *quotaSession {
	t.Helper()

	s := &quotaSession{grant: &store.Grant{
		UID:        grantUID,
		ExpiresAt:  time.Now().Add(time.Hour),
		Definition: def,
	}}
	s.usage = RegisterGrantUsage(registry, s.grant, &s.from, &s.to)
	s.usage.SetConnection(uuid.New())
	s.guard = NewLimitGuard(s.grant, &s.from, &s.to).WithUsage(s.usage)

	return s
}

// query is one completed statement: what each proxy does on completion.
func (s *quotaSession) query(bytes int64) {
	s.to.Add(bytes)
	s.grant.QueryCount++
	s.grant.BytesTransferred += bytes
	s.usage.AddQueries(1)
}

// quotaExceeded reports (statement quota hit, byte quota hit).
func quotaExceeded(s *quotaSession) (bool, bool) {
	q, b := QuotaUsage(s.grant, s.usage)

	return q >= *s.grant.MaxQueryCounts(), b >= *s.grant.MaxBytesTransferred()
}

func TestGrantUsage_ParallelSessionsShareTheByteQuota(t *testing.T) {
	t.Parallel()

	const budget = int64(1000)

	registry := cache.NewUsageRegistry()
	grantUID := uuid.New()
	def := &store.GrantDefinition{MaxBytesTransferred: int64Ptr(budget), MaxQueryCounts: int64Ptr(1000)}

	s1 := newQuotaSession(t, registry, grantUID, def)
	s2 := newQuotaSession(t, registry, grantUID, def)

	s1.query(600)

	if err := s1.guard.Check(); err != nil {
		t.Fatalf("session 1 at 0.6·B: Check() = %v, want nil", err)
	}

	if err := s2.guard.Check(); err != nil {
		t.Fatalf("session 2 before any traffic: Check() = %v, want nil", err)
	}

	s2.query(600)

	if err := s2.guard.Check(); !errors.Is(err, ErrByteQuotaExceeded) {
		t.Fatalf("session 2 at 1.2·B in total: Check() = %v, want ErrByteQuotaExceeded", err)
	}

	if err := s1.guard.Check(); !errors.Is(err, ErrByteQuotaExceeded) {
		t.Fatalf("session 1 once the grant is over: Check() = %v, want ErrByteQuotaExceeded", err)
	}

	if _, overBytes := quotaExceeded(s2); !overBytes {
		t.Fatal("session 2's per-statement byte check passed at 1.2·B in total")
	}

	// The bug this closes: without the shared reading, each guard only sees
	// its own 0.6·B and neither trips.
	// (Built from the connect-time snapshot, as the session's own guard was.)
	snapshot := &store.Grant{UID: grantUID, ExpiresAt: s2.grant.ExpiresAt, Definition: def}
	alone := NewLimitGuard(snapshot, &s2.from, &s2.to)
	if err := alone.Check(); err != nil {
		t.Fatalf("control without shared usage: Check() = %v, want nil (per-session snapshot)", err)
	}
}

func TestGrantUsage_ParallelSessionsShareTheQueryQuota(t *testing.T) {
	t.Parallel()

	registry := cache.NewUsageRegistry()
	grantUID := uuid.New()
	def := &store.GrantDefinition{MaxQueryCounts: int64Ptr(5), MaxBytesTransferred: int64Ptr(1 << 30)}

	s1 := newQuotaSession(t, registry, grantUID, def)
	s2 := newQuotaSession(t, registry, grantUID, def)

	for range 3 {
		s1.query(1)
	}

	if overQueries, _ := quotaExceeded(s2); overQueries {
		t.Fatal("session 2 refused at 3 of 5 statements")
	}

	s2.query(1)

	if overQueries, _ := quotaExceeded(s2); overQueries {
		t.Fatal("session 2 refused at 4 of 5 statements")
	}

	s2.query(1)

	if overQueries, _ := quotaExceeded(s2); !overQueries {
		t.Fatal("session 2 admitted a 6th statement on a grant capped at 5")
	}

	if overQueries, _ := quotaExceeded(s1); !overQueries {
		t.Fatal("session 1 admitted a 6th statement on a grant capped at 5")
	}

	// Before the fix each session compared only its own count: 3 and 2.
	if s1.grant.QueryCount >= 5 || s2.grant.QueryCount >= 5 {
		t.Fatalf("own counts = %d, %d; the control expects neither alone at the cap", s1.grant.QueryCount, s2.grant.QueryCount)
	}
}

func TestGrantUsage_ReleasedSessionStillCounts(t *testing.T) {
	t.Parallel()

	registry := cache.NewUsageRegistry()
	grantUID := uuid.New()
	def := &store.GrantDefinition{MaxQueryCounts: int64Ptr(5), MaxBytesTransferred: int64Ptr(1000)}

	s1 := newQuotaSession(t, registry, grantUID, def)
	s1.query(900)
	s1.usage.Release()

	// A session opened right after: its store snapshot may not include s1's
	// rows yet (they are written asynchronously), the registry still does.
	s2 := newQuotaSession(t, registry, grantUID, def)
	s2.query(200)

	if err := s2.guard.Check(); !errors.Is(err, ErrByteQuotaExceeded) {
		t.Fatalf("Check() after a sibling session ended = %v, want ErrByteQuotaExceeded", err)
	}
}

func TestGrantUsage_SingleSessionUnchanged(t *testing.T) {
	t.Parallel()

	registry := cache.NewUsageRegistry()
	def := &store.GrantDefinition{MaxQueryCounts: int64Ptr(3), MaxBytesTransferred: int64Ptr(1000)}

	s := newQuotaSession(t, registry, uuid.New(), def)
	s.grant.QueryCount = 0

	for range 2 {
		s.query(400)
	}

	if q, b := QuotaUsage(s.grant, s.usage); q != s.grant.QueryCount || b != s.grant.BytesTransferred {
		t.Fatalf("QuotaUsage() = (%d, %d), want the session's own (%d, %d)", q, b, s.grant.QueryCount, s.grant.BytesTransferred)
	}

	if err := s.guard.Check(); err != nil {
		t.Fatalf("Check() at 800 of 1000 = %v, want nil", err)
	}

	s.query(200)

	if err := s.guard.Check(); !errors.Is(err, ErrByteQuotaExceeded) {
		t.Fatalf("Check() at 1000 of 1000 = %v, want ErrByteQuotaExceeded", err)
	}

	if overQueries, _ := quotaExceeded(s); !overQueries {
		t.Fatal("3 of 3 statements not refused")
	}
}

func TestGrantUsage_GrantWithoutQuotaRegistersNothing(t *testing.T) {
	t.Parallel()

	registry := cache.NewUsageRegistry()
	grant := &store.Grant{UID: uuid.New(), Definition: &store.GrantDefinition{}}

	var from, to atomic.Int64

	if h := RegisterGrantUsage(registry, grant, &from, &to); h != nil {
		t.Fatal("RegisterGrantUsage() returned a handle for a grant with no quota")
	}

	if n := registry.Grants(); n != 0 {
		t.Fatalf("registry tracks %d grants, want 0", n)
	}

	if h := RegisterGrantUsage(registry, nil, &from, &to); h != nil {
		t.Fatal("RegisterGrantUsage(nil grant) returned a handle")
	}
}
