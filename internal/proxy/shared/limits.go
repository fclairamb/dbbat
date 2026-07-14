package shared

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
)

// Limit-enforcement errors shared across proxy implementations. They are
// surfaced both at command boundaries (a new query rejected because the grant
// is exhausted/expired) and mid-stream (a running query aborted the moment a
// limit is crossed).
var (
	// ErrByteQuotaExceeded indicates the grant's max_bytes_transferred quota
	// was crossed while data was flowing.
	ErrByteQuotaExceeded = errors.New("bandwidth quota exceeded for this grant")
	// ErrGrantExpired indicates the grant's expiry time passed while the
	// session was still open.
	ErrGrantExpired = errors.New("grant expired")
	// ErrGrantRevoked indicates the grant backing the session was revoked
	// (by an admin, via the API) while the connection was still live.
	ErrGrantRevoked = errors.New("grant revoked")
)

// DefaultLimitPollInterval is how often the watchdog re-evaluates limits when
// no explicit interval is given. Small enough to cut a runaway stream promptly,
// large enough that the poll cost (two atomic loads + a time compare) is
// negligible.
const DefaultLimitPollInterval = 250 * time.Millisecond

// LimitGuard evaluates a grant's time-window and bandwidth limits against the
// live wire-byte counters. It is designed to be called on the data path:
// Check() performs at most two atomic loads and a wall-clock comparison, with
// no allocation and no locking.
//
// A guard built from a nil grant (or a grant with no limits) never trips, so
// callers can construct one unconditionally.
type LimitGuard struct {
	from *atomic.Int64
	to   *atomic.Int64

	// baseBytes is the grant's already-consumed byte total at guard
	// construction (before this session contributed anything). The live
	// counters accumulate this session's bytes on top of it, so
	// baseBytes + from + to is the grant's true running total at any instant.
	baseBytes int64
	maxBytes  *int64
	expiresAt time.Time

	// revoked, when non-nil, is the session's shared revocation flag. It is
	// flipped to true by the API's grant-revoke path (via the store's
	// RevocationRegistry). Checked on the data path so a revoked grant blocks
	// the next query and the watchdog tears the live connection down. nil when
	// there is no revocation to watch.
	revoked *atomic.Bool

	// now is the clock, injectable for deterministic tests. Defaults to
	// time.Now.
	now func() time.Time
}

// NewLimitGuard builds a guard for grant, reading live traffic from the two
// atomic counters (either may be nil). grant may be nil — the resulting guard
// enforces nothing.
func NewLimitGuard(grant *store.Grant, from, to *atomic.Int64) *LimitGuard {
	g := &LimitGuard{
		from: from,
		to:   to,
		now:  time.Now,
	}

	if grant != nil {
		g.baseBytes = grant.BytesTransferred
		g.maxBytes = grant.MaxBytesTransferred
		g.expiresAt = grant.ExpiresAt
	}

	return g
}

// WithRevocation attaches the session's shared revocation flag to the guard so
// Check/Watch also trip when the grant is revoked mid-session. Returns the
// guard for fluent construction. A nil flag is a no-op (nothing to watch),
// keeping the plain NewLimitGuard signature stable for callers/tests that don't
// track revocation.
func (g *LimitGuard) WithRevocation(revoked *atomic.Bool) *LimitGuard {
	if g == nil {
		return g
	}

	g.revoked = revoked

	return g
}

// liveBytes returns this session's cumulative client-side bytes so far.
func (g *LimitGuard) liveBytes() int64 {
	var total int64
	if g.from != nil {
		total += g.from.Load()
	}

	if g.to != nil {
		total += g.to.Load()
	}

	return total
}

// Check reports the first limit that has been crossed, or nil if the grant is
// still within bounds. Bandwidth is checked before expiry so the "gigabytes in
// seconds" case is attributed to the byte quota, but either is a valid abort
// reason.
func (g *LimitGuard) Check() error {
	if g == nil {
		return nil
	}

	// Revocation is the most authoritative reason to stop: an admin explicitly
	// pulled access, so report it ahead of the incidental byte/time limits.
	if g.revoked != nil && g.revoked.Load() {
		return ErrGrantRevoked
	}

	if g.maxBytes != nil && g.baseBytes+g.liveBytes() >= *g.maxBytes {
		return ErrByteQuotaExceeded
	}

	if !g.expiresAt.IsZero() && !g.now().Before(g.expiresAt) {
		return ErrGrantExpired
	}

	return nil
}

// Watch polls Check on a ticker until a limit is crossed or ctx is canceled.
// On the first violation it invokes onViolation with the offending error and
// returns; onViolation is never called more than once. interval <= 0 falls back
// to DefaultLimitPollInterval.
//
// Watch is the guaranteed, protocol-agnostic enforcement path: it fires even
// when a query is blocked producing no traffic (idle expiry) and even for
// protocols whose client library owns the wire (MySQL). onViolation typically
// force-closes the client and upstream conns to tear the session down.
func (g *LimitGuard) Watch(ctx context.Context, interval time.Duration, onViolation func(error)) {
	if g == nil {
		return
	}

	// Nothing to enforce — avoid spinning a pointless ticker for the lifetime
	// of the session. A revocation flag is itself something to watch, so keep
	// polling whenever one is attached even if the grant carries no limits.
	if g.maxBytes == nil && g.expiresAt.IsZero() && g.revoked == nil {
		return
	}

	if interval <= 0 {
		interval = DefaultLimitPollInterval
	}

	// Immediate check so an already-exhausted/expired grant is caught without
	// waiting a full interval.
	if err := g.Check(); err != nil {
		onViolation(err)

		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.Check(); err != nil {
				onViolation(err)

				return
			}
		}
	}
}

// setNow overrides the guard's clock. Test-only.
func (g *LimitGuard) setNow(f func() time.Time) {
	g.now = f
}
