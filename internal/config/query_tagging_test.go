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
