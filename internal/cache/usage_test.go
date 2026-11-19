package cache

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

var errRefreshFailed = errors.New("refresh failed")

// fakeStore stands in for the store aggregate: per-connection persisted
// totals, summed minus the excluded connections, as Store.GrantUsage does.
type fakeStore struct {
	queries map[uuid.UUID]int64
	bytes   map[uuid.UUID]int64
	exclude []uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{queries: map[uuid.UUID]int64{}, bytes: map[uuid.UUID]int64{}}
}

func (f *fakeStore) fetch(_ context.Context, _ uuid.UUID, exclude []uuid.UUID) (int64, int64, error) {
	f.exclude = exclude

	var q, b int64

	for c, n := range f.queries {
		if !slices.Contains(exclude, c) {
			q += n
		}
	}

	for c, n := range f.bytes {
		if !slices.Contains(exclude, c) {
			b += n
		}
	}

	return q, b, nil
}

func TestUsageRegistry_RefreshExcludesLocalConnections(t *testing.T) {
	t.Parallel()

	r := NewUsageRegistry()
	st := newFakeStore()
	grant := uuid.New()
	local := uuid.New()

	var to atomic.Int64

	h := r.Register(grant, 0, 0, nil, &to)
	h.SetConnection(local)

	// The local session ran 2 statements, 300 bytes, and they were persisted.
	h.AddQueries(2)
	to.Add(300)
	st.queries[local], st.bytes[local] = 2, 300

	// Another replica's session wrote 5 statements, 1000 bytes.
	remote := uuid.New()
	st.queries[remote], st.bytes[remote] = 5, 1000

	if err := r.Refresh(context.Background(), st.fetch); err != nil {
		t.Fatalf("Refresh() = %v", err)
	}

	if !slices.Equal(st.exclude, []uuid.UUID{local}) {
		t.Fatalf("Refresh excluded %v, want only the local connection", st.exclude)
	}

	if q, b := h.Queries(), h.Bytes(); q != 7 || b != 1300 {
		t.Fatalf("after refresh: (%d, %d), want (7, 1300): local counted once, remote picked up", q, b)
	}

	if q, b := h.OtherQueries(), h.OtherBytes(); q != 5 || b != 1000 {
		t.Fatalf("others = (%d, %d), want the remote's (5, 1000)", q, b)
	}
}

func TestUsageRegistry_DepartedSessionCountsUntilGrace(t *testing.T) {
	t.Parallel()

	r := NewUsageRegistry()
	now := time.Now()
	r.setNow(func() time.Time { return now })

	st := newFakeStore()
	grant := uuid.New()
	gone := uuid.New()

	var goneTo, stayTo atomic.Int64

	h1 := r.Register(grant, 0, 0, nil, &goneTo)
	h1.SetConnection(gone)
	h1.AddQueries(4)
	goneTo.Add(400)

	h2 := r.Register(grant, 0, 0, nil, &stayTo)
	h2.SetConnection(uuid.New())

	h1.Release()
	h1.Release() // idempotent

	// Not persisted yet: the departed totals stand in for it.
	if err := r.Refresh(context.Background(), st.fetch); err != nil {
		t.Fatalf("Refresh() = %v", err)
	}

	if q, b := h2.Queries(), h2.Bytes(); q != 4 || b != 400 {
		t.Fatalf("within the grace: (%d, %d), want (4, 400)", q, b)
	}

	// Persisted, and the grace is over: the store reads it, memory drops it.
	st.queries[gone], st.bytes[gone] = 4, 400
	now = now.Add(UsageDepartedGrace + time.Second)

	if err := r.Refresh(context.Background(), st.fetch); err != nil {
		t.Fatalf("Refresh() = %v", err)
	}

	if slices.Contains(st.exclude, gone) {
		t.Fatal("a departed connection past the grace is still excluded")
	}

	if q, b := h2.Queries(), h2.Bytes(); q != 4 || b != 400 {
		t.Fatalf("past the grace: (%d, %d), want (4, 400), counted once", q, b)
	}

	// The last session leaves; a refresh past the grace forgets the grant.
	h2.Release()
	now = now.Add(UsageDepartedGrace + time.Second)

	if err := r.Refresh(context.Background(), st.fetch); err != nil {
		t.Fatalf("Refresh() = %v", err)
	}

	if n := r.Grants(); n != 0 {
		t.Fatalf("registry still tracks %d grants after every session left", n)
	}
}

func TestUsageRegistry_SecondSessionKeepsTheSharedBase(t *testing.T) {
	t.Parallel()

	r := NewUsageRegistry()
	grant := uuid.New()

	var to1 atomic.Int64

	h1 := r.Register(grant, 10, 100, nil, &to1)
	h1.AddQueries(3)
	to1.Add(50)

	// Session 2's snapshot already includes session 1's persisted 3/50.
	h2 := r.Register(grant, 13, 150, nil, nil)

	if q, b := h2.Queries(), h2.Bytes(); q != 13 || b != 150 {
		t.Fatalf("total = (%d, %d), want (13, 150), session 1 not counted twice", q, b)
	}

	if q, b := h2.OtherQueries(), h2.OtherBytes(); q != 0 || b != 0 {
		t.Fatalf("others for session 2 = (%d, %d), want (0, 0)", q, b)
	}
}

func TestUsageRegistry_RefreshSkipsWhenASessionRegistersMidRead(t *testing.T) {
	t.Parallel()

	r := NewUsageRegistry()
	grant := uuid.New()
	h := r.Register(grant, 7, 70, nil, nil)

	err := r.Refresh(context.Background(), func(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (int64, int64, error) {
		r.Register(grant, 0, 0, nil, nil)

		return 1000, 1000, nil
	})
	if err != nil {
		t.Fatalf("Refresh() = %v", err)
	}

	if q := h.Queries(); q != 7 {
		t.Fatalf("Queries() = %d, want 7: a read racing a registration is discarded", q)
	}
}

func TestUsageRegistry_RefreshErrorKeepsBase(t *testing.T) {
	t.Parallel()

	r := NewUsageRegistry()
	h := r.Register(uuid.New(), 7, 70, nil, nil)
	boom := errRefreshFailed

	err := r.Refresh(context.Background(), func(context.Context, uuid.UUID, []uuid.UUID) (int64, int64, error) {
		return 0, 0, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Refresh() = %v, want boom", err)
	}

	if q, b := h.Queries(), h.Bytes(); q != 7 || b != 70 {
		t.Fatalf("after a failed refresh: (%d, %d), want the previous (7, 70)", q, b)
	}
}

func TestUsageHandle_NilSafe(t *testing.T) {
	t.Parallel()

	var h *UsageHandle

	h.SetConnection(uuid.New())
	h.AddQueries(1)
	h.Release()

	if h.Queries() != 0 || h.Bytes() != 0 || h.OtherQueries() != 0 || h.OtherBytes() != 0 {
		t.Fatal("a nil handle reported consumption")
	}

	var r *UsageRegistry
	if r.Register(uuid.New(), 0, 0, nil, nil) != nil {
		t.Fatal("a nil registry returned a handle")
	}

	if err := r.Refresh(context.Background(), nil); err != nil {
		t.Fatalf("nil registry Refresh() = %v", err)
	}
}
