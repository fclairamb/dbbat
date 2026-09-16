package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryTaggingDefaultsToOff pins the safety default: tagging changes the
// bytes the target database receives, so an upgrade must never start doing it
// on its own.
func TestQueryTaggingDefaultsToOff(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.False(t, cfg.QueryTagging.Enabled)
}

// TestQueryTaggingEnvVarReachesItsKey is the whole point of the exact-match
// rule in envTransform: DBB_QUERY_TAGGING is one word short of the nested key
// it configures, so without the mapping the operator turns tagging on, gets no
// error, and nothing happens.
func TestQueryTaggingEnvVarReachesItsKey(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_QUERY_TAGGING", "true")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.True(t, cfg.QueryTagging.Enabled)
}

// TestQueryTaggingEnabledSpellingAlsoWorks covers the config-file-shaped
// variable, which is the spelling every other nested section uses.
func TestQueryTaggingEnabledSpellingAlsoWorks(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_QUERY_TAGGING_ENABLED", "true")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.True(t, cfg.QueryTagging.Enabled)
}

// TestQueryTaggingEnvKeyMapping checks the transform directly, so a regression
// names the mapping rather than a downstream symptom.
func TestQueryTaggingEnvKeyMapping(t *testing.T) {
	t.Parallel()

	key, value := envTransform("DBB_QUERY_TAGGING", "true")
	assert.Equal(t, "query_tagging.enabled", key)
	assert.Equal(t, "true", value)

	key, _ = envTransform("DBB_QUERY_TAGGING_ENABLED", "true")
	assert.Equal(t, "query_tagging.enabled", key,
		"the prefix rule must not be shadowed by the exact-match one")
}

// TestQueryTaggingOracleDefaultsToOff pins the safety default separately from
// DBB_QUERY_TAGGING's: Oracle's trade-off is its own — V$SQL keys on statement
// text, so the tag's cardinality is its cost — and an operator who turned
// tagging on for PostgreSQL has not consented to it.
func TestQueryTaggingOracleDefaultsToOff(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://localhost/dbbat")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	on, err := cfg.QueryTagging.ResolveOracle()
	require.NoError(t, err)
	assert.False(t, on)

	// Turning the shared switch on must not reach Oracle.
	t.Setenv("DBB_QUERY_TAGGING", "true")

	cfg, err = Load(LoadOptions{})
	require.NoError(t, err)
	require.True(t, cfg.QueryTagging.Enabled)

	on, err = cfg.QueryTagging.ResolveOracle()
	require.NoError(t, err)
	assert.False(t, on, "DBB_QUERY_TAGGING does not enable the Oracle tag")
}

// TestQueryTaggingOracleEnvVarReachesItsKey checks the koanf path, which rides
// the existing query_tagging_ prefix rather than needing an exact-match entry.
func TestQueryTaggingOracleEnvVarReachesItsKey(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://localhost/dbbat")
	t.Setenv("DBB_QUERY_TAGGING_ORACLE", "user")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, QueryTaggingOracleUser, cfg.QueryTagging.Oracle)

	on, err := cfg.QueryTagging.ResolveOracle()
	require.NoError(t, err)
	assert.True(t, on)
}

// TestQueryTaggingOracleRejectsAnythingElse is why the value is resolved rather
// than parsed with a fallback: the absence of a tag looks exactly like the
// feature being off, so a typo has to stop the process instead of quietly
// disabling what the operator asked for.
func TestQueryTaggingOracleRejectsAnythingElse(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"conn", "connection", "on", "true", "yes", "1"} {
		_, err := QueryTaggingConfig{Oracle: value}.ResolveOracle()
		assert.ErrorIs(t, err, ErrQueryTaggingOracleInvalid, "value %q", value)
	}

	for _, value := range []string{"", "off", "OFF", " off ", "User", "USER"} {
		_, err := QueryTaggingConfig{Oracle: value}.ResolveOracle()
		assert.NoError(t, err, "value %q", value)
	}
}
