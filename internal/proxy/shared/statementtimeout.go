package shared

import (
	"context"
	"time"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

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
// The memo itself lives on the store, not here, so an operator writing the
// parameter through the API drops every reader's copy at once — including the
// five proxies', which each hold a resolver of their own.
type StatementTimeoutResolver struct {
	store *store.Store
	cfg   *config.Config
}

// NewStatementTimeoutResolver builds a resolver over the store's limits.*
// parameters, falling back to cfg's DBB_STATEMENT_TIMEOUT. Either may be nil.
func NewStatementTimeoutResolver(st *store.Store, cfg *config.Config) *StatementTimeoutResolver {
	return &StatementTimeoutResolver{store: st, cfg: cfg}
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
		return store.ResolveStatementTimeout(store.Limits{}, r.cfg)
	}

	return r.store.ResolveStatementTimeoutCached(ctx, r.cfg)
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

// Invalidate drops the process-wide memo so the next Global() re-reads the
// store.
func (r *StatementTimeoutResolver) Invalidate() {
	if r == nil {
		return
	}

	r.store.InvalidateLimits()
}
