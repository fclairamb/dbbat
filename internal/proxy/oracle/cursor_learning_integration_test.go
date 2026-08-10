//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	go_ora "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// The log messages the measurement counts. They are the proxy's own, unchanged:
// counting them is the "counter and a WARN" the spec asked for, without leaving
// instrumentation behind in the production path.
const (
	logMsgQueryIntercepted = "query intercepted"
	logMsgLearnedCursorID  = "learned server-assigned cursor id"
	logMsgReexecGated      = "intercepted cursor re-execution"
	logMsgReexecUntracked  = "forwarding a piggyback re-execution of an untracked cursor: its statement is unknown"
	logMsgReexecRefused    = "refused a re-execution of an untracked cursor under a restrictive grant"
)

// countingHandler counts slog records by message and keeps the `sql` attribute
// of each one, so the measurement can check not only *that* a re-execution
// resolved to a statement but that it resolved to the **right** one — a
// mis-learned cursor id is a silent wrong-SQL gate, which is worse than a miss.
type countingHandler struct {
	mu     sync.Mutex
	counts map[string]int
	sqls   map[string][]string
}

func newCountingHandler() *countingHandler {
	return &countingHandler{
		counts: make(map[string]int),
		sqls:   make(map[string][]string),
	}
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, rec slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.counts[rec.Message]++

	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "sql" {
			h.sqls[rec.Message] = append(h.sqls[rec.Message], a.Value.String())
		}

		return true
	})

	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.counts[msg]
}

func (h *countingHandler) sqlsFor(msg string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]string, len(h.sqls[msg]))
	copy(out, h.sqls[msg])

	return out
}

// oracleThroughProxy stands up the whole chain — Oracle container, PostgreSQL
// store, dbbat Oracle proxy — and hands back a go-ora `*sql.DB` pointed at the
// proxy, plus the handler counting what the proxy logged.
type oracleThroughProxy struct {
	db   *sql.DB
	logs *countingHandler
}

func startOracleThroughProxy(t *testing.T, controls []string) *oracleThroughProxy {
	t.Helper()

	ctx := context.Background()

	oracleContainer, oracleHost, oraclePort := startOracleContainer(t)
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
	pgDSN := fmt.Sprintf("postgres://test:test@%s:%s/dbbat_test?sslmode=disable", pgHost, pgPort.Port())

	dataStore, err := store.New(ctx, pgDSN)
	require.NoError(t, err)

	t.Cleanup(func() { dataStore.Close() })
	require.NoError(t, dataStore.Migrate(ctx))

	// Lowercase: Oracle clients uppercase the username on the wire and the proxy
	// lowercases it again before the dbbat lookup.
	user, err := dataStore.CreateUser(ctx, "cursorprobe", "$argon2id$v=19$m=4096,t=3,p=1$salt$hash", []string{"connector"})
	require.NoError(t, err)

	encryptionKey := []byte("0123456789012345678901234567890X")

	service := oracleTestService()
	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:              service,
		Host:              oracleHost,
		Port:              oraclePort,
		DatabaseName:      service,
		OracleServiceName: &service,
		Username:          "system",
		Password:          "oracle",
		Protocol:          store.ProtocolOracle,
	}, encryptionKey)
	require.NoError(t, err)

	_, err = createGrantWithControls(ctx, t, dataStore, user.UID, db.UID, controls)
	require.NoError(t, err)

	// Oracle clients authenticate to dbbat with an API key as the password.
	_, plainKey, err := dataStore.CreateAPIKey(ctx, user.UID, "cursorprobe-key", nil, encryptionKey)
	require.NoError(t, err)

	logs := newCountingHandler()

	proxy := NewServer(dataStore, encryptionKey, nil, config.QueryStorageConfig{}, config.DumpConfig{}, slog.New(logs))
	go func() { _ = proxy.Start("127.0.0.1:0") }()

	t.Cleanup(func() { _ = proxy.Shutdown(ctx) })

	require.Eventually(t, func() bool { return proxy.Addr() != nil }, 5*time.Second, 50*time.Millisecond)

	host, portStr, err := net.SplitHostPort(proxy.Addr().String())
	require.NoError(t, err)

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

	return &oracleThroughProxy{db: client, logs: logs}
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

		defer stmt.Close()

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

		defer stmt.Close()

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

			defer stmt.Close()
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

		defer stmt.Close()

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

		defer stmt.Close()

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

		defer stmt.Close()

		for i := 0; i < reexecRuns; i++ {
			var cursor go_ora.RefCursor

			_, err := stmt.ExecContext(ctx, go_ora.Out{Dest: &cursor})
			require.NoError(t, err)

			ds, err := cursor.Query()
			require.NoError(t, err)

			row := make([]driver.Value, len(ds.Columns()))
			for ds.Next(row) == nil { //nolint:revive // draining the ref cursor is the point
				continue
			}

			require.NoError(t, ds.Close())
			require.NoError(t, cursor.Close())
		}

		expected = append(expected, call)
	})

	// 7. Statement-cache churn: enough distinct statements to push the earlier
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

		defer stmt.Close()

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
		untracked = env.logs.count(logMsgReexecUntracked)
		refused   = env.logs.count(logMsgReexecRefused)
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

	assert.Positive(t, parses, "the workloads must have reached the proxy at all")
	assert.Positive(t, reexecs, "the workloads must have produced cursor re-executions")

	// The claim under test: no re-execution of a statement this client parsed
	// through the proxy ever names a cursor dbbat could not resolve.
	assert.Zero(t, untracked,
		"cursor-id learning missed %d of %d re-executions; failing the piggyback path closed would "+
			"turn each of those into ORA-01031", untracked, reexecs)

	// And the ids it did learn point at the right statements.
	want := make(map[string]bool, len(expected))
	for _, q := range expected {
		want[truncateSQL(q, 200)] = true
	}

	var unexpected []string

	for _, got := range env.logs.sqlsFor(logMsgReexecGated) {
		if !want[got] {
			unexpected = append(unexpected, got)
		}
	}

	sort.Strings(unexpected)
	assert.Emptyf(t, unexpected,
		"a re-execution resolved to a statement this client never ran — the cursor id was mis-learned: %s",
		strings.Join(unexpected, " | "))
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

	defer stmt.Close()

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

	assert.Zero(t, env.logs.count(logMsgReexecRefused),
		"no ordinary read-only re-execution may be refused as an untracked cursor")
	assert.Zero(t, env.logs.count(logMsgReexecUntracked),
		"no ordinary read-only re-execution may name a cursor dbbat could not resolve")
}
