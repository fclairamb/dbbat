//go:build integration

package mysql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// replaceGrantWithStatementTimeout revokes every active grant and issues one
// whose definition carries a per-statement limit of the given seconds.
func replaceGrantWithStatementTimeout(ctx context.Context, t *testing.T, f *fixture, seconds int64) {
	t.Helper()

	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(t, err)
	require.NotEmpty(t, grants)

	user, err := f.store.GetUserByUsername(ctx, fixtureUser)
	require.NoError(t, err)

	for _, g := range grants {
		require.NoError(t, f.store.RevokeGrant(ctx, g.UID, user.UID))
	}

	databases, err := f.store.ListServers(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, databases)

	_, err = testsupport.CreateGrantWithControls(ctx, t, f.store, user.UID, databases[0].UID,
		nil, testsupport.WithStatementTimeout(seconds))
	require.NoError(t, err)
}

// TestIntegration_StatementTimeout_MySQL is layer 1 on MySQL: the session is
// pinned with max_execution_time, so `SELECT SLEEP(5)` is cut by the server
// itself with error 3024 and the session survives.
func TestIntegration_StatementTimeout_MySQL(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	replaceGrantWithStatementTimeout(ctx, t, f, 1)

	db := f.dialTLS()
	defer func() { _ = db.Close() }()

	var slept int
	err := db.QueryRowContext(ctx, "SELECT SLEEP(5)").Scan(&slept)
	require.Error(t, err, "SLEEP(5) must not survive a 1s statement limit")

	// The session itself is still usable — which is the whole point of pinning
	// the server-side setting rather than relying on the watchdog alone.
	var got int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT 1").Scan(&got),
		"the session should survive a server-side statement timeout")
	assert.Equal(t, 1, got)
}

// TestIntegration_StatementTimeout_MariaDB is the same, on the dialect that
// spells the setting `max_statement_time` and measures it in seconds. It is a
// separate test because getting the dialect wrong fails the session at connect,
// which a MySQL-only run would never notice.
func TestIntegration_StatementTimeout_MariaDB(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mariadbImage(), store.ProtocolMariaDB)
	replaceGrantWithStatementTimeout(ctx, t, f, 1)

	db := f.dialTLS()
	defer func() { _ = db.Close() }()

	var slept int
	err := db.QueryRowContext(ctx, "SELECT SLEEP(5)").Scan(&slept)
	require.Error(t, err, "SLEEP(5) must not survive a 1s statement limit")

	var got int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT 1").Scan(&got),
		"the session should survive a server-side statement timeout")
	assert.Equal(t, 1, got)
}

// TestIntegration_StatementTimeout_RefusesRaising covers the courtesy refusals:
// the `SET`s that would drop the managed limit, and the optimizer hint that
// would raise it. A hint asking for *less* than the limit is fine — a client
// narrowing its own deadline is the behavior the limit encourages.
func TestIntegration_StatementTimeout_RefusesRaising(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	replaceGrantWithStatementTimeout(ctx, t, f, 30)

	db := f.dialTLS()
	defer func() { _ = db.Close() }()

	for _, sql := range []string{
		"SET SESSION max_execution_time = 0",
		"SET max_execution_time = 600000",
		"SET @@session.max_execution_time = 0",
		"SET SESSION max_statement_time = 0",
		"SELECT /*+ MAX_EXECUTION_TIME(600000) */ 1",
		// 0 is MySQL's "no limit", so it is the widest possible hint.
		"SELECT /*+ MAX_EXECUTION_TIME(0) */ 1",
	} {
		_, err := db.ExecContext(ctx, sql)
		require.Error(t, err, "%q should be refused", sql)
		assert.Contains(t, strings.ToLower(err.Error()), "managed by dbbat",
			"%q was refused with the wrong error: %v", sql, err)
	}

	// A hint below the limit is allowed.
	var got int
	require.NoError(t,
		db.QueryRowContext(ctx, "SELECT /*+ MAX_EXECUTION_TIME(500) */ 1").Scan(&got),
		"a hint tighter than the grant's limit is not a bypass")
	assert.Equal(t, 1, got)
}

// TestIntegration_StatementTimeout_WatchdogRecordsTermination proves the
// watchdog half on MySQL. max_execution_time covers only read-only SELECTs, so
// a statement it cannot touch — here a write that sleeps — is the case the
// watchdog exists for, and the one that leaves a termination record.
func TestIntegration_StatementTimeout_WatchdogRecordsTermination(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	replaceGrantWithStatementTimeout(ctx, t, f, 1)

	db := f.dialTLS()
	defer func() { _ = db.Close() }()

	var warmup int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT 1").Scan(&warmup))

	started := time.Now()

	// A write: outside what max_execution_time covers, so only dbbat's own
	// watchdog can end it.
	_, err := db.ExecContext(ctx, "DO SLEEP(30)")
	require.Error(t, err, "the watchdog should have ended the session")

	elapsed := time.Since(started)
	assert.Less(t, elapsed, 15*time.Second,
		"the watchdog took %s to fire on a 1s limit (expected ~3s)", elapsed)

	var terminated *store.Connection

	require.Eventually(t, func() bool {
		conns, listErr := f.store.ListConnections(ctx, store.ConnectionFilter{})
		if listErr != nil {
			return false
		}

		for i := range conns {
			if conns[i].TerminationReason != nil &&
				*conns[i].TerminationReason == store.TerminationStatementTimeout {
				terminated = &conns[i]

				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond,
		"no connection row carries termination_reason=statement_timeout")

	require.NotNil(t, terminated.DisconnectedAt, "a terminated connection must also be closed")

	require.Eventually(t, func() bool {
		eventType := store.AuditEventConnectionTerminated

		events, aErr := f.store.ListAuditEvents(ctx, store.AuditFilter{EventType: &eventType})
		if aErr != nil {
			return false
		}

		for i := range events {
			if strings.Contains(string(events[i].Details), terminated.UID.String()) {
				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond,
		"no connection.terminated audit entry was written")
}
