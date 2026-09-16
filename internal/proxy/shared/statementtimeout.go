package shared

import (
	"context"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// statementTimeoutCacheTTL is how long a resolver reuses the store parameter it
// last read. A statement limit is an operator setting that changes a few times
// a year, and this is read once per *connection* on five protocols, so the
// alternative is a database round trip on every login for a value that almost
// never moves. Ten seconds is short enough that an operator editing it from the
// Settings page sees it take effect while they are still looking at the page.
const statementTimeoutCacheTTL = 10 * time.Second

// StatementTimeoutResolver answers "what is the instance-wide per-statement
// limit?" cheaply enough to ask at every connection.
//
// It owns the *global* half of the three-layer resolution: the operator-set
// limits.statement_timeout store parameter when there is one, otherwise
// DBB_STATEMENT_TIMEOUT. The per-grant-definition half lives on the grant
// (store.AccessGrant.StatementTimeout), and For() composes the two.
//
// A nil resolver resolves everything to "no limit", so a proxy built without
// one (unit tests, fixtures) enforces nothing rather than panicking.
type StatementTimeoutResolver struct {
	store *store.Store

	// fallback is the env-var default, parsed once at construction: it cannot
	// change while the process runs.
	fallback time.Duration

	mu     sync.Mutex
	value  time.Duration
	readAt time.Time
}

// NewStatementTimeoutResolver builds a resolver over the store's limits.*
// parameters, falling back to cfg's DBB_STATEMENT_TIMEOUT. Either may be nil.
func NewStatementTimeoutResolver(st *store.Store, cfg *config.Config) *StatementTimeoutResolver {
	fallback := time.Duration(0)
	if cfg != nil {
		fallback = store.ParseStatementTimeout(cfg.StatementTimeout)
	}

	return &StatementTimeoutResolver{store: st, fallback: fallback}
}

// Global returns the instance-wide limit, zero when there is none.
//
// A store error is not fatal and not fail-closed either: it falls back to the
// environment default. The alternative — refusing the connection — would take
// the whole proxy down on a transient store blip, and the alternative in the
// other direction — inventing a limit — would kill sessions nobody configured
// a limit for.
func (r *StatementTimeoutResolver) Global(ctx context.Context) time.Duration {
	if r == nil {
		return 0
	}

	if r.store == nil {
		return r.fallback
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.readAt.IsZero() && time.Since(r.readAt) < statementTimeoutCacheTTL {
		return r.value
	}

	limits, err := r.store.GetLimits(ctx)
	if err != nil {
		// Cache the fallback too: a store that is down stays down for more
		// than one connection, and hammering it per login helps nobody.
		r.value = r.fallback
		r.readAt = time.Now()

		return r.value
	}

	if limits.StatementTimeout != "" {
		r.value = store.ParseStatementTimeout(limits.StatementTimeout)
	} else {
		r.value = r.fallback
	}

	r.readAt = time.Now()

	return r.value
}

// For resolves the limit that applies to one session: the grant definition's
// value when it has one (including an explicit 0, "no limit, overriding the
// global"), otherwise the instance-wide default.
func (r *StatementTimeoutResolver) For(ctx context.Context, grant *store.Grant) time.Duration {
	if grant == nil {
		return 0
	}

	return grant.StatementTimeout(r.Global(ctx))
}

// Invalidate drops the cached global value so the next Global() re-reads the
// store. Called after an operator writes the parameter in-process; other
// replicas pick the change up within statementTimeoutCacheTTL.
func (r *StatementTimeoutResolver) Invalidate() {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.readAt = time.Time{}
}
