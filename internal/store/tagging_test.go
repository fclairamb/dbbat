package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

func TestTagging_ParameterRoundTrip(t *testing.T) {
	t.Parallel()

	s := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	// Unset: the environment defaults apply.
	tagging, err := s.GetTagging(ctx)
	require.NoError(t, err)
	assert.Empty(t, tagging.Enabled)
	assert.Empty(t, tagging.Oracle)

	cfg := &config.Config{
		QueryTagging: config.QueryTaggingConfig{Enabled: true, Oracle: config.QueryTaggingOracleUser},
	}
	assert.True(t, ResolveQueryTagging(tagging, cfg))
	assert.Equal(t, config.QueryTaggingOracleUser, ResolveOracleTaggingMode(tagging, cfg))

	// Set: the parameters win over the environment — in either direction.
	require.NoError(t, s.SetTagging(ctx, Tagging{Enabled: "false", Oracle: config.QueryTaggingOracleOff}))

	tagging, err = s.GetTagging(ctx)
	require.NoError(t, err)
	assert.Equal(t, "false", tagging.Enabled)
	assert.Equal(t, config.QueryTaggingOracleOff, tagging.Oracle)
	assert.False(t, ResolveQueryTagging(tagging, cfg),
		`an explicit "false" must override the environment default, not fall through to it`)
	assert.Equal(t, config.QueryTaggingOracleOff, ResolveOracleTaggingMode(tagging, cfg))

	// Oracle "user" turns the per-user tag on regardless of the environment.
	require.NoError(t, s.SetTagging(ctx, Tagging{Enabled: "true", Oracle: config.QueryTaggingOracleUser}))

	tagging, err = s.GetTagging(ctx)
	require.NoError(t, err)
	assert.Equal(t, "true", tagging.Enabled)
	assert.Equal(t, config.QueryTaggingOracleUser, ResolveOracleTaggingMode(tagging, cfg))

	// Clearing deletes the parameters rather than storing blank values, so
	// "unset" really does fall back to the environment again.
	require.NoError(t, s.SetTagging(ctx, Tagging{}))

	tagging, err = s.GetTagging(ctx)
	require.NoError(t, err)
	assert.Empty(t, tagging.Enabled)
	assert.Empty(t, tagging.Oracle)
	assert.Equal(t, config.QueryTaggingOracleUser, ResolveOracleTaggingMode(tagging, cfg))

	// Clearing an already-absent parameter is not an error.
	require.NoError(t, s.SetTagging(ctx, Tagging{}))
}

func TestResolveQueryTagging(t *testing.T) {
	t.Parallel()

	cfgOn := &config.Config{QueryTagging: config.QueryTaggingConfig{Enabled: true}}

	assert.True(t, ResolveQueryTagging(Tagging{}, cfgOn), "unset falls through to the environment")
	assert.False(t, ResolveQueryTagging(Tagging{}, nil))
	assert.True(t, ResolveQueryTagging(Tagging{Enabled: "true"}, nil))
	assert.False(t, ResolveQueryTagging(Tagging{Enabled: "false"}, cfgOn),
		"the parameter wins over the environment in both directions")
	assert.False(t, ResolveQueryTagging(Tagging{Enabled: "nonsense"}, nil),
		"anything but \"true\" is off — the API only ever writes true/false")
}

func TestResolveOracleTaggingMode(t *testing.T) {
	t.Parallel()

	cfgUser := &config.Config{
		QueryTagging: config.QueryTaggingConfig{Oracle: config.QueryTaggingOracleUser},
	}

	assert.Equal(t, config.QueryTaggingOracleUser, ResolveOracleTaggingMode(Tagging{}, cfgUser),
		"unset falls through to the environment")
	assert.Equal(t, config.QueryTaggingOracleOff, ResolveOracleTaggingMode(Tagging{}, nil))
	assert.Equal(t, config.QueryTaggingOracleUser, ResolveOracleTaggingMode(Tagging{Oracle: "user"}, nil))
	assert.Equal(t, config.QueryTaggingOracleOff, ResolveOracleTaggingMode(Tagging{Oracle: "off"}, cfgUser),
		"the parameter wins over the environment in both directions")

	// An unrecognized stored value folds to off — the resolver is what warns
	// about it; through the API the same value is a 400 at write time.
	assert.Equal(t, config.QueryTaggingOracleOff, ResolveOracleTaggingMode(Tagging{Oracle: "everyone"}, nil))
}

func TestTaggingOracleMisconfigured(t *testing.T) {
	t.Parallel()

	assert.False(t, TaggingOracleMisconfigured(""), "unset is not a misconfiguration")
	assert.False(t, TaggingOracleMisconfigured("off"))
	assert.False(t, TaggingOracleMisconfigured("user"))
	assert.False(t, TaggingOracleMisconfigured(" USER "), "the env var matching is case-insensitive")
	assert.True(t, TaggingOracleMisconfigured("everyone"))
}

func TestTagging_ResolveTaggingCached(t *testing.T) {
	t.Parallel()

	s := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	require.NoError(t, s.SetTagging(ctx, Tagging{Enabled: "true"}))
	s.InvalidateTagging()

	// First read populates the memo.
	tagging := s.ResolveTaggingCached(ctx)
	assert.Equal(t, "true", tagging.Enabled)

	// A write that bypasses InvalidateTagging is invisible until the memo
	// expires: the cached read does not query the store per call.
	require.NoError(t, s.SetParameter(ctx, GroupTagging, KeyTaggingEnabled, "false"))

	tagging = s.ResolveTaggingCached(ctx)
	assert.Equal(t, "true", tagging.Enabled,
		"the cached read must not re-query the store within the TTL")

	// Dropping the memo (what the API handler does after a write) makes the
	// next read see the new value.
	s.InvalidateTagging()

	tagging = s.ResolveTaggingCached(ctx)
	assert.Equal(t, "false", tagging.Enabled)

	// And the statement-timeout resolution shares the same memo, so its own
	// path still works after the tagging refresh.
	require.NoError(t, s.SetLimits(ctx, Limits{StatementTimeout: "7s"}))
	s.InvalidateLimits()

	assert.Equal(t, 7*time.Second, s.ResolveStatementTimeoutCached(ctx, nil))
}
