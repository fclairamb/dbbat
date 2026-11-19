//go:build integration

package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestIntegration_PreAuthDeadlineSparesAuthenticatedIdleSession is the
// positive control for the pre-auth window: a session that logged in and then
// sits idle for longer than the window is not closed, because the deadline is
// lifted once go-mysql's handshake returns. One pinned *sql.Conn, so the
// statement after the pause runs on the very session that idled.
func TestIntegration_PreAuthDeadlineSparesAuthenticatedIdleSession(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)

	const window = 500 * time.Millisecond

	f.proxy.preAuth.Set(window)

	db := f.dialTLS()
	defer db.Close()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.PingContext(ctx))

	time.Sleep(3 * window)

	var one int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT 1").Scan(&one))
	assert.Equal(t, 1, one)
}
