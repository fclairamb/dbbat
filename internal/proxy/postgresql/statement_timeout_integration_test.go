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
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/approval"
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

// ---------- Raw-protocol client ----------

// rawClient drives the proxy with hand-built pgproto3 frames: the only way to
// send the empty statement, Execute with a row limit (PortalSuspended), and a
// fully pipelined batch, none of which pgx will do on its own.
type rawClient struct {
	conn net.Conn
	fe   *pgproto3.Frontend
}

// mustRawConnect dials the proxy in plaintext, runs the cleartext-password
// handshake and waits for the first ReadyForQuery.
func mustRawConnect(ctx context.Context, t *testing.T, f *fixture) *rawClient {
	t.Helper()

	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", f.proxyAddr)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, conn.SetDeadline(time.Now().Add(60*time.Second)))

	fe := pgproto3.NewFrontend(conn, conn)

	fe.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{
			"user":     fixtureUser,
			"database": upstreamDB,
		},
	})
	require.NoError(t, fe.Flush())

	c := &rawClient{conn: conn, fe: fe}

	for {
		msg, rErr := fe.Receive()
		require.NoError(t, rErr, "handshake failed")

		switch m := msg.(type) {
		case *pgproto3.AuthenticationCleartextPassword:
			fe.Send(&pgproto3.PasswordMessage{Password: fixturePass})
			require.NoError(t, fe.Flush())
		case *pgproto3.ReadyForQuery:
			return c
		case *pgproto3.ErrorResponse:
			t.Fatalf("handshake refused: %s", m.Message)
		}
	}
}

// extended runs one extended-protocol batch (Parse/Bind/Execute/Sync) and
// returns the statement terminator the upstream answered with:
// CommandComplete, EmptyQueryResponse or PortalSuspended.
func (c *rawClient) extended(t *testing.T, name, sql string, maxRows uint32) pgproto3.BackendMessage {
	t.Helper()

	c.fe.Send(&pgproto3.Parse{Name: name, Query: sql})
	c.fe.Send(&pgproto3.Bind{DestinationPortal: "p_" + name, PreparedStatement: name})
	c.fe.Send(&pgproto3.Execute{Portal: "p_" + name, MaxRows: maxRows})
	c.fe.Send(&pgproto3.Sync{})
	require.NoError(t, c.fe.Flush())

	return c.untilReady(t)
}

// untilReady receives forwarded messages until the next ReadyForQuery,
// returning the last statement terminator seen on the way.
func (c *rawClient) untilReady(t *testing.T) pgproto3.BackendMessage {
	t.Helper()

	var terminator pgproto3.BackendMessage

	for {
		msg, err := c.fe.Receive()
		require.NoError(t, err, "no response from the proxy")

		switch m := msg.(type) {
		case *pgproto3.CommandComplete, *pgproto3.EmptyQueryResponse, *pgproto3.PortalSuspended:
			terminator = m
		case *pgproto3.ReadyForQuery:
			return terminator
		}
	}
}

// simple runs one simple-query statement and waits for its ReadyForQuery.
func (c *rawClient) simple(t *testing.T, sql string) pgproto3.BackendMessage {
	t.Helper()

	c.fe.Send(&pgproto3.Query{String: sql})
	require.NoError(t, c.fe.Flush())

	return c.untilReady(t)
}

// ---------- Integration tests ----------

// TestIntegration_StatementTimeout_DataGripSequencesKeepTheSessionAlive is the
// regression for the incident itself: a DataGrip-shaped session — the empty
// statement it always sends, then a result grid paged with Execute maxRows=501
// over a table with more rows than the page — under a 2s per-statement limit,
// then idle far past limit + grace + poll. Before the fix the paged grid and
// the empty statement each left a stale pending entry, the last statements
// were never completed, and the watchdog killed the idle session
// limit + 2s after its last statement.
func TestIntegration_StatementTimeout_DataGripSequencesKeepTheSessionAlive(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t)
	f.replaceGrantWithStatementTimeout(ctx, 2)

	// A table with more rows than a DataGrip page (501).
	seed := f.mustConnect(ctx, fixturePass)
	_, err := seed.Exec(ctx, "CREATE TABLE paged (id serial primary key)")
	require.NoError(t, err)

	_, err = seed.Exec(ctx, "INSERT INTO paged SELECT FROM generate_series(1, 600)")
	require.NoError(t, err)
	require.NoError(t, seed.Close(ctx))

	c := mustRawConnect(ctx, t, f)

	// The empty statement DataGrip sends right after it sets its
	// application_name, answered by the fourth terminator.
	term := c.extended(t, "empty", "", 0)
	require.IsType(t, &pgproto3.EmptyQueryResponse{}, term,
		"the empty statement must be answered by EmptyQueryResponse")

	// The paged catalog grid, answered by PortalSuspended.
	term = c.extended(t, "grid", "SELECT id FROM paged ORDER BY id LIMIT 600", 501)
	require.IsType(t, &pgproto3.PortalSuspended{}, term,
		"a 600-row table under a 501-row page must end with PortalSuspended")

	// Idle past limit + grace + poll: 2s limit, 2s grace, 250ms poll.
	time.Sleep(6 * time.Second)

	// The session is still alive and answers.
	term = c.simple(t, "SELECT 1")
	require.IsType(t, &pgproto3.CommandComplete{}, term)

	// And dbbat never recorded a statement_timeout termination for it: the
	// connection row is still open, with no reason on it.
	conns, listErr := f.store.ListConnections(ctx, store.ConnectionFilter{UserID: &f.user.UID})
	require.NoError(t, listErr)
	require.NotEmpty(t, conns)

	row := conns[0] // uid DESC: the latest session
	assert.Nil(t, row.TerminationReason,
		"an idle session must not be terminated: the paged grid and the empty statement must not park the clock")
	assert.Nil(t, row.DisconnectedAt, "the session must still be open")

	// The two statements were logged with their own durations, not shifted.
	queries, qErr := f.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &row.UID, Limit: 50})
	require.NoError(t, qErr)

	for _, q := range queries {
		if q.SQLText == "" || strings.Contains(q.SQLText, "FROM paged") {
			require.NotNil(t, q.DurationMs, "the %q row must be completed with its own duration", q.SQLText)
			assert.Less(t, *q.DurationMs, float64(1000), "its own duration, not a shifted one")
			assert.Nil(t, q.Error)
		}
	}
}

// TestIntegration_StatementTimeout_AttributesTheOldestInFlight pins the
// termination attribution end to end: two entries in flight (a held-then-
// approved pg_sleep, and a pipelined SELECT 1 whose batch the upstream has not
// reached), the watchdog fires — and the row completed with the limit text,
// and the connection.terminated audit entry, must name the OLDEST in-flight
// statement, not the newest. Before the fix the teardown blamed the newest.
func TestIntegration_StatementTimeout_AttributesTheOldestInFlight(t *testing.T) {
	ctx := context.Background()

	skipStatementTimeoutSetup.Store(true)
	t.Cleanup(func() { skipStatementTimeoutSetup.Store(false) })

	// An approval pattern so the oldest statement is persisted (with a uid)
	// before it runs: that is what lets the termination record and the audit
	// entry name it. The replaced grant carries the pattern AND the limit —
	// the fixture's own grant has the pattern but no limit.
	f := setupFixtureWith(ctx, t, fixtureOpts{approvalPatterns: []string{`^SELECT\s+pg_sleep`}})

	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(t, err)

	for _, g := range grants {
		require.NoError(t, f.store.RevokeGrant(ctx, g.UID, f.user.UID))
	}

	dbUID, err := uuid.Parse(f.dbUID)
	require.NoError(t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, t, f.store, f.user.UID, dbUID, []string{},
		testsupport.WithStatementTimeout(1), testsupport.WithApprovalPatterns(`^SELECT\s+pg_sleep`))
	require.NoError(t, err)

	c := mustRawConnect(ctx, t, f)

	// The warm-up proves the handshake and leaves the clock idle before the
	// two in-flight statements are queued.
	term := c.simple(t, "SELECT 1")
	require.IsType(t, &pgproto3.CommandComplete{}, term)

	// The oldest statement: pg_sleep(30), parked on the hold.
	c.sendBatch(t,
		&pgproto3.Parse{Name: "sleep", Query: "SELECT pg_sleep(30)"},
		&pgproto3.Bind{DestinationPortal: "p_sleep", PreparedStatement: "sleep"},
		&pgproto3.Execute{Portal: "p_sleep"},
		&pgproto3.Sync{},
	)

	// The hold: a pending row persisted before the statement ever ran.
	var held store.Query

	require.Eventually(t, func() bool {
		queries, qErr := f.store.ListQueries(ctx, store.QueryFilter{Limit: 100})
		if qErr != nil {
			return false
		}

		for i := range queries {
			if queries[i].ApprovalStatus != nil && *queries[i].ApprovalStatus == store.ApprovalPending {
				held = queries[i]

				return true
			}
		}

		return false
	}, 15*time.Second, 100*time.Millisecond, "the pg_sleep statement was never parked on the hold")

	// Release the hold as a second human would.
	by := f.user.UID
	f.approvals.Resolve(approval.Decision{
		QueryUID: held.UID,
		Status:   store.ApprovalApproved,
		By:       &by,
		ByName:   fixtureUser,
	})

	// The second statement, pipelined right behind batch 1: its Execute is
	// queued before batch 1's terminator ever arrives (the upstream is still
	// in pg_sleep), so two entries are in flight when the watchdog fires.
	c.sendBatch(t,
		&pgproto3.Parse{Name: "sel", Query: "SELECT 1"},
		&pgproto3.Bind{DestinationPortal: "p_sel", PreparedStatement: "sel"},
		&pgproto3.Execute{Portal: "p_sel"},
		&pgproto3.Sync{},
	)

	// The watchdog trips at limit + grace: ~3s after the hold released.
	var terminated *store.Connection

	require.Eventually(t, func() bool {
		conns, listErr := f.store.ListConnections(ctx, store.ConnectionFilter{UserID: &f.user.UID})
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
	}, 20*time.Second, 250*time.Millisecond,
		"the session was not terminated by the statement-timeout watchdog")

	// The row completed with the limit text is the OLDEST statement — the one
	// the clock measured — not the newest.
	var oldestRow *store.Query

	require.Eventually(t, func() bool {
		queries, qErr := f.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &terminated.UID, Limit: 50})
		if qErr != nil {
			return false
		}

		for i := range queries {
			if queries[i].Error != nil && strings.Contains(*queries[i].Error, "statement timeout") {
				row := queries[i]
				oldestRow = &row

				return true
			}
		}

		return false
	}, 15*time.Second, 250*time.Millisecond,
		"no query row records the statement that was cut off")

	assert.Equal(t, held.SQLText, oldestRow.SQLText,
		"the limit text must land on the oldest in-flight statement, not on the newest")
	assert.Equal(t, held.UID, oldestRow.UID)

	// And the other in-flight entry was completed as aborted, without the
	// limit text, so nothing reads as still running.
	require.Eventually(t, func() bool {
		queries, qErr := f.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &terminated.UID, Limit: 50})
		if qErr != nil {
			return false
		}

		for i := range queries {
			if queries[i].SQLText == "SELECT 1" {
				if queries[i].Error == nil {
					return false
				}

				assert.NotContains(t, *queries[i].Error, "statement timeout",
					"the aborted entry carries no limit text")

				return true
			}
		}

		return false
	}, 15*time.Second, 250*time.Millisecond, "the pipelined SELECT 1 was never completed")

	// The chained audit entry names the oldest statement too, via the
	// termination record's QueryUID — which is what the Slack payload reads.
	require.Eventually(t, func() bool {
		eventType := store.AuditEventConnectionTerminated

		events, aErr := f.store.ListAuditEvents(ctx, store.AuditFilter{EventType: &eventType})
		if aErr != nil {
			return false
		}

		for i := range events {
			if strings.Contains(string(events[i].Details), terminated.UID.String()) &&
				strings.Contains(string(events[i].Details), held.UID.String()) {
				return true
			}
		}

		return false
	}, 15*time.Second, 250*time.Millisecond,
		"the connection.terminated audit entry does not name the oldest in-flight statement")
}

// sendBatch flushes a whole client-side batch in one write, the way a
// pipelining driver (pgx, pgjdbc) does.
func (c *rawClient) sendBatch(t *testing.T, msgs ...pgproto3.FrontendMessage) {
	t.Helper()

	for _, m := range msgs {
		c.fe.Send(m)
	}

	require.NoError(t, c.fe.Flush())
}
