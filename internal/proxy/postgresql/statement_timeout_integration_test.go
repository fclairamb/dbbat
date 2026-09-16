//go:build integration

package postgresql

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// replaceGrantWithStatementTimeout revokes the fixture's grants and installs
// one whose definition carries a per-statement limit of the given seconds.
func (f *fixture) replaceGrantWithStatementTimeout(ctx context.Context, seconds int64) {
	f.t.Helper()

	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(f.t, err)

	for _, g := range grants {
		require.NoError(f.t, f.store.RevokeGrant(ctx, g.UID, f.user.UID))
	}

	dbUID, err := uuid.Parse(f.dbUID)
	require.NoError(f.t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, f.t, f.store, f.user.UID, dbUID,
		[]string{}, testsupport.WithStatementTimeout(seconds))
	require.NoError(f.t, err)
}

// TestIntegration_StatementTimeout_ServerSideCancel is layer 1: the statement
// is canceled by PostgreSQL itself, with a real SQLSTATE, and **the session
// survives**. That survival is the entire reason the server-side SET exists —
// the watchdog behind it would have taken the connection with it.
func TestIntegration_StatementTimeout_ServerSideCancel(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)
	f.replaceGrantWithStatementTimeout(ctx, 1)

	conn := f.mustConnect(ctx, fixturePass)

	_, err := conn.Exec(ctx, "SELECT pg_sleep(5)")
	require.Error(t, err, "pg_sleep(5) should not survive a 1s statement limit")

	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a PostgreSQL error, got %T: %v", err, err)
	assert.Equal(t, "57014", pgErr.Code, "expected query_canceled (57014), got %s: %s", pgErr.Code, pgErr.Message)

	// The session is still usable: only the statement was canceled.
	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got),
		"the session should survive a server-side statement timeout")
	assert.Equal(t, 1, got)
}

// TestIntegration_StatementTimeout_RefusesUnset covers the courtesy refusal:
// a client that tries to widen or drop the managed limit is told so, by name,
// rather than silently meeting the watchdog later with no explanation.
func TestIntegration_StatementTimeout_RefusesUnset(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)
	f.replaceGrantWithStatementTimeout(ctx, 30)

	conn := f.mustConnect(ctx, fixturePass)

	for _, sql := range []string{
		"SET statement_timeout = 0",
		"SET SESSION statement_timeout TO DEFAULT",
		"RESET statement_timeout",
		"RESET ALL",
		// Comment-normalized: this reaches the server as a plain SET.
		"SET/**/statement_timeout = 0",
	} {
		_, err := conn.Exec(ctx, sql)
		require.Error(t, err, "%q should be refused", sql)
		assert.Contains(t, strings.ToLower(err.Error()), "managed by dbbat",
			"%q was refused with the wrong error: %v", sql, err)
	}

	// Reading the limit is the opposite of bypassing it, and stays allowed.
	var value string
	require.NoError(t, conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&value))
	assert.NotEmpty(t, value)

	// A definition with no limit has nothing to bypass, so the same statement
	// is an ordinary client setting again.
	f.replaceGrantWithStatementTimeout(ctx, 0)

	unlimited, err := f.connect(ctx, fixtureUser, fixturePass)
	require.NoError(t, err)

	defer func() { _ = unlimited.Close(context.Background()) }()

	_, err = unlimited.Exec(ctx, "SET statement_timeout = 0")
	assert.NoError(t, err, "with no limit configured, SET statement_timeout is not a bypass")
}

// TestIntegration_StatementTimeout_WatchdogTerminates is layer 2, with layer 1
// deliberately switched off: it proves the watchdog alone ends the statement,
// cancels it upstream (pg_stat_activity no longer shows the backend), and
// leaves the whole paper trail — the connection's termination_reason, the query
// row's error, and the chained audit entry.
func TestIntegration_StatementTimeout_WatchdogTerminates(t *testing.T) {
	ctx := context.Background()

	skipStatementTimeoutSetup.Store(true)
	t.Cleanup(func() { skipStatementTimeoutSetup.Store(false) })

	f := setupFixture(ctx, t)
	f.replaceGrantWithStatementTimeout(ctx, 1)

	conn := f.mustConnect(ctx, fixturePass)

	var warmup int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&warmup))

	started := time.Now()

	_, err := conn.Exec(ctx, "SELECT pg_sleep(30)")
	require.Error(t, err, "the watchdog should have ended the session")

	elapsed := time.Since(started)
	assert.Less(t, elapsed, 10*time.Second,
		"the watchdog took %s to fire on a 1s limit (expected ~3s)", elapsed)

	// The backend is gone upstream: the CancelRequest landed, rather than the
	// server finishing the sleep for nobody.
	requireNoUpstreamSleep(ctx, t, f)

	// The session's paper trail. The teardown writes it asynchronously, so poll
	// rather than assuming the close has landed by the time Exec returned.
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
	}, 15*time.Second, 250*time.Millisecond,
		"no connection row carries termination_reason=statement_timeout")

	require.NotNil(t, terminated.DisconnectedAt,
		"a terminated connection must also be closed")

	// The statement that caused it is completed, with an error naming the limit
	// — this is the row an operator looks for afterwards.
	require.Eventually(t, func() bool {
		queries, qErr := f.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &terminated.UID})
		if qErr != nil {
			return false
		}

		for i := range queries {
			if queries[i].Error == nil {
				continue
			}

			if strings.Contains(*queries[i].Error, "statement timeout") &&
				strings.Contains(*queries[i].Error, "terminated by dbbat") {
				return true
			}
		}

		return false
	}, 15*time.Second, 250*time.Millisecond,
		"no query row records the statement that was cut off")

	// And the chained audit entry, which survives even a DELETE of the
	// connection row.
	require.Eventually(t, func() bool {
		eventType := store.AuditEventConnectionTerminated

		events, aErr := f.store.ListAuditEvents(ctx, store.AuditFilter{EventType: &eventType})
		if aErr != nil {
			return false
		}

		for i := range events {
			if strings.Contains(string(events[i].Details), terminated.UID.String()) &&
				strings.Contains(string(events[i].Details), store.TerminationStatementTimeout) {
				return true
			}
		}

		return false
	}, 15*time.Second, 250*time.Millisecond,
		"no connection.terminated audit entry was written")
}

// requireNoUpstreamSleep asserts the upstream has no pg_sleep backend left
// running — i.e. the CancelRequest actually reached it. Checked on a direct
// connection to the upstream, not through the proxy, because the proxy session
// is exactly what just went away.
func requireNoUpstreamSleep(ctx context.Context, t *testing.T, f *fixture) {
	t.Helper()

	admin, err := pgconn.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		upstreamUsr, upstreamPwd, net.JoinHostPort(f.upstreamHost, strconv.Itoa(f.upstreamPort)), upstreamDB))
	require.NoError(t, err)

	defer func() { _ = admin.Close(context.Background()) }()

	require.Eventually(t, func() bool {
		result := admin.ExecParams(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE query LIKE '%pg_sleep%' AND state = 'active'",
			nil, nil, nil, nil).Read()
		if result.Err != nil {
			return false
		}

		if len(result.Rows) == 0 || len(result.Rows[0]) == 0 {
			return false
		}

		return string(result.Rows[0][0]) == "0"
	}, 15*time.Second, 500*time.Millisecond,
		"the upstream is still running pg_sleep: the cancel never landed")
}
