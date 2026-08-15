//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	go_ora "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// oracleThroughProxy stands up the whole chain — Oracle container, PostgreSQL
// store, dbbat Oracle proxy — and hands back a go-ora `*sql.DB` pointed at the
// proxy, plus the handler counting what the proxy logged.
type oracleThroughProxy struct {
	db   *sql.DB
	logs *countingHandler

	// The same coordinates, for a client that is not go-ora.
	host     string
	port     int
	service  string
	username string
	apiKey   string

	// dsn is the go-ora URL the client above was opened with; newClient reuses
	// it to get a *fresh* session, which is the only way to pick up a grant
	// swapped out from under the fixture (the grant is resolved once, at auth).
	dsn string

	// The store behind the proxy, and who the fixture is: the blocked-statement
	// assertions read `queries` rows back and walk the connection's HMAC chain.
	store *store.Store
	user  *store.User
	dbUID uuid.UUID

	// oracle is the upstream container itself. It is kept rather than dropped
	// into a cleanup because the container-hosted OCI client runs sqlplus
	// *inside* it — see oci_client_integration_test.go.
	oracle testcontainers.Container
}

// oracleFixtureOptions are the knobs startOracleThroughProxyWith takes. The
// zero value is what every test had before the container-hosted OCI client
// existed: no controls on the grant, proxy bound on loopback only.
type oracleFixtureOptions struct {
	// controls are the grant controls the fixture user is issued.
	controls []string

	// reachableFromContainers binds the proxy on every interface instead of
	// loopback, and gives the Oracle container a route back to the host, so a
	// client running inside that container can dial the proxy. Off by default:
	// binding a test listener on 0.0.0.0 is a real (if brief) exposure and, on
	// macOS with the firewall on, an interactive prompt.
	reachableFromContainers bool
}

func startOracleThroughProxy(t *testing.T, controls []string) *oracleThroughProxy {
	t.Helper()

	return startOracleThroughProxyWith(t, oracleFixtureOptions{controls: controls})
}

func startOracleThroughProxyWith(t *testing.T, opts oracleFixtureOptions) *oracleThroughProxy {
	t.Helper()

	ctx := context.Background()
	controls := opts.controls

	oracleContainer, oracleHost, oraclePort := startOracleContainerWith(t, opts.reachableFromContainers)
	t.Cleanup(func() { _ = oracleContainer.Terminate(ctx) })

	pgContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:15-alpine",
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_DB":       "dbbat_test",
				"POSTGRES_USER":     "test",
				"POSTGRES_PASSWORD": "test",
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	pgHost, _ := pgContainer.Host(ctx)
	pgPort, _ := pgContainer.MappedPort(ctx, "5432")
	pgDSN := "postgres://test:test@" + net.JoinHostPort(pgHost, pgPort.Port()) + "/dbbat_test?sslmode=disable"

	encryptionKey := []byte("0123456789012345678901234567890X")

	// Handing the master key to the store is what makes the tamper-evident
	// query chain active, exactly as a serving process always has it. Without
	// it every proxied query would be written unchained and the chain
	// assertions in the blocked-statement tests would pass on nothing.
	dataStore, err := store.New(ctx, pgDSN, store.Options{EncryptionKey: encryptionKey})
	require.NoError(t, err)

	t.Cleanup(func() { dataStore.Close() })
	require.NoError(t, dataStore.Migrate(ctx))
	require.True(t, dataStore.ChainEnabled(), "the fixture store must chain its query rows")

	// Lowercase: Oracle clients uppercase the username on the wire and the proxy
	// lowercases it again before the dbbat lookup.
	user, err := dataStore.CreateUser(ctx, "cursorprobe", "$argon2id$v=19$m=4096,t=3,p=1$salt$hash", []string{"connector"})
	require.NoError(t, err)

	// service is the upstream Oracle service name (XEPDB1/FREEPDB1/ORCLPDB1,
	// whichever the running image serves) — never a slug, so it cannot also be
	// the dbbat entry's Name any more. Every client fixture below connects
	// using `service` (the raw upstream name, in ociConnectStringAt and every
	// go_ora.BuildUrl call), which resolveDatabase resolves through its
	// OracleServiceName fallback (an exact GetServerByName miss on the slug
	// name, then a single-candidate ListServersByOracleServiceName hit) — the
	// same mutualized-instance path a real client hitting a shared upstream
	// service takes, and a closer match to production than the old
	// Name-equals-service coincidence that skipped it entirely.
	service := oracleTestService()
	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:              "oracle_e2e",
		Host:              oracleHost,
		Port:              oraclePort,
		DatabaseName:      service,
		OracleServiceName: &service,
		Username:          "system",
		Password:          "oracle",
		Protocol:          store.ProtocolOracle,
	}, encryptionKey)
	require.NoError(t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, t, dataStore, user.UID, db.UID, controls)
	require.NoError(t, err)

	// Oracle clients authenticate to dbbat with an API key as the password.
	_, plainKey, err := dataStore.CreateAPIKey(ctx, user.UID, "cursorprobe-key", nil, encryptionKey)
	require.NoError(t, err)

	logs := newCountingHandler()

	bindAddr := "127.0.0.1:0"
	if opts.reachableFromContainers {
		bindAddr = "0.0.0.0:0"
	}

	proxy := NewServer(dataStore, encryptionKey, nil, config.QueryStorageConfig{}, config.DumpConfig{}, slog.New(logs))
	go func() { _ = proxy.Start(bindAddr) }()

	// Bounded, like the PostgreSQL fixture's: Shutdown waits for every live
	// session's goroutine, and a session parked on a client that will never
	// send another byte — which is what a refused statement leaves behind
	// today, see the blocked-statement test — would otherwise hang teardown
	// until the whole suite's timeout.
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = proxy.Shutdown(shutdownCtx)
	})

	require.Eventually(t, func() bool { return proxy.Addr() != nil }, 5*time.Second, 50*time.Millisecond)

	host, portStr, err := net.SplitHostPort(proxy.Addr().String())
	require.NoError(t, err)

	// A wildcard bind reports 0.0.0.0, which is not an address a client should
	// be handed; every in-process client still goes to loopback.
	if host == "0.0.0.0" {
		host = "127.0.0.1"
	}

	port := 0
	_, err = fmt.Sscanf(portStr, "%d", &port)
	require.NoError(t, err)

	dsn := go_ora.BuildUrl(host, port, service, user.Username, plainKey, nil)

	client, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	// One connection, so every cursor below lives on a single dbbat session.
	client.SetMaxOpenConns(1)
	client.SetMaxIdleConns(1)

	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, client.PingContext(ctx))

	return &oracleThroughProxy{
		db:       client,
		logs:     logs,
		host:     host,
		port:     port,
		service:  service,
		username: user.Username,
		apiKey:   plainKey,
		dsn:      dsn,
		store:    dataStore,
		user:     user,
		dbUID:    db.UID,
		oracle:   oracleContainer,
	}
}

// newClient opens a *second* go-ora connection through the proxy. A session
// resolves its grant once, at authentication, so anything that swaps the grant
// out (replaceGrant) only reaches a connection opened afterwards.
func (e *oracleThroughProxy) newClient(t *testing.T) *sql.DB {
	t.Helper()

	client, err := sql.Open("oracle", e.dsn)
	require.NoError(t, err)

	client.SetMaxOpenConns(1)
	client.SetMaxIdleConns(1)

	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, client.PingContext(context.Background()))

	return client
}

// replaceGrant revokes every live grant of the fixture user and issues one
// carrying the given controls. The same shape as the PostgreSQL fixture's
// helper, so the two suites narrow a grant the same way.
func (e *oracleThroughProxy) replaceGrant(t *testing.T, controls []string, opts ...testsupport.GrantOption) {
	t.Helper()

	e.revokeAllGrants(t)

	_, err := testsupport.CreateGrantWithControls(context.Background(), t, e.store,
		e.user.UID, e.dbUID, controls, opts...)
	require.NoError(t, err)
}

// revokeAllGrants revokes every live grant of the fixture user in the store and
// issues nothing in its place. It leaves sessions that are already open alone —
// which is what replaceGrant has always wanted, since a session resolves its
// grant once and the point of swapping is the *next* connection.
func (e *oracleThroughProxy) revokeAllGrants(t *testing.T) []uuid.UUID {
	t.Helper()

	ctx := context.Background()

	grants, err := e.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(t, err)

	uids := make([]uuid.UUID, 0, len(grants))

	for _, g := range grants {
		require.NoError(t, e.store.RevokeGrant(ctx, g.UID, e.user.UID))

		uids = append(uids, g.UID)
	}

	return uids
}

// revokeAllGrantsLive is revokeAllGrants plus the signal that reaches
// connections which are already open: the store row alone is inert, and it is
// the API handler (internal/api/grants.go) that pokes the RevocationRegistry
// afterwards, flipping the flag every live session's limit watchdog polls.
//
// The one measurement that needs it is the idle-revocation case — the refusal
// path that fires with no call in flight — so the fixture reproduces what the
// REST endpoint does rather than what the store alone does.
func (e *oracleThroughProxy) revokeAllGrantsLive(t *testing.T) {
	t.Helper()

	for _, uid := range e.revokeAllGrants(t) {
		e.store.Revocations().Revoke(uid)
	}
}

// runCursorWorkloads drives every shape the spec named as a stress on cursor-id
// learning: a prepared SELECT loop, bind-heavy re-executions, several cursors
// open at once, DML, an anonymous PL/SQL block, a REF cursor, and a statement
// cache churned past anything the proxy would keep. It returns the statements it
// expects to see re-executed by cursor id.
func runCursorWorkloads(t *testing.T, db *sql.DB) []string {
	t.Helper()

	ctx := context.Background()

	const reexecRuns = 5

	var expected []string

	exec := func(query string, args ...any) {
		t.Helper()

		_, err := db.ExecContext(ctx, query, args...)
		require.NoErrorf(t, err, "exec %q", query)
	}

	_, _ = db.ExecContext(ctx, "DROP TABLE dbbat_learn_probe")
	exec("CREATE TABLE dbbat_learn_probe (id NUMBER, label VARCHAR2(50))")

	t.Cleanup(func() { _, _ = db.ExecContext(ctx, "DROP TABLE dbbat_learn_probe") })

	// 1. The plain prepared-SELECT loop — the shape a `cur.execute()` loop sends.
	t.Run("prepared select loop", func(t *testing.T) {
		stmt, err := db.PrepareContext(ctx, "SELECT 1 AS n FROM dual")
		require.NoError(t, err)

		defer func() { _ = stmt.Close() }()

		for i := 0; i < reexecRuns; i++ {
			var n int
			require.NoError(t, stmt.QueryRowContext(ctx).Scan(&n))
		}
	})

	expected = append(expected, "SELECT 1 AS n FROM dual")

	// 2. Bind-heavy: the binds change on every run, the cursor does not.
	t.Run("bind heavy", func(t *testing.T) {
		q := "SELECT :1 AS a, :2 AS b, :3 AS c, :4 AS d, :5 AS e FROM dual"

		stmt, err := db.PrepareContext(ctx, q)
		require.NoError(t, err)

		defer func() { _ = stmt.Close() }()

		for i := 0; i < reexecRuns; i++ {
			var a, b, c, d, e int
			require.NoError(t, stmt.QueryRowContext(ctx, i, i+1, i+2, i+3, i+4).Scan(&a, &b, &c, &d, &e))
		}
	})

	expected = append(expected, "SELECT :1 AS a, :2 AS b, :3 AS c, :4 AS d, :5 AS e FROM dual")

	// 3. Several cursors open on one session, executions interleaved.
	t.Run("concurrent cursors", func(t *testing.T) {
		queries := []string{
			"SELECT 10 AS n FROM dual",
			"SELECT 20 AS n FROM dual",
			"SELECT 30 AS n FROM dual",
		}

		stmts := make([]*sql.Stmt, len(queries))

		for i, q := range queries {
			stmt, err := db.PrepareContext(ctx, q)
			require.NoError(t, err)

			stmts[i] = stmt

			defer func() { _ = stmt.Close() }()
		}

		for run := 0; run < reexecRuns; run++ {
			for _, stmt := range stmts {
				var n int
				require.NoError(t, stmt.QueryRowContext(ctx).Scan(&n))
			}
		}

		expected = append(expected, queries...)
	})

	// 4. DML — the other piggyback sub-op (0x04 rather than 0x4e).
	t.Run("prepared dml", func(t *testing.T) {
		stmt, err := db.PrepareContext(ctx, "INSERT INTO dbbat_learn_probe VALUES (:1, :2)")
		require.NoError(t, err)

		defer func() { _ = stmt.Close() }()

		for i := 0; i < reexecRuns; i++ {
			_, err := stmt.ExecContext(ctx, i, fmt.Sprintf("row-%d", i))
			require.NoError(t, err)
		}
	})

	expected = append(expected, "INSERT INTO dbbat_learn_probe VALUES (:1, :2)")

	// 5. An anonymous PL/SQL block, re-executed.
	t.Run("plsql block", func(t *testing.T) {
		block := "BEGIN INSERT INTO dbbat_learn_probe VALUES (:1, 'plsql'); END;"

		stmt, err := db.PrepareContext(ctx, block)
		require.NoError(t, err)

		defer func() { _ = stmt.Close() }()

		for i := 0; i < reexecRuns; i++ {
			_, err := stmt.ExecContext(ctx, 1000+i)
			require.NoError(t, err)
		}

		expected = append(expected, block)
	})

	// 6. A REF cursor: the server opens a cursor dbbat never saw parsed, and the
	//    client then fetches from it by id alone.
	t.Run("ref cursor", func(t *testing.T) {
		exec(`CREATE OR REPLACE PROCEDURE dbbat_learn_refcur(p OUT SYS_REFCURSOR) AS
BEGIN
  OPEN p FOR SELECT LEVEL AS n FROM dual CONNECT BY LEVEL <= 5;
END;`)

		t.Cleanup(func() { _, _ = db.ExecContext(ctx, "DROP PROCEDURE dbbat_learn_refcur") })

		call := "BEGIN dbbat_learn_refcur(:1); END;"

		stmt, err := db.PrepareContext(ctx, call)
		require.NoError(t, err)

		defer func() { _ = stmt.Close() }()

		for i := 0; i < reexecRuns; i++ {
			var cursor go_ora.RefCursor

			_, err := stmt.ExecContext(ctx, go_ora.Out{Dest: &cursor})
			require.NoError(t, err)

			ds, err := cursor.Query()
			require.NoError(t, err)

			row := make([]driver.Value, len(ds.Columns()))
			for ds.Next(row) == nil {
				continue
			}

			require.NoError(t, ds.Close())
			require.NoError(t, cursor.Close())
		}

		expected = append(expected, call)
	})

	// 7. A statement that fails, retried. This is the one shape the measurement
	//    flagged as never yielding a cursor id: findCursorIDInResponse refuses to
	//    read an id off an OER carrying a real ORA code. If a client re-executed
	//    such a cursor by id, that would be a learning miss with teeth.
	t.Run("failing statement retried", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			rows, err := db.QueryContext(ctx, "SELECT * FROM dbbat_no_such_table_at_all")
			require.Error(t, err, "the statement must really fail")

			if rows != nil {
				_ = rows.Close()
			}
		}
	})

	// 8. Statement-cache churn: enough distinct statements to push the earlier
	//    ones out of any cache, then the originals again.
	t.Run("statement cache churn", func(t *testing.T) {
		for i := 0; i < 40; i++ {
			q := fmt.Sprintf("SELECT %d AS churn FROM dual", i)

			stmt, err := db.PrepareContext(ctx, q)
			require.NoError(t, err)

			for run := 0; run < 2; run++ {
				var n int
				require.NoError(t, stmt.QueryRowContext(ctx).Scan(&n))
			}

			require.NoError(t, stmt.Close())

			expected = append(expected, q)
		}

		// And back to the first statement, long after its cursor was churned.
		stmt, err := db.PrepareContext(ctx, "SELECT 1 AS n FROM dual")
		require.NoError(t, err)

		defer func() { _ = stmt.Close() }()

		for run := 0; run < reexecRuns; run++ {
			var n int
			require.NoError(t, stmt.QueryRowContext(ctx).Scan(&n))
		}
	})

	return expected
}

// TestIntegration_CursorIDLearningMissRate is step 1 of
// specs/todos/2026-08-09-oracle-piggyback-reexec-unknown-cursor.md: before the
// piggyback path can be made to fail closed on an untracked cursor, the cost of
// doing so has to be known, and that cost is exactly how often dbbat fails to
// learn the cursor id the server assigned.
//
// Every re-execution of a cursor dbbat did not learn is a statement that would
// become ORA-01031 under a restrictive grant. The measurement therefore counts,
// against a real Oracle over the real proxy:
//
//   - parses seen (each one is a chance to learn a cursor id),
//   - cursor ids actually learned,
//   - re-executions resolved to their statement,
//   - re-executions naming a cursor the tracker did not hold.
//
// It also checks every resolved re-execution against the set of statements the
// client actually ran, because an anchored scan that learns the *wrong* id is a
// worse failure than one that learns none.
func TestIntegration_CursorIDLearningMissRate(t *testing.T) {
	env := startOracleThroughProxy(t, nil)

	expected := runCursorWorkloads(t, env.db)

	// Give the response leg time to finish draining before reading the counters.
	time.Sleep(2 * time.Second)

	var (
		parses    = env.logs.count(logMsgQueryIntercepted)
		learned   = env.logs.count(logMsgLearnedCursorID)
		resolved  = env.logs.count(logMsgReexecGated)
		untracked = env.logs.count(logMsgUntrackedCursorForwarded)
		refused   = env.logs.count(logMsgUntrackedCursorRefused)
	)

	reexecs := resolved + untracked

	t.Logf("cursor-id learning measurement (image=%s):", oracleTestImage())
	t.Logf("  parses seen (query intercepted):      %d", parses)
	t.Logf("  cursor ids learned:                   %d", learned)
	t.Logf("  re-executions resolved to their SQL:  %d", resolved)
	t.Logf("  re-executions naming an unknown id:   %d", untracked)
	t.Logf("  re-executions refused:                %d", refused)

	if reexecs > 0 {
		t.Logf("  learning miss rate:                   %d/%d", untracked, reexecs)
	}

	// Which parses never yielded a cursor id. Only the statements that *failed*
	// belong here: their OER carries a real ORA code, which
	// findCursorIDInResponse deliberately refuses to read an id from, and no
	// client re-executes a statement that errored.
	unlearned := multisetDiff(env.logs.sqlsFor(logMsgQueryIntercepted),
		env.logs.sqlsFor(logMsgLearnedCursorID))
	if len(unlearned) > 0 {
		t.Logf("  parses with no cursor id learned:     %s", strings.Join(unlearned, " | "))
	}

	if os.Getenv("CURSOR_TRACE") != "" {
		for i, l := range env.logs.traceLines() {
			t.Logf("TRACE %03d %s", i, l)
		}
	}

	assert.Positive(t, parses, "the workloads must have reached the proxy at all")
	assert.Positive(t, reexecs, "the workloads must have produced cursor re-executions")

	// The claim under test: no re-execution of a statement this client parsed
	// through the proxy ever names a cursor dbbat could not resolve.
	assert.Zero(t, untracked,
		"cursor-id learning missed %d of %d re-executions; failing the piggyback path closed would "+
			"turn each of those into ORA-01031", untracked, reexecs)

	// The sharper claim, and the one that catches a learning miss even when a
	// recycled cursor id hides it behind a stale tracker entry: every statement
	// that *succeeded* got its cursor id learned. Anything else in this list is
	// a regression in findCursorIDInResponse — it is how the end-to-end ECID
	// sequence bound (oerMaxSeqNumber) was caught silently switching learning
	// off part-way through every session.
	assert.Equal(t,
		[]string{
			"DROP TABLE dbbat_learn_probe",
			"SELECT * FROM dbbat_no_such_table_at_all",
			"SELECT * FROM dbbat_no_such_table_at_all",
			"SELECT * FROM dbbat_no_such_table_at_all",
		},
		unlearned,
		"only the statements that failed may go without a learned cursor id")

	// And the ids it did learn point at the right statements.
	want := make(map[string]bool, len(expected))
	for _, q := range expected {
		want[truncateSQL(q, 200)] = true
	}

	var unexpected []string

	histogram := make(map[string]int)

	for _, got := range env.logs.sqlsFor(logMsgReexecGated) {
		histogram[got]++

		if !want[got] {
			unexpected = append(unexpected, got)
		}
	}

	sort.Strings(unexpected)
	assert.Emptyf(t, unexpected,
		"a re-execution resolved to a statement this client never ran — the cursor id was mis-learned: %s",
		strings.Join(unexpected, " | "))

	// The three interleaved cursors ran the same number of times each, so their
	// re-execution counts must come out equal. A cursor id learned off the wrong
	// OER would show up here as a skew long before it showed up as an unknown
	// statement — this is the mis-learning check the set membership above cannot
	// make.
	symmetric := []string{
		"SELECT 10 AS n FROM dual",
		"SELECT 20 AS n FROM dual",
		"SELECT 30 AS n FROM dual",
	}

	for _, q := range symmetric {
		t.Logf("  re-executions of %-32s %d", q+":", histogram[q])
		assert.Positivef(t, histogram[q], "%q was re-executed but never resolved", q)
		assert.Equalf(t, histogram[symmetric[0]], histogram[q],
			"three interleaved cursors ran equally often; %q did not", q)
	}

	assertTrackerStaysBounded(t, env, parses)
}

// trackerPeakBound is the ceiling the cursor tracker must stay under across the
// whole workload above.
//
// It is chosen against the statement-cache-churn shape, which parses 40
// distinct statements one after another and closes each cursor before opening
// the next: with the close list decoded, the tracker holds a handful of live
// cursors at a time; with it un-decoded it grows monotonically past 40, which
// is the leak this bound is here to catch. Anything between is a partial
// regression and should fail too, so the bound sits well below 40 rather than
// just under it.
//
// Measured on gvenzl/oracle-free:23-slim: 66 close-cursors frames decoded, a
// peak of **3** cursors tracked at once and 1 left at the end. The headroom is
// for the other images CI runs (18c XE) and for clients that batch differently,
// not because 20 is expected.
const trackerPeakBound = 20

// assertTrackerStaysBounded is the standing answer to the question the spec
// asked and deliberately answered "no" to: does s.tracker.cursors need a cap?
//
// It does not — a client that opens cursors also closes them, and the leftover
// entries the original measurement saw were batched closes dbbat could not
// read, not genuine leaks. But "no cap" is only defensible if the absence of
// growth is actually checked, and a cap would have been the *wrong* fix: every
// evicted entry turns a correctly-gated re-execution into a refusal. So the
// bound is asserted here instead, on a workload that opens and closes 40
// cursors on one session.
//
// The tracker lives inside a session goroutine, so its size is read back off
// the record handleCloseCursors emits — which
// TestHandleCloseCursorsReportsTheTrackerSize keeps honest.
func assertTrackerStaysBounded(t *testing.T, env *oracleThroughProxy, parses int) {
	t.Helper()

	closes := env.logs.count(logMsgCursorsClosed)

	peak, seen := env.logs.maxIntFor(logMsgCursorsClosed, "tracked")
	tracked := env.logs.intsFor(logMsgCursorsClosed, "tracked")

	t.Logf("  close-cursors frames decoded:         %d", closes)
	t.Logf("  peak cursors tracked at once:         %d", peak)

	if len(tracked) > 0 {
		t.Logf("  cursors tracked after the last close: %d", tracked[len(tracked)-1])
	}

	require.True(t, seen,
		"the workload closed its cursors, so the proxy must have decoded at least one close list; "+
			"a run with none would make every bound below vacuous")

	assert.Lessf(t, peak, int64(trackerPeakBound),
		"the cursor tracker peaked at %d entries across %d parses — it is not being emptied by the "+
			"client's closes, which is what lets a recycled cursor id resolve to a statement the "+
			"client is no longer running", peak, parses)

	assert.LessOrEqual(t, tracked[len(tracked)-1], int64(trackerPeakBound),
		"the tracker must come back down once the client's closes have been processed")
}

// pythonThinWorkload is the same set of shapes as runCursorWorkloads, for the
// client the spec singles out: python-oracledb thin, whose plain `cur.execute()`
// loop re-executes by cursor id with no prepared-statement API in sight. The
// statements deliberately match the go-ora ones so the two measurements are
// comparable.
//
// It is a script rather than a Go driver because there is no Go implementation
// of what is being measured — how *that* client drives the wire.
const pythonThinWorkload = `
import sys
import oracledb

host, port, service, user, password = sys.argv[1:6]
conn = oracledb.connect(user=user, password=password,
                        dsn=oracledb.makedsn(host, int(port), service_name=service))

cur = conn.cursor()

# 1. the plain execute() loop
for _ in range(5):
    cur.execute("SELECT 1 AS n FROM dual")
    cur.fetchall()

# 2. bind-heavy, same statement every time
for i in range(5):
    cur.execute("SELECT :a AS a, :b AS b, :c AS c FROM dual", a=i, b=i + 1, c=i + 2)
    cur.fetchall()

# 3. several cursors open at once, interleaved
cursors = [conn.cursor() for _ in range(3)]
for _ in range(5):
    for j, c in enumerate(cursors):
        c.execute("SELECT %d AS n FROM dual" % (10 * (j + 1)))
        c.fetchall()

# 4. DML
cur.execute("BEGIN EXECUTE IMMEDIATE 'DROP TABLE dbbat_py_probe'; EXCEPTION WHEN OTHERS THEN NULL; END;")
cur.execute("CREATE TABLE dbbat_py_probe (id NUMBER)")
for i in range(5):
    cur.execute("INSERT INTO dbbat_py_probe VALUES (:1)", [i])
conn.commit()

# 5. an anonymous PL/SQL block
for i in range(5):
    cur.execute("BEGIN INSERT INTO dbbat_py_probe VALUES (:1); END;", [1000 + i])
conn.commit()

# 6. a REF cursor
cur.execute("""CREATE OR REPLACE PROCEDURE dbbat_py_refcur(p OUT SYS_REFCURSOR) AS
BEGIN
  OPEN p FOR SELECT LEVEL AS n FROM dual CONNECT BY LEVEL <= 5;
END;""")
for _ in range(5):
    out = cur.var(oracledb.CURSOR)
    cur.callproc("dbbat_py_refcur", [out])
    out.getvalue().fetchall()

# 7. a statement that fails, retried: its OER carries a real ORA code, so no
#    cursor id is read off it
for _ in range(3):
    try:
        cur.execute("SELECT * FROM dbbat_no_such_table_at_all")
        cur.fetchall()
    except oracledb.DatabaseError:
        pass

# 8. statement-cache churn, well past the default cache size of 20
for i in range(40):
    for _ in range(2):
        cur.execute("SELECT %d AS churn FROM dual" % i)
        cur.fetchall()

# ...and back to the first statement, long after its cursor was churned
for _ in range(5):
    cur.execute("SELECT 1 AS n FROM dual")
    cur.fetchall()

cur.execute("DROP PROCEDURE dbbat_py_refcur")
cur.execute("DROP TABLE dbbat_py_probe")
conn.close()
print("ok")
`

// TestIntegration_CursorIDLearningMissRate_PythonThin repeats the measurement
// with python-oracledb thin. go-ora is the client dbbat's own tests are written
// in, so measuring only it would prove the least: the spec's worry is the client
// that reaches the piggyback path without ever asking for a prepared statement.
//
// Skipped when python-oracledb is not installed, which is the case in CI; the
// recorded python capture replayed in cursor_reexec_replay_test.go is the
// always-on half of the same evidence.
func TestIntegration_CursorIDLearningMissRate_PythonThin(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	if err := exec.Command("python3", "-c", "import oracledb").Run(); err != nil {
		t.Skip("python-oracledb not installed (pip install oracledb)")
	}

	env := startOracleThroughProxy(t, nil)

	script := filepath.Join(t.TempDir(), "workload.py")
	require.NoError(t, os.WriteFile(script, []byte(pythonThinWorkload), 0o600))

	cmd := exec.Command("python3", script,
		env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python-oracledb workload failed:\n%s", out)

	time.Sleep(2 * time.Second)

	var (
		parses    = env.logs.count(logMsgQueryIntercepted)
		learned   = env.logs.count(logMsgLearnedCursorID)
		resolved  = env.logs.count(logMsgReexecGated)
		untracked = env.logs.count(logMsgUntrackedCursorForwarded)
	)

	reexecs := resolved + untracked

	t.Logf("cursor-id learning measurement, python-oracledb thin (image=%s):", oracleTestImage())
	t.Logf("  parses seen (query intercepted):      %d", parses)
	t.Logf("  cursor ids learned:                   %d", learned)
	t.Logf("  re-executions resolved to their SQL:  %d", resolved)
	t.Logf("  re-executions naming an unknown id:   %d", untracked)

	if reexecs > 0 {
		t.Logf("  learning miss rate:                   %d/%d", untracked, reexecs)
	}

	unlearned := multisetDiff(env.logs.sqlsFor(logMsgQueryIntercepted),
		env.logs.sqlsFor(logMsgLearnedCursorID))
	if len(unlearned) > 0 {
		t.Logf("  parses with no cursor id learned:     %s", strings.Join(unlearned, " | "))
	}

	for _, q := range unlearned {
		assert.Contains(t, q, "dbbat_no_such_table_at_all",
			"only the statement that fails may go without a learned cursor id")
	}

	assert.Positive(t, reexecs, "python-oracledb must have produced cursor re-executions")
	assert.Zero(t, untracked,
		"cursor-id learning missed %d of %d python-oracledb re-executions", untracked, reexecs)
}

// TestIntegration_CursorReexecUnderReadOnlyIsNotBrokenByTheGate is the guard the
// fail-closed change needs: the same workloads, under a `read_only` grant (a
// statement-shaped control), must still run every SELECT to completion. If
// cursor-id learning ever regresses, this is where it surfaces — as the
// ORA-01031 the spec warned about — rather than in a customer's session.
func TestIntegration_CursorReexecUnderReadOnlyIsNotBrokenByTheGate(t *testing.T) {
	env := startOracleThroughProxy(t, []string{store.ControlReadOnly})

	ctx := context.Background()

	stmt, err := env.db.PrepareContext(ctx, "SELECT 1 AS n FROM dual")
	require.NoError(t, err)

	defer func() { _ = stmt.Close() }()

	for i := 0; i < 10; i++ {
		var n int
		require.NoErrorf(t, stmt.QueryRowContext(ctx).Scan(&n), "read-only re-execution %d", i)
		assert.Equal(t, 1, n)
	}

	// Several cursors at once, still under read_only.
	queries := []string{
		"SELECT 10 AS n FROM dual",
		"SELECT :1 AS n FROM dual",
		"SELECT LEVEL AS n FROM dual CONNECT BY LEVEL <= 20",
	}

	for _, q := range queries {
		st, err := env.db.PrepareContext(ctx, q)
		require.NoError(t, err)

		for run := 0; run < 5; run++ {
			var args []any
			if strings.Contains(q, ":1") {
				args = []any{run}
			}

			rows, err := st.QueryContext(ctx, args...)
			require.NoErrorf(t, err, "read-only re-execution of %q", q)

			for rows.Next() {
				var n int
				require.NoError(t, rows.Scan(&n))
			}

			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
		}

		require.NoError(t, st.Close())
	}

	time.Sleep(2 * time.Second)

	assert.Zero(t, env.logs.count(logMsgUntrackedCursorRefused),
		"no ordinary read-only re-execution may be refused as an untracked cursor")
	assert.Zero(t, env.logs.count(logMsgUntrackedCursorForwarded),
		"no ordinary read-only re-execution may name a cursor dbbat could not resolve")
}
