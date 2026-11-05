//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_PreAuthDeadlineSparesAuthenticatedIdleSession is the
// positive control for the pre-auth window: a session that logged in and then
// sits idle for longer than the window is not closed, because the deadline is
// lifted once dbbat hands the client its AUTH OK. The window still has to fit
// a real login (the pre-auth relay alone waits out a 750ms drain grace).
func TestIntegration_PreAuthDeadlineSparesAuthenticatedIdleSession(t *testing.T) {
	ctx := context.Background()
	f := startOracleThroughProxy(t, nil)

	const window = 3 * time.Second

	f.proxy.preAuth.Set(window)

	db, err := sql.Open("oracle", f.dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.PingContext(ctx))

	time.Sleep(2 * window)

	var one int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT 1 FROM DUAL").Scan(&one))
	assert.Equal(t, 1, one)
	assert.Len(t, f.proxy.preAuthRelays, 0, "a finished login gives its relay slot back")
}
