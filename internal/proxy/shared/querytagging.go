package shared

import (
	"context"
	"log/slog"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// QueryTaggingResolver answers "does this session carry the dbbat identity
// tag?" from the store, at every connection's auth.
//
// It is the tagging counterpart of StatementTimeoutResolver and shares its
// shape on purpose: the operator-set tagging.* store parameter wins when set,
// otherwise the deployment's DBB_QUERY_TAGGING / DBB_QUERY_TAGGING_ORACLE.
// The memo lives on the store, not here, so an operator writing the parameter
// through the Settings page drops every reader's copy at once — including the
// four proxies', which each hold a resolver of their own.
//
// A nil resolver resolves everything to off, so a proxy built without one
// (unit tests, fixtures) tags nothing rather than panicking.
type QueryTaggingResolver struct {
	store *store.Store
	cfg   *config.Config
}

// NewQueryTaggingResolver builds a resolver over the store's tagging.*
// parameters, falling back to cfg's DBB_QUERY_TAGGING and
// DBB_QUERY_TAGGING_ORACLE. Either may be nil.
func NewQueryTaggingResolver(st *store.Store, cfg *config.Config) *QueryTaggingResolver {
	return &QueryTaggingResolver{store: st, cfg: cfg}
}

// Enabled reports whether a session authenticating now should carry the
// statement tag on PostgreSQL, MySQL/MariaDB or MongoDB.
//
// A store error is not fatal: it falls back to the environment default, the
// same contract StatementTimeoutResolver.Global keeps — refusing the
// connection would take the proxy down on a transient store blip.
func (r *QueryTaggingResolver) Enabled(ctx context.Context) bool {
	enabled, _ := r.resolve(ctx)
	return enabled
}

// OracleMode returns config.QueryTaggingOracleOff or
// config.QueryTaggingOracleUser for a session authenticating now.
func (r *QueryTaggingResolver) OracleMode(ctx context.Context) string {
	_, mode := r.resolve(ctx)
	return mode
}

// resolve reads the cached parameter group and interprets it, warning once per
// unrecognized stored Oracle value. A session that authenticates while the
// store is unreadable gets the environment defaults, cached like everything
// else.
func (r *QueryTaggingResolver) resolve(ctx context.Context) (bool, string) {
	if r == nil {
		return false, config.QueryTaggingOracleOff
	}

	var tagging store.Tagging
	if r.store != nil {
		tagging = r.store.ResolveTaggingCached(ctx)
	}

	return interpretTagging(tagging, r.cfg, func(msg string, args ...any) {
		slog.WarnContext(ctx, msg, args...)
	})
}

// interpretTagging turns one stored tagging.* read plus the environment
// defaults into the (enabled, oracle mode) a session authenticating now gets.
// warn receives the anomaly lines — an unrecognized tagging.oracle is folded
// to off rather than failing the session, but never silently.
func interpretTagging(
	t store.Tagging,
	cfg *config.Config,
	warn func(msg string, args ...any),
) (bool, string) {
	if store.TaggingOracleMisconfigured(t.Oracle) {
		warn("tagging.oracle parameter not recognized; Oracle statements run untagged",
			slog.String("value", t.Oracle))
	}

	return store.ResolveQueryTagging(t, cfg), store.ResolveOracleTaggingMode(t, cfg)
}
