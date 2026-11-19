//go:build integration

package postgresql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_PreAuthDeadlineSparesAuthenticatedIdleSession is the
// positive control for the pre-auth window: a session that logged in and then
// sits idle for longer than the window is not closed, because the deadline is
// lifted on authentication.
func TestIntegration_PreAuthDeadlineSparesAuthenticatedIdleSession(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	const window = 500 * time.Millisecond

	f.proxy.preAuth.Set(window)

	conn := f.mustConnect(ctx, fixturePass)

	time.Sleep(3 * window)

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got))
	assert.Equal(t, 1, got)
}
