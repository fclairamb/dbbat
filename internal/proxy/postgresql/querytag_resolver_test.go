package postgresql

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// resolvingSession is a session about to authenticate with the store-backed
// tagging resolver installed, the way Server.handleConnection hands one to
// every session once main.go has called SetQueryTaggingResolver.
func resolvingSession(t *testing.T, dataStore *store.Store, cfg *config.Config) *Session {
	t.Helper()

	s := newTestSession("write")
	s.ctx = context.Background()
	s.connUID = uuid.MustParse(querytagConnUID)
	// The environment default the resolver falls back to, as
	// SetQueryTagging would have stored it at startup.
	s.queryTagging = cfg.QueryTagging.Enabled
	s.queryTaggingResolver = shared.NewQueryTaggingResolver(dataStore, cfg)

	return s
}

// TestQueryTagging_ResolvedFromTheStoreAtAuth is the point of the whole spec:
// the tagging decision comes from the tagging.enabled parameter an operator can
// flip from the Settings page, not from the DBB_QUERY_TAGGING the deployment
// was rolled out with — in both directions, and proven on the wire rather than
// on the resolver alone.
//
// The cases share one storage container and one parameter, so they run in
// sequence.
//
//nolint:paralleltest,tparallel // One tagging.enabled parameter, rewritten by each case: parallel would test the race, not the resolution.
func TestQueryTagging_ResolvedFromTheStoreAtAuth(t *testing.T) {
	t.Parallel()

	dataStore := newCopyTestStore(t)
	ctx := context.Background()

	envOff := &config.Config{}
	envOn := &config.Config{QueryTagging: config.QueryTaggingConfig{Enabled: true}}

	t.Run("stored true tags although the environment is off", func(t *testing.T) {
		require.NoError(t, dataStore.SetTagging(ctx, store.Tagging{Enabled: "true"}))
		dataStore.InvalidateTagging()

		s := resolvingSession(t, dataStore, envOff)
		s.resolveQueryTagging("florent", "diag-paris")
		require.True(t, s.queryTagging)

		msg := &pgproto3.Query{String: "SELECT 1"}
		require.NoError(t, s.handleQuery(msg))

		assert.True(t, strings.HasPrefix(msg.String, "/*dbbat="),
			"the statement forwarded upstream must carry the tag: %q", msg.String)
		assert.Contains(t, msg.String, "user='florent'")
		assert.Contains(t, msg.String, "conn='3f9a1c7b2e4d'")
		assert.Contains(t, msg.String, "grant='diag-paris'")
		assert.True(t, strings.HasSuffix(msg.String, "SELECT 1"))

		require.NotNil(t, s.currentQuery)
		assert.Equal(t, "SELECT 1", s.currentQuery.sql,
			"what dbbat records is still the client's statement")
	})

	t.Run("stored false wins over an environment default that is on", func(t *testing.T) {
		require.NoError(t, dataStore.SetTagging(ctx, store.Tagging{Enabled: "false"}))
		dataStore.InvalidateTagging()

		s := resolvingSession(t, dataStore, envOn)
		require.True(t, s.queryTagging, "the environment default starts the session on")

		s.resolveQueryTagging("florent", "diag-paris")
		assert.False(t, s.queryTagging,
			"an operator turning tagging off must not need a redeploy to undo DBB_QUERY_TAGGING")

		msg := &pgproto3.Query{String: "SELECT 1"}
		require.NoError(t, s.handleQuery(msg))
		assert.Equal(t, "SELECT 1", msg.String, "nothing may be prepended once tagging is off")
	})

	t.Run("unset falls back to the environment", func(t *testing.T) {
		require.NoError(t, dataStore.SetTagging(ctx, store.Tagging{}))
		dataStore.InvalidateTagging()

		s := resolvingSession(t, dataStore, envOn)
		s.resolveQueryTagging("florent", "diag-paris")
		assert.True(t, s.queryTagging, "no parameter means the deployment default decides")
	})

	t.Run("a live session keeps the decision it authenticated under", func(t *testing.T) {
		require.NoError(t, dataStore.SetTagging(ctx, store.Tagging{Enabled: "true"}))
		dataStore.InvalidateTagging()

		s := resolvingSession(t, dataStore, envOff)
		s.resolveQueryTagging("florent", "diag-paris")
		require.True(t, s.queryTagging)

		// The operator turns it off mid-session.
		require.NoError(t, dataStore.SetTagging(ctx, store.Tagging{Enabled: "false"}))
		dataStore.InvalidateTagging()
		require.False(t, s.queryTaggingResolver.Enabled(ctx),
			"the next session to authenticate must see the new value")

		msg := &pgproto3.Query{String: "SELECT 1"}
		require.NoError(t, s.handleQuery(msg))

		assert.True(t, strings.HasPrefix(msg.String, "/*dbbat="),
			"a statement tagged on some executions and not others would get two digests in pg_stat_statements")
	})
}
