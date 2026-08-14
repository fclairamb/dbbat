//go:build integration

package oracle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// The end-to-end half of failed_stmt_replay_test.go: a statement that fails
// upstream must land in `queries` with its ORA text in `error`.
//
// The replay tests prove the decoder reads the recorded bytes. This proves the
// whole chain does it — proxy, session, store — against a live Oracle, on the
// client dbbat's own tests are written in *and* on the one whose OER layout
// differs. Before the fix, only the failed DDL was recorded with an error;
// everything else was logged as a plain success by the next statement's flush.

// failingStatementDeadline bounds one failing statement end to end, and doubles
// as the deadline for the store poll that reads its row back.
//
// It is generous on purpose. Each test here stands up its own Oracle *and* its
// own PostgreSQL container, so on a Docker host with other suites competing a
// statement that would normally answer in milliseconds can take seconds — and a
// tight ceiling turns that into a failure that looks like it is about the
// statement rather than about the machine.
const failingStatementDeadline = 90 * time.Second

// failedStmtSetup is the table the constraint violation needs.
const (
	failedStmtCreate = "CREATE TABLE dbbat_failed_stmt (id NUMBER PRIMARY KEY)"
	failedStmtSeed   = "INSERT INTO dbbat_failed_stmt VALUES (1)"
	failedStmtDrop   = "DROP TABLE dbbat_failed_stmt"
)

// failedStatement is one statement that must fail, and the diagnostic dbbat has
// to have recorded for it.
type failedStatement struct {
	sql     string
	query   bool // driven as a query, so a failure raised while producing the row surfaces
	wantORA string
}

// failedStatements are the shapes measured in the captures, minus the DDL that
// already worked: those are the ones whose OER carries no end-of-call bit.
var failedStatements = []failedStatement{
	{sql: "SELECT 1 FROM dbbat_no_such_table_xyz", query: true, wantORA: "ORA-00942"},
	{sql: "INSERT INTO dbbat_failed_stmt VALUES (1)", wantORA: "ORA-00001"},
	{sql: "SELECT 1/0 FROM dual", query: true, wantORA: "ORA-01476"},
	{sql: "BEGIN RAISE_APPLICATION_ERROR(-20001, 'dbbat measured this'); END;", wantORA: "ORA-20001"},
	{sql: "DROP TABLE dbbat_no_such_table_xyz", wantORA: "ORA-00942"},
}

// TestIntegration_FailingStatementsRecordTheirORAError drives every shape above
// through the proxy with go-ora and reads the query rows back.
func TestIntegration_FailingStatementsRecordTheirORAError(t *testing.T) {
	env := startOracleThroughProxy(t, nil)
	ctx := context.Background()

	_, _ = env.db.ExecContext(ctx, failedStmtDrop) // ORA-00942 on a clean database
	_, err := env.db.ExecContext(ctx, failedStmtCreate)
	require.NoError(t, err)

	defer func() { _, _ = env.db.ExecContext(context.Background(), failedStmtDrop) }()

	_, err = env.db.ExecContext(ctx, failedStmtSeed)
	require.NoError(t, err)

	for _, st := range failedStatements {
		t.Run(st.wantORA+" "+st.sql, func(t *testing.T) {
			runCtx, cancel := context.WithTimeout(ctx, failingStatementDeadline)
			defer cancel()

			var runErr error

			if st.query {
				var n int
				runErr = env.db.QueryRowContext(runCtx, st.sql).Scan(&n)
			} else {
				_, runErr = env.db.ExecContext(runCtx, st.sql)
			}

			require.Errorf(t, runErr, "%q was supposed to fail upstream", st.sql)
			assert.Contains(t, runErr.Error(), st.wantORA, "the client's own error")

			env.assertFailedQueryLogged(t, st.sql, st.wantORA)
		})
	}
}

// assertFailedQueryLogged is the assertion the whole file exists for: the
// statement is in `queries` with its ORA text, and with no row count, since it
// affected nothing.
func (e *oracleThroughProxy) assertFailedQueryLogged(t *testing.T, wantSQL, wantORA string) {
	t.Helper()

	ctx := context.Background()

	var match *store.Query

	require.Eventuallyf(t, func() bool {
		queries, err := e.store.ListQueries(ctx, store.QueryFilter{Limit: 500})
		if err != nil {
			return false
		}

		match = nil

		for i := range queries {
			if queries[i].SQLText == wantSQL && queries[i].Error != nil {
				match = &queries[i]
			}
		}

		return match != nil
	}, failingStatementDeadline, 250*time.Millisecond,
		"the failing statement %q was never logged with an error", wantSQL)

	assert.Containsf(t, *match.Error, wantORA,
		"queries.error must carry the server's diagnostic for %q", wantSQL)
	assert.Nilf(t, match.RowsAffected, "%q affected nothing", wantSQL)

	conn, err := e.store.GetConnectionByUID(ctx, match.ConnectionID)
	require.NoError(t, err)
	assert.Equal(t, e.user.UID, conn.UserID, "a failure row joins to its user like any other query")

	result, err := e.store.VerifyQueryChain(ctx, match.ConnectionID)
	require.NoError(t, err)
	assert.Nil(t, result.Break, "a failure row must be a valid link in the connection's query chain")
}

// pythonFailingScript runs the same failures from python-oracledb thin, which
// negotiates a different OER tail than go-ora. It prints nothing the test parses
// beyond a completion marker — the assertions are made against `queries`.
const pythonFailingScript = `
import sys
import oracledb

host, port, service, user, password = sys.argv[1:6]
conn = oracledb.connect(user=user, password=password,
                        dsn=oracledb.makedsn(host, int(port), service_name=service))
cur = conn.cursor()

for sql in [
    "SELECT 1 FROM dbbat_no_such_table_py",
    "SELECT 1/0 FROM dual",
    "BEGIN RAISE_APPLICATION_ERROR(-20001, 'dbbat measured this'); END;",
]:
    try:
        cur.execute(sql)
        cur.fetchall()
        print("DID-NOT-FAIL:", sql, flush=True)
    except oracledb.DatabaseError as e:
        print("failed:", str(e).strip().splitlines()[0], flush=True)

conn.close()
print("done", flush=True)
`

// TestIntegration_FailingStatementsRecordTheirORAErrorPythonThin is the second
// client. It matters because the premise this fix overturned was that the
// end-of-call bit told these two apart; the measurement says it does not, and
// this is where that stops being a claim about a capture file.
//
// Skipped when python-oracledb is not installed, which is the case in CI; the
// replay tests carry the same evidence there.
func TestIntegration_FailingStatementsRecordTheirORAErrorPythonThin(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	if err := exec.Command("python3", "-c", "import oracledb").Run(); err != nil {
		t.Skip("python-oracledb not installed (pip install oracledb)")
	}

	env := startOracleThroughProxy(t, nil)

	script := filepath.Join(t.TempDir(), "failing.py")
	require.NoError(t, os.WriteFile(script, []byte(pythonFailingScript), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), failingStatementDeadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", script,
		env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python-oracledb never finished:\n%s", out)

	output := string(out)
	assert.NotContains(t, output, "DID-NOT-FAIL", "every statement was supposed to fail:\n%s", output)
	assert.Contains(t, output, "done", "the client must close cleanly:\n%s", output)

	env.assertFailedQueryLogged(t, "SELECT 1 FROM dbbat_no_such_table_py", "ORA-00942")
	env.assertFailedQueryLogged(t, "SELECT 1/0 FROM dual", "ORA-01476")
	env.assertFailedQueryLogged(t,
		"BEGIN RAISE_APPLICATION_ERROR(-20001, 'dbbat measured this'); END;", "ORA-20001")
}

// The mid-fetch shape, live. Every failure above is raised *before* the first
// row — the server sends the OER instead of the QueryResult — which is the one
// thing separating them from the case
// midfetch_fail_replay_test.go measures out of a capture. Here the failure is
// raised deep inside a fetch, so the whole chain has to survive a statement
// that was already streaming when it died: it was persisted long ago, its
// column definitions are decoded, and the diagnostic arrives as a bit-less
// standalone OER mid-row-stream.
//
// Before the relaxation this went through as a *success*: the OER was dropped,
// the statement stayed pending, and the DROP that follows closed it clean.
const (
	// The fixture the capture tooling uses, under its own table name so it
	// cannot collide with a leftover from a recording run against the same
	// database.
	midFetchLiveRows  = 20000
	midFetchLiveBad   = 15000
	midFetchLiveTable = "dbbat_midfetch_live"

	midFetchLiveCreate = "CREATE TABLE " + midFetchLiveTable + " (id NUMBER, txt VARCHAR2(30))"
	midFetchLiveDrop   = "DROP TABLE " + midFetchLiveTable

	// No ORDER BY, deliberately: a sort would materialize the result set and
	// raise before the first row, which is the shape already covered above.
	midFetchLiveSelect = "SELECT id, TO_NUMBER(txt) AS n FROM " + midFetchLiveTable
)

// midFetchLiveSeed fills the table with one row that will not convert, far
// enough in that hundreds of fetch round trips precede it.
func midFetchLiveSeed() string {
	return fmt.Sprintf(`INSERT INTO %s SELECT level, `+
		`CASE WHEN level = %d THEN 'not-a-number' ELSE TO_CHAR(level) END `+
		`FROM dual CONNECT BY level <= %d`, midFetchLiveTable, midFetchLiveBad, midFetchLiveRows)
}

// TestIntegration_MidFetchFailureRecordsItsORAError drives the mid-fetch
// failure through the proxy against a live Oracle and reads the query row back.
func TestIntegration_MidFetchFailureRecordsItsORAError(t *testing.T) {
	env := startOracleThroughProxy(t, nil)
	ctx := context.Background()

	_, _ = env.db.ExecContext(ctx, midFetchLiveDrop) // ORA-00942 on a clean database

	_, err := env.db.ExecContext(ctx, midFetchLiveCreate)
	require.NoError(t, err)

	defer func() { _, _ = env.db.ExecContext(context.Background(), midFetchLiveDrop) }()

	_, err = env.db.ExecContext(ctx, midFetchLiveSeed())
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(ctx, failingStatementDeadline)
	defer cancel()

	rows, err := env.db.QueryContext(runCtx, midFetchLiveSelect)
	require.NoError(t, err, "the execute itself must succeed — the failure has to be raised mid-fetch")

	var seen int

	for rows.Next() {
		var id, n float64

		require.NoError(t, rows.Scan(&id, &n))

		seen++
	}

	require.Errorf(t, rows.Err(), "the fetch was supposed to fail; it returned %d rows cleanly", seen)
	assert.Contains(t, rows.Err().Error(), "ORA-01722", "the client's own error")
	assert.Positive(t, seen, "rows must have reached the client before the failure — otherwise this "+
		"is the pre-first-row shape the tests above already cover")

	require.NoError(t, rows.Close())

	// A statement that dies mid-fetch is still a failure, and the row it lands
	// on is one that has existed since the stream opened.
	env.assertFailedQueryLogged(t, midFetchLiveSelect, "ORA-01722")
}
