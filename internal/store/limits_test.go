package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

func TestLimits_StatementTimeoutParameter(t *testing.T) {
	t.Parallel()

	s := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	// Unset: the environment default applies.
	limits, err := s.GetLimits(ctx)
	require.NoError(t, err)
	assert.Empty(t, limits.StatementTimeout)

	cfg := &config.Config{StatementTimeout: "45s"}
	assert.Equal(t, 45*time.Second, ResolveStatementTimeout(limits, cfg))

	// Set: the parameter wins.
	require.NoError(t, s.SetLimits(ctx, Limits{StatementTimeout: "10s"}))

	limits, err = s.GetLimits(ctx)
	require.NoError(t, err)
	assert.Equal(t, "10s", limits.StatementTimeout)
	assert.Equal(t, 10*time.Second, ResolveStatementTimeout(limits, cfg))

	// An explicit "0" disables the limit outright, environment default included.
	require.NoError(t, s.SetLimits(ctx, Limits{StatementTimeout: "0"}))

	limits, err = s.GetLimits(ctx)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), ResolveStatementTimeout(limits, cfg),
		`an explicit "0" must override the environment default, not fall through to it`)

	// Clearing it falls back to the environment default again, rather than
	// storing a blank value that would read as "0".
	require.NoError(t, s.SetLimits(ctx, Limits{StatementTimeout: ""}))

	limits, err = s.GetLimits(ctx)
	require.NoError(t, err)
	assert.Empty(t, limits.StatementTimeout)
	assert.Equal(t, 45*time.Second, ResolveStatementTimeout(limits, cfg))

	// Clearing an already-absent parameter is not an error.
	require.NoError(t, s.SetLimits(ctx, Limits{StatementTimeout: ""}))
}

func TestStatementTimeoutMisconfigured(t *testing.T) {
	t.Parallel()

	assert.False(t, StatementTimeoutMisconfigured(""), "unset is not a misconfiguration")
	assert.False(t, StatementTimeoutMisconfigured("0"), `an explicit "0" is a valid choice`)
	assert.False(t, StatementTimeoutMisconfigured("30s"))
	assert.True(t, StatementTimeoutMisconfigured("thirty"))
	assert.True(t, StatementTimeoutMisconfigured("30"), "a bare number is not a Go duration")
	assert.True(t, StatementTimeoutMisconfigured("-5s"))
}

func TestTermination_Message(t *testing.T) {
	t.Parallel()

	assert.Empty(t, Termination{}.Message(), "the zero value describes no termination")

	full := Termination{
		Reason:   TerminationStatementTimeout,
		Limit:    30 * time.Second,
		Observed: 32100 * time.Millisecond,
	}
	assert.Equal(t, "statement timeout: limit 30s, ran 32.1s, session terminated by dbbat", full.Message())

	// Without an observed duration the message still names the limit, which is
	// the actionable half.
	partial := Termination{Reason: TerminationStatementTimeout, Limit: 30 * time.Second}
	assert.Equal(t, "statement timeout: limit 30s, session terminated by dbbat", partial.Message())

	other := Termination{Reason: TerminationGrantRevoked}
	assert.Equal(t, "grant_revoked: session terminated by dbbat", other.Message())
}
