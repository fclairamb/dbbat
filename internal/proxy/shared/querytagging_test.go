package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

func TestQueryTaggingResolver_NilResolvesOff(t *testing.T) {
	t.Parallel()

	var r *QueryTaggingResolver

	assert.False(t, r.Enabled(t.Context()))
	assert.Equal(t, config.QueryTaggingOracleOff, r.OracleMode(t.Context()))
}

func TestQueryTaggingResolver_EnvFallbackWithoutStore(t *testing.T) {
	t.Parallel()

	// No store: the environment default is the whole decision.
	off := NewQueryTaggingResolver(nil, &config.Config{})
	assert.False(t, off.Enabled(t.Context()))
	assert.Equal(t, config.QueryTaggingOracleOff, off.OracleMode(t.Context()))

	envOn := NewQueryTaggingResolver(nil, &config.Config{
		QueryTagging: config.QueryTaggingConfig{Enabled: true, Oracle: config.QueryTaggingOracleUser},
	})
	assert.True(t, envOn.Enabled(t.Context()))
	assert.Equal(t, config.QueryTaggingOracleUser, envOn.OracleMode(t.Context()))
}

func TestInterpretTagging_WarnsOnUnrecognizedOracle(t *testing.T) {
	t.Parallel()

	var warnings []string

	warn := func(msg string, _ ...any) { warnings = append(warnings, msg) }

	// A stored value neither "off" nor "user" resolves to off — the way
	// ParseStatementTimeout folds a malformed duration into "no limit" — and
	// says so rather than silently disabling a feature someone asked for.
	enabled, mode := interpretTagging(
		store.Tagging{Enabled: "true", Oracle: "everyone"},
		nil, warn)

	assert.True(t, enabled)
	assert.Equal(t, config.QueryTaggingOracleOff, mode)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "not recognized")

	// The recognized values stay silent.
	warnings = nil

	_, mode = interpretTagging(store.Tagging{Oracle: config.QueryTaggingOracleUser}, nil, warn)
	assert.Equal(t, config.QueryTaggingOracleUser, mode)

	_, mode = interpretTagging(store.Tagging{Oracle: config.QueryTaggingOracleOff}, nil, warn)
	assert.Equal(t, config.QueryTaggingOracleOff, mode)
	assert.Empty(t, warnings, "recognized values must not warn")
}
