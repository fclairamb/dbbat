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

// The OCI half, live. Everything above drives a *thin* client, and for a long
// time that was the whole file: sqlplus, the Instant Client and SQL*Developer
// over OCI marshal the summary object fixed-width, dbbat's decoder read TTC
// compressed integers only, and so **every** failing statement on those clients
// was written to `queries` as a success — mid-fetch or not, DML or DDL. The
// client saw its ORA text; the query row said nothing happened.
//
// That is what these statements prove is over, end to end rather than off
// recorded bytes: the same shapes as failedStatements, on the client whose
// encoding the decoder had to learn to read (decodeOERFieldsAtLayout).
//
// The PL/SQL block is deliberately absent. sqlplus needs a lone `/` to submit
// one, and what reaches `queries` then is not the text as typed — the
// assertions here match SQL exactly, so a shape that cannot be matched exactly
// would have to be asserted more loosely than the point deserves. Its coverage
// is the go-ora case above and the replayed bytes.
const ociFailingScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'oci-ready=' || 1 FROM dual;
SELECT 1 FROM dbbat_no_such_table_oci;
INSERT INTO dbbat_failed_stmt_oci VALUES (1);
SELECT 1/0 FROM dual;
DROP TABLE dbbat_no_such_table_oci;
SELECT 'oci-done=' || 42 FROM dual;
EXIT
`

// ociFailedStatements is what ociFailingScript must have left behind, spelled
// out rather than derived from the script so a change to one cannot quietly
// stop the other from matching anything. The text is the statement *without*
// its trailing semicolon, which is what sqlplus puts on the wire.
var ociFailedStatements = []failedStatement{
	{sql: "SELECT 1 FROM dbbat_no_such_table_oci", wantORA: "ORA-00942"},
	{sql: "INSERT INTO dbbat_failed_stmt_oci VALUES (1)", wantORA: "ORA-00001"},
	{sql: "SELECT 1/0 FROM dual", wantORA: "ORA-01476"},
	{sql: "DROP TABLE dbbat_no_such_table_oci", wantORA: "ORA-00942"},
}

// ociFailedStmtSetup is the table the unique-key violation needs. It is seeded
// through the fixture's own go-ora connection, so nothing in the sqlplus script
// has to succeed for the failures to be failures.
const (
	ociFailedStmtCreate = "CREATE TABLE dbbat_failed_stmt_oci (id NUMBER PRIMARY KEY)"
	ociFailedStmtSeed   = "INSERT INTO dbbat_failed_stmt_oci VALUES (1)"
	ociFailedStmtDrop   = "DROP TABLE dbbat_failed_stmt_oci"
)

// TestIntegration_FailingStatementsRecordTheirORAErrorOCI runs the shapes above
// through sqlplus.
//
// The OCI client is sqlplus from PATH when there is one, and otherwise the one
// bundled in the Oracle container the fixture already started, so this runs in
// CI too; ORACLE_TEST_REQUIRE_OCI_CLIENT=1 makes "no client" a failure rather
// than a skip. See oci_client_integration_test.go.
//
// WHENEVER SQLERROR is deliberately not set to EXIT: sqlplus must report each
// ORA error and carry on, so one script covers every shape and the closing
// SELECT proves the session outlived them.
func TestIntegration_FailingStatementsRecordTheirORAErrorOCI(t *testing.T) {
	ociAuthModeNote(t)

	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	ctx := context.Background()

	_, _ = env.db.ExecContext(ctx, ociFailedStmtDrop) // ORA-00942 on a clean database

	_, err := env.db.ExecContext(ctx, ociFailedStmtCreate)
	require.NoError(t, err)

	defer func() { _, _ = env.db.ExecContext(context.Background(), ociFailedStmtDrop) }()

	_, err = env.db.ExecContext(ctx, ociFailedStmtSeed)
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(ctx, failingStatementDeadline)
	defer cancel()

	output, err := oci.run(t, runCtx, ociFailingScript)
	require.NoErrorf(t, err, "%s never came back:\n%s", oci.label, output)

	assertNoOCIAuthMalformation(t, output)

	assert.Contains(t, output, "oci-ready=1", "the OCI session must work before the failures:\n%s", output)
	assert.Contains(t, output, "oci-done=42", "and must outlive them:\n%s", output)

	for _, st := range ociFailedStatements {
		assert.Containsf(t, output, st.wantORA,
			"sqlplus must have reported %s for %q itself:\n%s", st.wantORA, st.sql, output)
	}

	// The assertion the whole file is about: what the *client* saw is also what
	// dbbat recorded. Before the fixed-width decoder every one of these rows
	// carried no error at all.
	for _, st := range ociFailedStatements {
		t.Run(st.wantORA+" "+st.sql, func(t *testing.T) {
			env.assertFailedQueryLogged(t, st.sql, st.wantORA)
		})
	}
}

// The *successful* OCI call, live — the other half of the standalone summary
// object, and the one that stayed broken a release longer than the failures did.
//
// An OCI client ends a call with a fixed-width summary object that carries no
// end-of-call bit (measured: CallStatus 0x1 on every standalone one in
// testdata/), and decodeOERAt demanded the bit. So a successful statement was
// completed by the *next* statement's flushPendingQuery, which knows nothing
// about it: no rows_affected, and a duration_ms measuring however long the client
// sat idle in between. See decodeFixedStatusOERAt and
// standalone_status_oer_replay_test.go, which pin both halves off the recordings.
//
// **Two tests, because the two observables live on two different paths, and only
// one of them is what this fix reaches.** A `SELECT` is the shape that ends on a
// *standalone* status OER — measured: `sqlplus_cursor_reexec.pcapng` is five of
// them, one per statement — so its `duration_ms` is a live assertion of the fix
// and is expected to pass. A DML is not: every DML in `sqlplus_midfetch_fail.pcapng`
// completed through `handleResponse` instead, and its row count is refused by a
// *third* fixed-width layout. That assertion is therefore gated — see
// TestIntegration_DMLRowCountLandsFromItsOwnOEROCI.
//
// What makes the `HOST sleep` load-bearing rather than decoration: the gap sits
// between the statement under test and the next one, so a statement still pending
// when the next one arrives is charged the whole of it. If `HOST` is unavailable
// the duration assertion simply stops being able to fail — it never turns into a
// false one.
const ociSelectScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'oci-sel-ready=' || 1 FROM dual;
SELECT 1 AS n FROM dual;
HOST sleep 4
SELECT 'oci-sel-done=' || 42 FROM dual;
EXIT
`

const (
	// ociSelectStatement is the statement text sqlplus puts on the wire for the
	// second line of the script — without the trailing semicolon, exactly as
	// ociFailedStatements spells its own. It is the same SQL
	// sqlplus_cursor_reexec.pcapng runs four times, whose standalone ORA-01403
	// status OER TestDumpReplay_OCIStatusOERsCompleteTheirOwnStatement pins.
	ociSelectStatement = "SELECT 1 AS n FROM dual"

	// ociIdleMs is the `HOST sleep` in the scripts, in milliseconds. A statement
	// completed by its own OER cannot be charged it.
	ociIdleMs = 4000
)

// TestIntegration_SuccessfulSelectCompletesOnItsOwnOEROCI is the live assertion
// of what this fix does: a sqlplus SELECT is completed by its own standalone
// status OER, so the idle time that follows it is not charged to it.
//
// Before the fix the statement stayed pending across the `HOST sleep` and was
// closed by the *next* statement's flushPendingQuery, which is exactly the 74 s
// UPDATE the python-oracledb half of this story was found by.
func TestIntegration_SuccessfulSelectCompletesOnItsOwnOEROCI(t *testing.T) {
	ociAuthModeNote(t)

	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	runCtx, cancel := context.WithTimeout(context.Background(),
		failingStatementDeadline+ociIdleMs*time.Millisecond)
	defer cancel()

	output, err := oci.run(t, runCtx, ociSelectScript)
	require.NoErrorf(t, err, "%s never came back:\n%s", oci.label, output)

	assertNoOCIAuthMalformation(t, output)

	assert.Contains(t, output, "oci-sel-ready=1", "the OCI session must work before the SELECT:\n%s", output)
	assert.Contains(t, output, "oci-sel-done=42", "and must outlive it:\n%s", output)

	env.assertQueryCompletedWithin(t, ociSelectStatement, ociIdleMs)
}

// ociDMLRowCountEnv un-gates the test below. It is an env var rather than a plain
// t.Skip because the assertion is meant to be *runnable* by whoever fixes the
// third fixed-width layout, not read and re-derived.
const ociDMLRowCountEnv = "ORACLE_TEST_OCI_DML_ROWCOUNT"

// The DML script. Same shape as ociSelectScript, with the idle gap between the
// UPDATE and the COMMIT that would flush it.
const ociDMLScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'oci-dml-ready=' || 1 FROM dual;
UPDATE dbbat_dml_oci SET n = n + 1;
HOST sleep 4
COMMIT;
SELECT 'oci-dml-done=' || 42 FROM dual;
EXIT
`

// The table the DML above runs against, and the statement text sqlplus puts on
// the wire for it.
const (
	ociDMLCreate = "CREATE TABLE dbbat_dml_oci (id NUMBER PRIMARY KEY, n NUMBER)"
	ociDMLDrop   = "DROP TABLE dbbat_dml_oci"
	ociDMLUpdate = "UPDATE dbbat_dml_oci SET n = n + 1"

	// ociDMLRows is how many rows the UPDATE must report having touched. Seeded
	// through the fixture's own go-ora connection, so nothing in the sqlplus
	// script has to succeed for the count to be what it is.
	ociDMLRows = 7
)

// TestIntegration_DMLRowCountLandsFromItsOwnOEROCI is a **known-gap test, gated
// off by default**. It is the assertion an OCI DML's `rows_affected` deserves, on
// a path the standalone status reading does not reach.
//
// Why it is gated rather than expected to pass, measured rather than guessed. An
// OCI DML's summary object does not arrive standalone: in
// `sqlplus_midfetch_fail.pcapng` the whole `DROP` / `CREATE` /
// `INSERT ... SELECT` / `COMMIT` sequence is answered by func `0x08` Responses,
// and `ociStatusDumps()` pins that recording at exactly **one** standalone status
// OER — the login probe. So a DML's row count is `handleResponse`'s business, and
// there it depends on a **third** fixed-width layout dbbat cannot read: packet #31
// of that recording (the `INSERT ... SELECT` of 20 000 rows) carries a populated
// logical-rowid DLC that displaces the trailing RetCode, so
// decodeOERFixedFieldsAt's anchor refuses it at both known layouts
// (oerFixed32Layout and oerFixed64Layout) and the fall-through reaches the legacy
// decodeTTCResponse, which misreads v315+ responses. Nothing makes a seven-row
// UPDATE's rowid block less likely to be populated than a 20 000-row INSERT's.
//
// **What un-gates it:** teaching decodeOERFixedFieldsAt (or
// oerFixedWidthTailFieldsAt, which walks the same prefix) the layout in which the
// rowid DLC is populated, so packet #31's summary object decodes and its
// CurRowNumber of 20 000 is readable. `skipOERFixedFields` is where that walk
// bails today. Then run with ORACLE_TEST_OCI_DML_ROWCOUNT=1 and delete this gate.
// Left un-gated it would turn a real, out-of-scope gap into a red nightly with no
// owner — and a green nightly must not be read as "OCI DML row counts are
// verified", which is the other half of why the gate says so out loud.
//
// See docs/oracle.md, "a successful call on an OCI client".
func TestIntegration_DMLRowCountLandsFromItsOwnOEROCI(t *testing.T) {
	if os.Getenv(ociDMLRowCountEnv) == "" {
		t.Skipf("gated: an OCI DML's summary object arrives embedded in a Response with a populated "+
			"logical-rowid DLC that displaces the trailing RetCode (sqlplus_midfetch_fail.pcapng "+
			"packet #31), so decodeOERFixedFieldsAt refuses it at both known layouts and "+
			"rows_affected stays NULL. That third layout is out of the scope of the standalone "+
			"status reading this file's SELECT test covers. Teach the decoder that layout, then "+
			"set %s=1 and remove this gate", ociDMLRowCountEnv)
	}

	ociAuthModeNote(t)

	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	ctx := context.Background()

	_, _ = env.db.ExecContext(ctx, ociDMLDrop) // ORA-00942 on a clean database

	_, err := env.db.ExecContext(ctx, ociDMLCreate)
	require.NoError(t, err)

	defer func() { _, _ = env.db.ExecContext(context.Background(), ociDMLDrop) }()

	for i := 1; i <= ociDMLRows; i++ {
		_, err = env.db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO dbbat_dml_oci VALUES (%d, %d)", i, i))
		require.NoError(t, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, failingStatementDeadline+ociIdleMs*time.Millisecond)
	defer cancel()

	output, err := oci.run(t, runCtx, ociDMLScript)
	require.NoErrorf(t, err, "%s never came back:\n%s", oci.label, output)

	assertNoOCIAuthMalformation(t, output)

	assert.Contains(t, output, "oci-dml-ready=1", "the OCI session must work before the DML:\n%s", output)
	assert.Contains(t, output, "oci-dml-done=42", "and must outlive it:\n%s", output)

	match := env.assertQueryCompletedWithin(t, ociDMLUpdate, ociIdleMs)

	require.NotNilf(t, match.RowsAffected,
		"%q must carry the row count its own OER reported — a NULL here means the statement was "+
			"closed by the next one's flushPendingQuery instead", ociDMLUpdate)
	assert.Equal(t, int64(ociDMLRows), *match.RowsAffected)
}

// assertQueryCompletedWithin is the positive counterpart of
// assertFailedQueryLogged: the statement is in `queries` with no error and with a
// duration that ends where the call did rather than where the client's next
// statement began. It returns the row so a caller can assert more of it.
func (e *oracleThroughProxy) assertQueryCompletedWithin(
	t *testing.T,
	wantSQL string,
	maxDurationMs float64,
) *store.Query {
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
			if queries[i].SQLText == wantSQL {
				match = &queries[i]
			}
		}

		return match != nil && match.DurationMs != nil
	}, failingStatementDeadline, 250*time.Millisecond,
		"the statement %q was never logged as complete", wantSQL)

	assert.Nilf(t, match.Error, "%q succeeded; queries.error must be empty", wantSQL)

	require.NotNil(t, match.DurationMs)
	assert.Lessf(t, *match.DurationMs, maxDurationMs,
		"%q was logged at %.0fms: a statement completed by its own OER cannot be charged the idle "+
			"time that follows it", wantSQL, *match.DurationMs)

	conn, err := e.store.GetConnectionByUID(ctx, match.ConnectionID)
	require.NoError(t, err)
	assert.Equal(t, e.user.UID, conn.UserID, "a success row joins to its user like any other query")

	result, err := e.store.VerifyQueryChain(ctx, match.ConnectionID)
	require.NoError(t, err)
	assert.Nil(t, result.Break, "a success row must be a valid link in the connection's query chain")

	return match
}
