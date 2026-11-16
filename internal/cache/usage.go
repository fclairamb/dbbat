package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// UsageDepartedGrace is how long a session that ended on this replica keeps
// counting in its grant's in-process total.
//
// A session's traffic is written to the store asynchronously, and the refresh
// only takes the store's word for a connection once that has had time to
// happen. Until then the departed session stays excluded from the store
// aggregate and its final in-memory totals stand in for it, so a session that
// ends does not briefly hand its budget back. Far longer than any store write
// takes, and well past one refresh tick.
const UsageDepartedGrace = 30 * time.Second

// UsageFetcher reads a grant's consumption from the store, leaving out the
// connections listed in exclude (the ones this replica counts in memory).
type UsageFetcher func(ctx context.Context, grantUID uuid.UUID, exclude []uuid.UUID) (queries, bytes int64, err error)

// UsageRegistry is the in-process, per-grant consumption reading the quota
// checks compare with a grant's max_query_counts and max_bytes_transferred.
//
// Without it each session reads the grant's consumption once, at connect, and
// then counts only its own traffic, so N parallel sessions get N times the
// budget. With it every session of a grant on this replica adds to one shared
// total:
//
//	total = base + departed + Σ live sessions' own counters
//
// where base is the store aggregate *excluding* every connection counted on
// the right-hand side. Excluding them, rather than subtracting what was
// persisted, is what rules out double counting: a connection is either read
// from the store or from memory, never both, whatever the asynchronous
// persistence has reached so far.
//
// base starts as the first session's connect-time snapshot and is replaced by
// Refresh, which the heartbeat runs every GrantUsageRefreshInterval. That
// refresh is the only thing that brings in other replicas' traffic, so across
// replicas the reading lags by up to one tick plus the statements in flight
// there (a statement counts once it completes and is written).
type UsageRegistry struct {
	mu     sync.Mutex
	grants map[uuid.UUID]*grantUsage
	now    func() time.Time
}

// grantUsage is one grant's shared reading.
type grantUsage struct {
	grantUID uuid.UUID

	// base* is the store aggregate, excluding every connection in live and
	// departed.
	baseQueries atomic.Int64
	baseBytes   atomic.Int64

	// departed* sums the final totals of the sessions in departed.
	departedQueries atomic.Int64
	departedBytes   atomic.Int64

	// live is copy-on-write so the data path reads it without a lock.
	live atomic.Pointer[[]*UsageHandle]

	// Guarded by the registry's mu.
	departed      []departedUsage
	registrations uint64
}

type departedUsage struct {
	connUID uuid.UUID
	queries int64
	bytes   int64
	at      time.Time
}

// UsageHandle is one live session's share of its grant's reading. All methods
// are nil-safe: a nil handle (no registry, or a grant with no quota) reports
// no consumption beyond the session's own.
type UsageHandle struct {
	registry *UsageRegistry
	entry    *grantUsage

	// Guarded by the registry's mu: set once the connection row exists.
	connUID uuid.UUID

	// snapshot* is what the session's own grant fields started from (the
	// grant's consumption at connect), so the session can be told how much
	// more the grant has used since, by others.
	snapshotQueries int64
	snapshotBytes   int64

	queries  atomic.Int64
	from, to *atomic.Int64
	released atomic.Bool
}

// NewUsageRegistry creates an empty registry.
func NewUsageRegistry() *UsageRegistry {
	return &UsageRegistry{grants: make(map[uuid.UUID]*grantUsage), now: time.Now}
}

// Register adds a live session of grantUID. snapshotQueries/snapshotBytes are
// the grant's consumption read at connect, from and to the session's wire byte
// counters (either may be nil). Release must be called when the session ends.
//
// The first session of a grant seeds base with its snapshot. Later ones do
// not: their snapshot already includes what the live sessions here persisted,
// which those sessions also count in memory.
func (r *UsageRegistry) Register(grantUID uuid.UUID, snapshotQueries, snapshotBytes int64, from, to *atomic.Int64) *UsageHandle {
	if r == nil || grantUID == uuid.Nil {
		return nil
	}

	h := &UsageHandle{
		registry:        r,
		snapshotQueries: snapshotQueries,
		snapshotBytes:   snapshotBytes,
		from:            from,
		to:              to,
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	e := r.grants[grantUID]
	if e != nil && e.idleLocked(r.now()) {
		// Nothing here is counted in memory any more: the store has it all,
		// and the new snapshot is the freshest reading there is.
		e = nil
	}

	if e == nil {
		e = &grantUsage{grantUID: grantUID}
		e.baseQueries.Store(snapshotQueries)
		e.baseBytes.Store(snapshotBytes)
		e.live.Store(&[]*UsageHandle{})
		r.grants[grantUID] = e
	}

	h.entry = e
	e.registrations++

	old := *e.live.Load()
	live := make([]*UsageHandle, 0, len(old)+1)
	live = append(live, old...)
	live = append(live, h)
	e.live.Store(&live)

	return h
}

// idleLocked reports whether the entry no longer counts anything in memory:
// no live session, and every departed one past the grace.
func (e *grantUsage) idleLocked(now time.Time) bool {
	if len(*e.live.Load()) > 0 {
		return false
	}

	for _, d := range e.departed {
		if now.Before(d.at.Add(UsageDepartedGrace)) {
			return false
		}
	}

	return true
}

// SetConnection records the session's connection uid, which the refresh
// excludes from the store aggregate. Protocols that create the connection row
// after registering call it as soon as the row exists.
func (h *UsageHandle) SetConnection(connUID uuid.UUID) {
	if h == nil {
		return
	}

	h.registry.mu.Lock()
	h.connUID = connUID
	h.registry.mu.Unlock()
}

// AddQueries charges n statements to this session.
func (h *UsageHandle) AddQueries(n int64) {
	if h == nil {
		return
	}

	h.queries.Add(n)
}

func (h *UsageHandle) ownBytes() int64 {
	var total int64
	if h.from != nil {
		total += h.from.Load()
	}

	if h.to != nil {
		total += h.to.Load()
	}

	return total
}

// Queries is the grant's statement count across every session.
func (h *UsageHandle) Queries() int64 {
	if h == nil {
		return 0
	}

	e := h.entry
	total := e.baseQueries.Load() + e.departedQueries.Load()

	for _, s := range *e.live.Load() {
		total += s.queries.Load()
	}

	return total
}

// Bytes is the grant's transfer across every session.
func (h *UsageHandle) Bytes() int64 {
	if h == nil {
		return 0
	}

	e := h.entry
	total := e.baseBytes.Load() + e.departedBytes.Load()

	for _, s := range *e.live.Load() {
		total += s.ownBytes()
	}

	return total
}

// OtherQueries is what the grant used, by statement count, beyond this
// session's snapshot plus its own statements: the other sessions' share, here
// and on other replicas. Never negative, so adding it to the session's own
// reading can only make a check stricter.
func (h *UsageHandle) OtherQueries() int64 {
	if h == nil {
		return 0
	}

	return max(0, h.Queries()-h.snapshotQueries-h.queries.Load())
}

// OtherBytes is OtherQueries for bytes.
func (h *UsageHandle) OtherBytes() int64 {
	if h == nil {
		return 0
	}

	return max(0, h.Bytes()-h.snapshotBytes-h.ownBytes())
}

// Release ends the session's live share. Its final totals keep counting for
// UsageDepartedGrace, standing in for the store rows still being written.
// Idempotent.
func (h *UsageHandle) Release() {
	if h == nil || !h.released.CompareAndSwap(false, true) {
		return
	}

	r := h.registry
	e := h.entry

	r.mu.Lock()
	defer r.mu.Unlock()

	queries, bytes := h.queries.Load(), h.ownBytes()

	if h.connUID != uuid.Nil {
		e.departed = append(e.departed, departedUsage{connUID: h.connUID, queries: queries, bytes: bytes, at: r.now()})
		e.departedQueries.Add(queries)
		e.departedBytes.Add(bytes)
	}

	old := *e.live.Load()
	live := make([]*UsageHandle, 0, len(old))

	for _, s := range old {
		if s != h {
			live = append(live, s)
		}
	}

	e.live.Store(&live)
}

// Grants reports how many grants the registry currently tracks. For tests and
// diagnostics.
func (r *UsageRegistry) Grants() int {
	if r == nil {
		return 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.grants)
}

// Refresh re-reads every tracked grant's base from the store through fetch.
// It returns the first error met; a grant that failed keeps its previous
// base, and the next refresh tries again.
//
// Departed sessions past the grace stop being excluded in the same step that
// stops counting them: the new base, which includes their rows, is stored
// first, so a reader in between sees a brief overcount, never an undercount.
func (r *UsageRegistry) Refresh(ctx context.Context, fetch UsageFetcher) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	entries := make([]*grantUsage, 0, len(r.grants))

	for _, e := range r.grants {
		entries = append(entries, e)
	}
	r.mu.Unlock()

	var firstErr error

	for _, e := range entries {
		if err := r.refreshOne(ctx, e, fetch); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func (r *UsageRegistry) refreshOne(ctx context.Context, e *grantUsage, fetch UsageFetcher) error {
	now := r.now()

	r.mu.Lock()

	generation := e.registrations

	var (
		exclude  []uuid.UUID
		expiring int
	)

	for _, h := range *e.live.Load() {
		if h.connUID != uuid.Nil {
			exclude = append(exclude, h.connUID)
		}
	}

	for _, d := range e.departed {
		if now.Before(d.at.Add(UsageDepartedGrace)) {
			exclude = append(exclude, d.connUID)
		} else {
			expiring++
		}
	}

	r.mu.Unlock()

	queries, bytes, err := fetch(ctx, e.grantUID, exclude)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// A session registered during the read: its connection may hold rows the
	// read counted and that it also counts itself. Skip; the next tick has it.
	if e.registrations != generation {
		return nil
	}

	e.baseQueries.Store(queries)
	e.baseBytes.Store(bytes)

	if expiring > 0 {
		kept := e.departed[:0]

		for _, d := range e.departed {
			if now.Before(d.at.Add(UsageDepartedGrace)) {
				kept = append(kept, d)

				continue
			}

			e.departedQueries.Add(-d.queries)
			e.departedBytes.Add(-d.bytes)
		}

		e.departed = kept
	}

	if len(*e.live.Load()) == 0 && len(e.departed) == 0 && r.grants[e.grantUID] == e {
		delete(r.grants, e.grantUID)
	}

	return nil
}

// setNow overrides the registry clock. Test-only.
func (r *UsageRegistry) setNow(f func() time.Time) {
	r.now = f
}
