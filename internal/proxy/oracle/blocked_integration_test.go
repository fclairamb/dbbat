//go:build integration

package oracle

import (
	"context"
	"database/sql"
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

// refusalDeadline bounds a refused statement, which now has to come back on its
// own. It is a deadlock detector, not a grace period: a refusal answers in a
// round trip because it never leaves the proxy.
//
// It used to be a 20s "the call never ends, give up" bound, because
// writeTTCError synthesized a TTC Response (0x08) where a server ends a call
// with an OER (0x04), and framed it with the legacy 2-byte TNS length on top —
// measured here against Oracle 23ai Free, go-ora's ExecContext parked forever.
// Both halves are fixed, so the assertions below are what the spec asked for:
// the client gets its ORA error back.
const refusalDeadline = 30 * time.Second

// TestIntegration_BlockedStatementsAreLogged is the Oracle half of spec
// 2026-08-09-log-blocked-statements-pg-oracle, asserted the way the PostgreSQL
// suite asserts it (TestIntegration_ReadOnlyGrant_BlocksWrite /
// TestIntegration_BlockDDL_BlocksCreateTable): a statement dbbat refuses must
// leave a `queries` row behind carrying the refusal as `error`.
//
// The unit tests in blocked_persist_test.go drive handleOALL8 directly; this
// is the only place a real go-ora client, the real proxy and a real Oracle are
// in the same picture — which is how the hang above was found at all.
//
// Both controls share one fixture on purpose: every Oracle container start
// costs minutes, and narrowing the grant between the two halves (replaceGrant
// plus a fresh connection, because a session resolves its grant once at auth)
// is exactly what the PostgreSQL fixture does.
func TestIntegration_BlockedStatementsAreLogged(t *testing.T) {
	ctx := context.Background()

	env := startOracleThroughProxy(t, nil)

	// Seed under the permissive grant the fixture starts with.
	_, _ = env.db.ExecContext(ctx, "DROP TABLE dbbat_blocked_probe")

	_, err := env.db.ExecContext(ctx, "CREATE TABLE dbbat_blocked_probe (id NUMBER)")
	require.NoError(t, err, "the seed DDL must be allowed under an unrestricted grant")

	t.Run("read_only refuses a write", func(t *testing.T) {
		env.replaceGrant(t, []string{store.ControlReadOnly})

		client := env.newClient(t)

		// Reads still work under read_only.
		var n int
		require.NoError(t, client.QueryRowContext(ctx, "SELECT count(*) FROM dbbat_blocked_probe").Scan(&n))
		assert.Equal(t, 0, n)

		const writeSQL = "INSERT INTO dbbat_blocked_probe VALUES (1)"

		env.mustRefuse(t, client, writeSQL)
		env.mustStaySurvivable(t, client)

		assert.Zero(t, env.probeRowCount(t), "the refused INSERT must never have reached upstream")

		env.assertBlockedQueryLogged(t, writeSQL, "read-only")
	})

	t.Run("block_ddl refuses a CREATE TABLE", func(t *testing.T) {
		env.replaceGrant(t, []string{store.ControlBlockDDL})

		client := env.newClient(t)

		// DML still goes through under block_ddl.
		_, err := client.ExecContext(ctx, "INSERT INTO dbbat_blocked_probe VALUES (7)")
		require.NoError(t, err, "DML must still be allowed under a block_ddl grant")
		assert.Equal(t, 1, env.probeRowCount(t), "the allowed INSERT must have reached upstream")

		const ddlSQL = "CREATE TABLE dbbat_blocked_ddl (id NUMBER)"

		env.mustRefuse(t, client, ddlSQL)
		env.mustStaySurvivable(t, client)

		var exists int

		require.NoError(t,
			client.QueryRowContext(ctx,
				"SELECT count(*) FROM user_tables WHERE table_name = 'DBBAT_BLOCKED_DDL'").Scan(&exists))
		assert.Zero(t, exists, "the refused CREATE TABLE must never have reached upstream")

		env.assertBlockedQueryLogged(t, ddlSQL, "DDL")
	})
}

// mustRefuse runs a statement dbbat is expected to refuse and requires the
// client to come back with the ORA error, on its own, without the test having
// to abandon the call.
//
// This is the assertion the whole spec is about. Everything else in this file
// was already true while a real client hung on every refusal.
func (e *oracleThroughProxy) mustRefuse(t *testing.T, client *sql.DB, query string) {
	t.Helper()

	done := make(chan error, 1)

	go func() {
		_, err := client.ExecContext(context.Background(), query)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a refused statement must never report success: %q", query)
		assert.Contains(t, err.Error(), "ORA-01031",
			"the client must receive the refusal as an ORA error, not a protocol failure")
	case <-time.After(refusalDeadline):
		t.Fatalf("the refused statement %q never ended the client's call", query)
	}
}

// mustStaySurvivable is the other half of what a refusal owes the session: the
// PostgreSQL proxy keeps the connection alive across one, and Oracle now does
// too — which is only observable at all because the call ends.
func (e *oracleThroughProxy) mustStaySurvivable(t *testing.T, client *sql.DB) {
	t.Helper()

	var n int

	require.NoError(t,
		client.QueryRowContext(context.Background(), "SELECT 42 FROM dual").Scan(&n),
		"the connection that carried a refusal must still answer the next statement")
	assert.Equal(t, 42, n)
}

// probeRowCount reads the seeded table over a fresh connection, so it reports
// what actually reached upstream regardless of the refused session's state.
func (e *oracleThroughProxy) probeRowCount(t *testing.T) int {
	t.Helper()

	var n int

	require.NoError(t,
		e.newClient(t).QueryRowContext(context.Background(),
			"SELECT count(*) FROM dbbat_blocked_probe").Scan(&n))

	return n
}

// assertBlockedQueryLogged is the actual point of the test above: the refused
// statement is in `queries`, exactly once, with the refusal as `error`, with
// duration and rows_affected at zero because it never ran, attributed to the
// fixture user's connection — and it is an ordinary link in that connection's
// HMAC chain, so `dbbat audit verify --queries` still walks it cleanly.
func (e *oracleThroughProxy) assertBlockedQueryLogged(t *testing.T, wantSQL, wantErrFragment string) {
	t.Helper()

	ctx := context.Background()

	var matches []store.Query

	require.Eventually(t, func() bool {
		queries, err := e.store.ListQueries(ctx, store.QueryFilter{Limit: 500})
		if err != nil {
			return false
		}

		matches = nil

		for i := range queries {
			if queries[i].SQLText == wantSQL {
				matches = append(matches, queries[i])
			}
		}

		return len(matches) > 0
	}, 30*time.Second, 250*time.Millisecond, "the refused statement %q was never logged", wantSQL)

	require.Len(t, matches, 1, "a refused statement must be logged exactly once")

	row := matches[0]
	assertBlockedOracleRow(t, &row, wantSQL, wantErrFragment)

	conn, err := e.store.GetConnectionByUID(ctx, row.ConnectionID)
	require.NoError(t, err)
	assert.Equal(t, e.user.UID, conn.UserID, "a refusal row joins to its user like any other query")

	result, err := e.store.VerifyQueryChain(ctx, row.ConnectionID)
	require.NoError(t, err)
	assert.Nil(t, result.Break, "a refusal row must be a valid link in the connection's query chain")
	assert.False(t, result.TruncatedPrefix, "nothing was retained away; the chain starts at seq 1")
	assert.Positive(t, result.Verified, "the chain walk must actually cover rows")
}

// pythonRefusalScript drives the same refusal as the go-ora halves above from
// python-oracledb thin, and then keeps using the connection.
//
// It is a script rather than a Go driver for the reason the cursor-learning
// suite gives: what is being measured is how *that* client reads dbbat's frame,
// and there is no Go implementation of that.
const pythonRefusalScript = `
import sys
import oracledb

host, port, service, user, password = sys.argv[1:6]
conn = oracledb.connect(user=user, password=password,
                        dsn=oracledb.makedsn(host, int(port), service_name=service))
cur = conn.cursor()

cur.execute("SELECT 1 FROM dual")
print("read-ok", cur.fetchall()[0][0], flush=True)

# The table does not exist on this fixture, which is the point: a refusal
# never reaches upstream, so the error has to be ORA-01031 and not ORA-00942.

try:
    cur.execute("INSERT INTO dbbat_blocked_probe VALUES (99)")
    print("REFUSAL-MISSING", flush=True)
except oracledb.DatabaseError as e:
    print("refused:", str(e).strip().splitlines()[0], flush=True)

cur.execute("SELECT 42 FROM dual")
print("survived", cur.fetchall()[0][0], flush=True)

conn.close()
print("done", flush=True)
`

// TestIntegration_BlockedStatementRefusesPythonThin is the second client the
// spec asked for. go-ora is what dbbat's own tests are written in, and the
// summary object it parses is not the one python-oracledb parses: measured
// against Oracle 23ai Free, the server gives python-oracledb two extra fields
// before the error text. A frame built for one client and handed to the other
// does not merely lose the message — it hangs the call, which is the bug this
// whole spec is about.
//
// Skipped when python-oracledb is not installed, which is the case in CI; the
// unit fixtures in ttc_oer_encode_test.go carry the same evidence there.
func TestIntegration_BlockedStatementRefusesPythonThin(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	if err := exec.Command("python3", "-c", "import oracledb").Run(); err != nil {
		t.Skip("python-oracledb not installed (pip install oracledb)")
	}

	env := startOracleThroughProxy(t, []string{store.ControlReadOnly})

	script := filepath.Join(t.TempDir(), "refusal.py")
	require.NoError(t, os.WriteFile(script, []byte(pythonRefusalScript), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), refusalDeadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", script,
		env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey)

	out, err := cmd.CombinedOutput()

	require.NoErrorf(t, err,
		"python-oracledb never came back from the refused statement:\n%s", out)

	output := string(out)
	assert.Contains(t, output, "refused: ORA-01031",
		"python-oracledb must receive the refusal as an ORA error:\n%s", output)
	assert.NotContains(t, output, "REFUSAL-MISSING",
		"the write must be refused, not executed:\n%s", output)
	assert.Contains(t, output, "survived 42",
		"the connection must still answer after a refusal:\n%s", output)
	assert.Contains(t, output, "done", "the client must close cleanly:\n%s", output)
}

// sqlplusRefusalScript is the OCI half of the refusal coverage. sqlplus reports
// an ORA error to stderr *and* keeps going, so the surrounding SELECTs are what
// prove the session survived: the last one only prints when the connection is
// still usable after the refusal.
//
// WHENEVER SQLERROR is deliberately NOT set to EXIT — a refusal that killed the
// session would be indistinguishable from one that was merely reported.
const sqlplusRefusalScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'read-ok=' || 1 FROM dual;
INSERT INTO dbbat_blocked_probe VALUES (99);
SELECT 'survived=' || 42 FROM dual;
EXIT
`

// TestIntegration_BlockedStatementRefusesSQLPlus is the third client, and the
// one that needed a third on-wire shape: OCI marshals the summary object as
// fixed-width little-endian integers and terminates its messages with the TTC
// end-of-response marker. A frame built for a thin client — or a byte-perfect
// fixed-width one with no marker — hangs sqlplus exactly the way the old TTC
// Response did, which is what this test is here to catch.
//
// It is the only automated cover for the unlearned fallback in nextOERFrame
// (see encodeOERFixedWidth): that path seeds both halves of the OCI shape from
// the client's AUTH framing, and nothing else exercises it end to end.
//
// Skipped when sqlplus is not installed, which is the case in CI; the fixtures
// in ttc_oer_encode_test.go carry the byte-level half of the same evidence
// there.
func TestIntegration_BlockedStatementRefusesSQLPlus(t *testing.T) {
	sqlplus, err := exec.LookPath("sqlplus")
	if err != nil {
		t.Skipf("sqlplus unavailable: %v", err)
	}

	env := startOracleThroughProxy(t, nil)

	ctx := context.Background()

	// Seed the table under the permissive grant the fixture starts with, so the
	// refusal below is the only thing that can stop the INSERT.
	_, _ = env.db.ExecContext(ctx, "DROP TABLE dbbat_blocked_probe")

	_, err = env.db.ExecContext(ctx, "CREATE TABLE dbbat_blocked_probe (id NUMBER)")
	require.NoError(t, err, "the seed DDL must be allowed under an unrestricted grant")

	env.replaceGrant(t, []string{store.ControlReadOnly})

	script := filepath.Join(t.TempDir(), "refusal.sql")
	require.NoError(t, os.WriteFile(script, []byte(sqlplusRefusalScript), 0o600))

	runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
	defer cancel()

	cmd := exec.CommandContext(runCtx, sqlplus, "-S",
		fmt.Sprintf("%s/%s@//%s:%d/%s", env.username, env.apiKey, env.host, env.port, env.service),
		"@"+script)

	out, err := cmd.CombinedOutput()
	output := string(out)

	require.NoErrorf(t, err, "sqlplus never came back from the refused statement:\n%s", output)

	assert.Contains(t, output, "read-ok=1",
		"reads must still work under read_only:\n%s", output)
	assert.Contains(t, output, "ORA-01031",
		"sqlplus must receive the refusal as an ORA error:\n%s", output)
	assert.Contains(t, output, "survived=42",
		"the connection must still answer after a refusal:\n%s", output)

	assert.Zero(t, env.probeRowCount(t), "the refused INSERT must never have reached upstream")
	env.assertBlockedQueryLogged(t, "INSERT INTO dbbat_blocked_probe VALUES (99)", "read-only")
}
