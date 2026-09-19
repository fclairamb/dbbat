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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// The per-user statement tag against a real Oracle.
//
// Every other test of this feature reasons about bytes. This one is the only
// thing that answers the question that actually matters: **does the server
// accept the frame dbbat rewrote?** The failure mode if it does not is
// `ORA-03146 invalid buffer length for TTC field` and a dead session, so the
// assertions below are deliberately blunt — run the statement, and read the tag
// back out of `V$SQL`, which is where Oracle's own tooling would see it.
//
// `V$SQL` is also what makes the cost argument checkable: it keys on statement
// text, so a tag whose cardinality is the number of dbbat *users* costs k
// cursors rather than one per session. See docs/oracle.md.

// vsqlTextsLike returns every V$SQL entry whose text contains marker, read
// through the proxy (so the lookup itself is a tagged statement, which is fine —
// it just does not match the marker).
func vsqlTextsLike(t *testing.T, db *sql.DB, marker string) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(),
		"SELECT sql_text FROM v$sql WHERE sql_text LIKE :1 AND sql_text NOT LIKE :2",
		"%"+marker+"%", "%v$sql%")
	require.NoError(t, err)

	defer func() { _ = rows.Close() }()

	var out []string

	for rows.Next() {
		var text string

		require.NoError(t, rows.Scan(&text))

		out = append(out, text)
	}

	require.NoError(t, rows.Err())

	return out
}

// recordedStatements returns the sql_text of every `queries` row this fixture's
// user has, which is what the UI, the audit chain and the capture all agree on.
func recordedStatements(t *testing.T, env *oracleThroughProxy) []string {
	t.Helper()

	rows, err := env.store.ListQueries(context.Background(), store.QueryFilter{
		UserID: &env.user.UID,
		Limit:  500,
	})
	require.NoError(t, err)

	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SQLText)
	}

	return out
}

// TestIntegration_StatementTagReachesVSQL is the whole feature, end to end: a
// statement issued through a tagging session arrives at Oracle carrying the tag,
// the server parses and runs it, and what dbbat recorded is the client's own
// text.
func TestIntegration_StatementTagReachesVSQL(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	const marker = "dbbat_tag_probe_one"

	probe := fmt.Sprintf("SELECT 1 AS %s FROM dual", marker)

	var n int

	require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n),
		"the rewritten frame must be one the server accepts (ORA-03146 is the failure this checks for)")
	assert.Equal(t, 1, n)

	texts := vsqlTextsLike(t, env.db, marker)
	require.NotEmpty(t, texts, "the statement must be in V$SQL")

	for _, text := range texts {
		assert.True(t, strings.HasPrefix(text, "/*dbbat='"),
			"Oracle's own view of the statement carries the tag: %q", text)
		assert.Contains(t, text, "user='"+env.username+"'")
		assert.Contains(t, text, "grant='")
		assert.NotContains(t, text, "conn='",
			"the Oracle tag is per user, not per connection — that is the whole cost argument")
	}

	// And the storage invariant: the tag is on the wire and nowhere else.
	recorded := recordedStatements(t, env)
	require.NotEmpty(t, recorded)

	found := false

	for _, sqlText := range recorded {
		assert.NotContains(t, sqlText, "/*dbbat='",
			"no recorded statement may carry the tag")

		if sqlText == probe {
			found = true
		}
	}

	assert.True(t, found, "the probe must be recorded verbatim; got %v", recorded)
}

// TestIntegration_StatementTagCostsOneCursorPerUser is the measurement the whole
// feature rests on, asserted rather than remembered: the same statement executed
// many times over one identity is one SQL_ID, not one per execution.
func TestIntegration_StatementTagCostsOneCursorPerUser(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	const marker = "dbbat_tag_probe_cardinality"

	probe := fmt.Sprintf("SELECT 1 AS %s FROM dual", marker)

	for range 25 {
		var n int

		require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n))
	}

	texts := vsqlTextsLike(t, env.db, marker)
	assert.Len(t, texts, 1,
		"25 executions of one statement under one identity must be one shared-pool entry, got %v", texts)
}

// TestIntegration_StatementTagCrossesTheCLRBoundary is the encoding case the
// spec singles out: a statement sitting just under the 252-byte CLR short-form
// limit, which the ~50-byte tag pushes into the 0xFE-chunked long form. Getting
// that wrong is the ORA-03146 case, so it gets a real server.
func TestIntegration_StatementTagCrossesTheCLRBoundary(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	for _, pad := range []int{150, 200, 210, 220, 240, 245} {
		marker := fmt.Sprintf("dbbat_clr_probe_%d", pad)
		probe := fmt.Sprintf("SELECT 1 AS %s /* %s */ FROM dual",
			marker, strings.Repeat("p", pad))

		var n int

		require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n),
			"pad=%d (statement %d bytes) must be accepted", pad, len(probe))
		assert.Equal(t, 1, n)

		texts := vsqlTextsLike(t, env.db, marker)
		require.NotEmpty(t, texts, "pad=%d", pad)
		assert.True(t, strings.HasPrefix(texts[0], "/*dbbat='"), "pad=%d: %q", pad, texts[0])
	}
}

// TestIntegration_StatementTagPastTheSDU covers the other growth axis: a message
// already at the negotiated session data unit cannot absorb the tag in place, so
// the outgoing packets are re-cut. The statement is deliberately larger than the
// 8192-byte default.
func TestIntegration_StatementTagPastTheSDU(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	const marker = "dbbat_sdu_probe"

	probe := fmt.Sprintf("SELECT 1 AS %s /* %s */ FROM dual", marker, strings.Repeat("s", 20000))
	require.Greater(t, len(probe), 16384)

	var n int

	require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n),
		"a statement spanning several TNS packets must survive being re-cut")
	assert.Equal(t, 1, n)

	// V$SQL truncates SQL_TEXT at 1000 characters, so the tag — which is at the
	// front, deliberately, precisely because every such view truncates — is
	// still visible.
	texts := vsqlTextsLike(t, env.db, marker)
	require.NotEmpty(t, texts)
	assert.True(t, strings.HasPrefix(texts[0], "/*dbbat='"), "%q", texts[0])
}

// TestIntegration_StatementTaggingOffLeavesVSQLAlone is the default path, on a
// real server: with the setting off not one byte changes, so Oracle sees exactly
// what the client sent.
func TestIntegration_StatementTaggingOffLeavesVSQLAlone(t *testing.T) {
	env := startOracleThroughProxy(t, nil)

	const marker = "dbbat_untagged_probe"

	probe := fmt.Sprintf("SELECT 1 AS %s FROM dual", marker)

	var n int

	require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n))

	texts := vsqlTextsLike(t, env.db, marker)
	require.NotEmpty(t, texts)

	for _, text := range texts {
		assert.NotContains(t, text, "dbbat=",
			"with tagging off the server must see the client's own statement: %q", text)
	}
}

// --- how long a statement may be --------------------------------------------
//
// The tag grows a statement by ~50 bytes, and everything in the rewriter
// accounts for that growth except the one thing dbbat does not own: how long a
// statement the *server* will parse. If there were a band just under Oracle's
// limit, turning `DBB_QUERY_TAGGING_ORACLE=user` on would stop a statement that
// ran yesterday — and the error would come from Oracle, about a statement whose
// `queries` row is the client's own untagged text.
//
// The two tests below are that question and its answer. The first walks the
// length up against a real server and finds **no such band**: 23ai parses far
// more than dbbat will ever hand it (128 MB in the exploratory run; see
// docs/oracle.md). The second pins the bound that does bind, which is dbbat's
// own — `maxTaggableStatementBytes`, so that a statement dbbat tags never
// declares a length dbbat itself would refuse to read.

// ceilingWalkCap bounds the doubling walk.
//
// Twice `execMaxSQLLen` is the assertion's whole point rather than a budget:
// everything dbbat can tag is at or below that bound, so a walk that gets past
// it without a refusal has shown that no statement dbbat tags can be one Oracle
// declines for its length. CEILING_WALK_CAP raises it for a one-off
// exploration — that is how the 128 MB figure was taken, and each doubling past
// here costs real seconds.
var ceilingWalkCap = 2 * execMaxSQLLen

// statementOfLength builds a statement of exactly n bytes carrying marker near
// the front, so V$SQL's 1000-character SQL_TEXT still shows it.
func statementOfLength(marker string, n int) string {
	head := fmt.Sprintf("SELECT 1 AS %s /* ", marker)

	const tail = " */ FROM dual"

	pad := n - len(head) - len(tail)
	if pad < 0 {
		panic("statementOfLength: n below the fixed part")
	}

	return head + strings.Repeat("q", pad) + tail
}

// runsAtLength executes a statement of exactly n bytes and reports whether the
// server accepted it, plus whatever it said when it did not.
func runsAtLength(t *testing.T, db *sql.DB, marker string, n int) (bool, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var got int

	err := db.QueryRowContext(ctx, statementOfLength(marker, n)).Scan(&got)
	if err != nil {
		return false, err
	}

	require.Equal(t, 1, got)

	return true, nil
}

// measureStatementCeiling walks statement length upward against the live server
// — doubling until one is refused, then bisecting to the byte — and returns the
// longest statement accepted, the shortest refused (0 when none was) and the
// server's own words for the refusal.
//
// Every statement goes through a proxy with tagging **off**, so what is measured
// is Oracle's limit and not dbbat's.
func measureStatementCeiling(t *testing.T, db *sql.DB) (accepted, refused int, refusal error) {
	t.Helper()

	// 20 KB is what TestIntegration_StatementTagPastTheSDU already runs, so the
	// walk starts from a length known to work rather than from nothing.
	accepted = 20480
	probe := 0

	for n := 32768; n <= ceilingWalkCap; n *= 2 {
		probe++

		ok, err := runsAtLength(t, db, fmt.Sprintf("dbbat_ceil_up_%d", probe), n)
		t.Logf("ceiling walk: %d bytes -> accepted=%v err=%v", n, ok, err)

		if !ok {
			refused, refusal = n, err

			break
		}

		accepted = n
	}

	if refused == 0 {
		return accepted, 0, nil
	}

	for refused-accepted > 1 {
		mid := accepted + (refused-accepted)/2
		probe++

		ok, err := runsAtLength(t, db, fmt.Sprintf("dbbat_ceil_bis_%d", probe), mid)
		if ok {
			accepted = mid
		} else {
			refused, refusal = mid, err
		}
	}

	return accepted, refused, refusal
}

// TestIntegration_OracleStatementLengthCeiling is the measurement that settled
// the question, kept as a test rather than run once and written down.
//
// What it found is a negative, and the negative is the useful part: there is no
// statement-length band where prepending the tag turns a working statement into
// an Oracle error, because Oracle's limit is nowhere near anything dbbat will
// ever send. The walk is capped at twice dbbat's own reading bound for run time;
// with CEILING_WALK_CAP raised it went to **128 MB on 23ai Free without a single
// refusal**, which is the figure docs/oracle.md records.
//
// So this test fails in exactly one situation: a server — a future release, a
// different edition, an Oracle-compatible thing behind the proxy — that refuses
// a statement dbbat would have been willing to tag. That is the day
// maxTaggableStatementBytes stops being dbbat's number and has to become the
// server's.
func TestIntegration_OracleStatementLengthCeiling(t *testing.T) {
	env := startOracleThroughProxy(t, nil)

	if v := os.Getenv("CEILING_WALK_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err)

		ceilingWalkCap = n
	}

	accepted, refused, refusal := measureStatementCeiling(t, env.db)

	t.Logf("image=%s: longest statement accepted %d bytes, shortest refused %d (0 = none under the "+
		"%d-byte cap), refusal: %v", oracleTestImage(), accepted, refused, ceilingWalkCap, refusal)

	assert.GreaterOrEqual(t, accepted, maxTaggableStatementBytes,
		"this server parses less than dbbat is willing to tag (%d bytes), so tagging can turn a "+
			"working statement into an error Oracle attributes to the client: "+
			"maxTaggableStatementBytes has to come down to the server's ceiling. First refusal at "+
			"%d bytes: %v", maxTaggableStatementBytes, refused, refusal)
}

// taggedPrefixLength reads the tag's own width off a live session, rather than
// recomputing it here from the version, the username and the grant slug — the
// point of the probes below is *where* the boundary falls, so the width that
// places it has to be the one this fixture actually emits.
func taggedPrefixLength(t *testing.T, env *oracleThroughProxy) int {
	t.Helper()

	const marker = "dbbat_tagwidth_probe"

	var n int

	require.NoError(t, env.db.QueryRowContext(context.Background(),
		fmt.Sprintf("SELECT 1 AS %s FROM dual", marker)).Scan(&n))

	texts := vsqlTextsLike(t, env.db, marker)
	require.NotEmpty(t, texts)

	end := strings.Index(texts[0], "*/ ")
	require.Positive(t, end, "the tagged text must carry a closed comment: %q", texts[0])

	return end + len("*/ ")
}

// TestIntegration_StatementTagStopsAtDbbatsOwnBound is the rule, live and to the
// byte: the longest statement that can absorb the tag is tagged, and the very
// next one is forwarded untagged — while still running, and still being recorded
// verbatim.
//
// Before the rule existed this band was a real hole rather than a theoretical
// one. Measured on this same fixture: a statement of exactly execMaxSQLLen bytes
// was tagged, which put a TTC length field declaring 1 048 634 bytes on the
// upstream wire — a length dbbat's own decoders call implausible and would
// refuse to read back. Oracle did not mind; dbbat's invariant is the thing that
// was broken.
func TestIntegration_StatementTagStopsAtDbbatsOwnBound(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	tagLen := taggedPrefixLength(t, env)
	t.Logf("this fixture's tag is %d bytes", tagLen)

	for _, tc := range []struct {
		name      string
		length    int
		wantTag   bool
		assertion string
	}{
		{
			name:      "the last statement that fits tagged",
			length:    maxTaggableStatementBytes - tagLen,
			wantTag:   true,
			assertion: "a statement whose tagged length lands exactly on the bound must still be tagged",
		},
		{
			name:    "one byte further",
			length:  maxTaggableStatementBytes - tagLen + 1,
			wantTag: false,
			assertion: "one byte past it must not be — tagging it would declare a length dbbat " +
				"itself would refuse to read",
		},
	} {
		marker := fmt.Sprintf("dbbat_bound_probe_%d", tc.length)

		probe := statementOfLength(marker, tc.length)
		require.Len(t, probe, tc.length)

		var n int

		require.NoErrorf(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n),
			"%s: the statement must run either way — the rule changes the tag, never the outcome", tc.name)
		assert.Equal(t, 1, n)

		texts := vsqlTextsLike(t, env.db, marker)
		require.NotEmptyf(t, texts, "%s: the statement must be in V$SQL", tc.name)

		assert.Equalf(t, tc.wantTag, strings.HasPrefix(texts[0], "/*dbbat='"),
			"%s (%d bytes): %s — V$SQL shows %q", tc.name, tc.length, tc.assertion,
			truncateSQL(texts[0], 80))

		// And either way the statement dbbat recorded is the client's own, which
		// is what makes the untagged case forgivable: the `queries` row and the
		// audit chain do not change with the tag.
		recorded := recordedStatements(t, env)
		require.NotEmpty(t, recorded)

		for _, sqlText := range recorded {
			assert.NotContains(t, sqlText, "/*dbbat='", "%s: no recorded statement may carry the tag", tc.name)
		}
	}
}

// --- the other client shapes ------------------------------------------------
//
// The tests above drive go-ora, which is one of the three on-wire shapes the
// locator covers. These two cover the other two: the OCI wide header (sqlplus,
// `sqlLen * 3` as a little-endian ub4, the trailing NUL inside the declared
// length) and python-oracledb thin. A shape that regressed here would show up as
// a session that quietly stopped tagging — or, if the rewrite were wrong rather
// than refused, as ORA-03146 / ORA-03120.

// sqlplusTagProbeScript runs a marked statement and then looks for it in V$SQL.
//
// The lookup's own text is built with `||` so that it does not itself contain
// the marker — otherwise the lookup would match itself and the count would be
// about the wrong statement.
const sqlplusTagProbeScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'probe=' || 1 FROM dual WHERE 'dbbat_oci_probe' = 'dbbat_oci_probe';
SELECT 'tagged=' || COUNT(*) FROM v$sql WHERE sql_text LIKE '%dbbat' || '_oci_probe%' AND sql_text LIKE '/*dbbat=''%';
EXIT
`

// TestIntegration_StatementTagFromOCIClient is the OCI wide header on a real
// server: a different length encoding (`sqlLen * 3`, little-endian ub4) and a
// declared length that sometimes counts a trailing NUL.
func TestIntegration_StatementTagFromOCIClient(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{
		statementTagging:        true,
		reachableFromContainers: plannedOCIClient() == ociClientContainer,
	})

	client := requireOCIClient(t, env)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := client.run(t, ctx, sqlplusTagProbeScript)
	require.NoErrorf(t, err, "sqlplus through a tagging proxy:\n%s", out)

	assert.NotContains(t, out, "ORA-03146",
		"a wrong TTC length field is what this whole design exists to avoid:\n%s", out)
	assert.NotContains(t, out, "ORA-03120",
		"a desynchronized message reads as a conversion overflow:\n%s", out)
	assert.Contains(t, out, "probe=1", "the statement must have run:\n%s", out)
	assert.Contains(t, out, "tagged=1",
		"Oracle's own view of the OCI client's statement must carry the tag:\n%s", out)
}

// pythonTagProbeScript is the same probe through python-oracledb thin.
const pythonTagProbeScript = `import sys
import oracledb

host, port, service, user, key = sys.argv[1:6]
conn = oracledb.connect(user=user, password=key, dsn="%s:%s/%s" % (host, port, service))
cur = conn.cursor()

cur.execute("SELECT 'probe=' || 1 FROM dual WHERE 'dbbat_py_probe' = 'dbbat_py_probe'")
print(cur.fetchone()[0])

cur.execute("SELECT sql_text FROM v$sql WHERE sql_text LIKE '%dbbat' || '_py_probe%'")
rows = [r[0] for r in cur.fetchall()]
for r in rows:
    print("VSQL:", r[:100])
print("tagged=%d" % sum(1 for r in rows if r.startswith("/*dbbat='")))

conn.close()
print("done")
`

// TestIntegration_StatementTagFromPythonThin is the third recorded client shape
// against a real server.
func TestIntegration_StatementTagFromPythonThin(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	if err := exec.Command("python3", "-c", "import oracledb").Run(); err != nil {
		t.Skip("python-oracledb not installed (pip install oracledb)")
	}

	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	script := filepath.Join(t.TempDir(), "tagprobe.py")
	require.NoError(t, os.WriteFile(script, []byte(pythonTagProbeScript), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "python3", script,
		env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey).CombinedOutput()

	output := string(out)
	require.NoErrorf(t, err, "python-oracledb through a tagging proxy:\n%s", output)

	assert.NotContains(t, output, "ORA-03146", "%s", output)
	assert.NotContains(t, output, "ORA-03120", "%s", output)
	assert.Contains(t, output, "probe=1", "%s", output)
	assert.Contains(t, output, "tagged=1",
		"Oracle's own view of the thin client's statement must carry the tag:\n%s", output)
	assert.Contains(t, output, "done", "%s", output)
}

// --- the fourth shape: ojdbc thin (and sqlcl, which is ojdbc thin) ----------
//
// go-ora and python-oracledb both write the statement length as a compressed
// int followed by a short-form CLR prefix. ojdbc thin does not: it is the
// `bare` shape, where the header's length field is the statement's *only*
// declared length and no CLR prefix repeats it. That is the shape where a
// second copy of the length hiding elsewhere in the frame would be caught by
// nothing — the corpus test proves the frames dbbat recorded, and only a live
// run proves the frames ojdbc sends today.
//
// The jar is resolved by oracleTestOJDBCJar (blocked_integration_test.go):
// ORACLE_TEST_OJDBC_JAR, or an ojdbc jar on CLASSPATH. CI fetches one and
// exports the variable, and with it set neither a missing jar nor a missing JVM
// is allowed to become a skip — the whole point of asking for this coverage is
// getting it.

// jdbcTagProgram runs a marked statement through the proxy, re-executes a
// second marked statement 25 times off one prepared handle, and then reads both
// back out of V$SQL — which is where Oracle's own tooling would see the tag.
//
// It is run through `java Tag.java` (single-file source mode), so no build
// tooling beyond a JDK is involved; the file must therefore be named Tag.java.
//
// Every V$SQL lookup splits its own marker with `||` so that the lookup does
// not match itself: after `dbbat` the text carries `' || '`, which no
// `%dbbat_jdbc_probe%` pattern spans.
const jdbcTagProgram = `import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.Statement;

public class Tag {
    public static void main(String[] args) throws Exception {
        String url = String.format("jdbc:oracle:thin:@//%s:%s/%s", args[0], args[1], args[2]);

        try (Connection conn = DriverManager.getConnection(url, args[3], args[4])) {
            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery(
                     "SELECT 'probe=' || 1 FROM dual WHERE 'dbbat_jdbc_probe' = 'dbbat_jdbc_probe'")) {
                rs.next();
                System.out.println(rs.getString(1));
            }

            // The per-user cardinality claim on a client that prepares once and
            // re-executes rather than re-parsing: 25 executions, one SQL_ID.
            try (PreparedStatement ps = conn.prepareStatement(
                     "SELECT 'card=' || 1 FROM dual WHERE 'dbbat_jdbc_card' = 'dbbat_jdbc_card'")) {
                for (int i = 0; i < 25; i++) {
                    try (ResultSet rs = ps.executeQuery()) {
                        rs.next();
                    }
                }
            }

            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery(
                     "SELECT sql_text FROM v$sql WHERE sql_text LIKE '%dbbat' || '_jdbc_probe%'")) {
                int tagged = 0;
                int seen = 0;
                while (rs.next()) {
                    String text = rs.getString(1);
                    seen++;
                    System.out.println("VSQL: " + text.substring(0, Math.min(100, text.length())));
                    if (text.startsWith("/*dbbat='")) {
                        tagged++;
                    }
                }
                System.out.println("seen=" + seen);
                System.out.println("tagged=" + tagged);
            }

            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery(
                     "SELECT COUNT(DISTINCT sql_id) FROM v$sql WHERE sql_text LIKE '%dbbat' || '_jdbc_card%'")) {
                rs.next();
                System.out.println("cards=" + rs.getInt(1));
            }
        }

        System.out.println("done");
    }
}
`

// jdbcTagDeadline is the probe's budget. It is a deadlock detector rather than
// a performance bound — a tagged statement that comes back at all comes back in
// a round trip — with room for the JVM: single-file source mode compiles
// Tag.java in-process before running it, and the JDBC driver's connect is
// slower to start than a thin client's.
const jdbcTagDeadline = 3 * time.Minute

// TestIntegration_StatementTagFromJDBCThin is the fourth client shape on a real
// server, and the one the feature shipped without: `bare`, where the header's
// length field is the statement's only declared length.
//
// It also carries the cardinality half of the cost argument for this client
// class specifically. TestIntegration_StatementTagCostsOneCursorPerUser makes
// the same measurement through go-ora, which re-sends the statement text on
// every execution; JDBC prepares once and re-executes, so a tag that varied per
// execution — or a rewrite applied inconsistently across a session — would show
// up here as more than one SQL_ID.
func TestIntegration_StatementTagFromJDBCThin(t *testing.T) {
	java := requireTestJava(t)
	if java == "" {
		t.Skip("java not available")
	}

	jar := oracleTestOJDBCJar(t)
	if jar == "" {
		t.Skipf("no Oracle JDBC driver: set %s to an ojdbc jar, or put one on CLASSPATH", ojdbcJarEnv)
	}

	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	program := filepath.Join(t.TempDir(), "Tag.java")
	require.NoError(t, os.WriteFile(program, []byte(jdbcTagProgram), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), jdbcTagDeadline)
	defer cancel()

	out, err := exec.CommandContext(ctx, java, "-cp", jar, program,
		env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey).CombinedOutput()

	output := string(out)
	require.NoErrorf(t, err, "JDBC thin through a tagging proxy:\n%s", output)

	assert.NotContains(t, output, "ORA-03146",
		"a wrong TTC length field is what this whole design exists to avoid:\n%s", output)
	assert.NotContains(t, output, "ORA-03120",
		"a desynchronized message reads as a conversion overflow:\n%s", output)
	assert.Contains(t, output, "probe=1", "the statement must have run:\n%s", output)
	assert.Contains(t, output, "tagged=1",
		"Oracle's own view of the JDBC thin client's statement must carry the tag — the `bare` "+
			"shape is the one where the header length is the statement's only declared length:\n%s", output)
	assert.Contains(t, output, "cards=1",
		"25 executions of one prepared statement under one identity must be one shared-pool "+
			"entry, which is the per-user cardinality claim:\n%s", output)
	assert.Contains(t, output, "done", "the client must close cleanly:\n%s", output)

	// And the storage invariant, read from dbbat's own side: the tag is on the
	// wire and nowhere else.
	for _, sqlText := range recordedStatements(t, env) {
		assert.NotContains(t, sqlText, "/*dbbat='", "no recorded statement may carry the tag")
	}
}

// sqlclEnv points the sqlcl probe below at a SQLcl launcher (`sql`, or `sql.exe`
// on Windows). Like the ojdbc jar there is nothing to look up — SQLcl is a
// downloaded zip or a Homebrew cask, not a packaged driver — so the knob is an
// explicit path, with a `sql` on PATH honoured when it identifies itself as
// SQLcl.
const sqlclEnv = "ORACLE_TEST_SQLCL"

// sqlclTagProbeScript is the sqlplus probe's wording through SQLcl: run a
// marked statement, then count the tagged V$SQL entries for it.
//
// The lookup splits its own marker with `||` for the same reason the sqlplus
// one does — otherwise it matches itself and the count is about the wrong
// statement.
const sqlclTagProbeScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'probe=' || 1 FROM dual WHERE 'dbbat_sqlcl_probe' = 'dbbat_sqlcl_probe';
SELECT 'tagged=' || COUNT(*) FROM v$sql WHERE sql_text LIKE '%dbbat' || '_sqlcl_probe%' AND sql_text LIKE '/*dbbat=''%';
EXIT
`

// oracleTestSQLcl resolves a SQLcl launcher, or "" when this machine has none.
//
// A sqlclEnv that points at something which is not there is a failure rather
// than a skip, on the same rule as ojdbcJarEnv: it is someone asking for this
// coverage and not getting it. A bare `sql` on PATH is a common enough name to
// be something else entirely (a shell alias, another vendor's tool), so it has
// to say `SQLcl` when asked for its version before it is believed.
func oracleTestSQLcl(t *testing.T) string {
	t.Helper()

	if path := os.Getenv(sqlclEnv); path != "" {
		_, err := os.Stat(path)
		require.NoErrorf(t, err, "%s points at a launcher that is not there", sqlclEnv)

		return path
	}

	sqlcl, err := exec.LookPath("sql")
	if err != nil {
		return ""
	}

	out, err := exec.Command(sqlcl, "-V").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SQLcl") {
		return ""
	}

	return sqlcl
}

// TestIntegration_StatementTagFromSQLcl is the same wire shape as the JDBC test
// above — SQLcl is ojdbc thin under the hood — so what it buys is client
// *version* coverage: SQLcl ships its own bundled driver, generally newer than
// whatever jar a developer or CI happens to have, and it is the client the
// `bare` corpus fixtures were captured from (sqlcl_regression_test.go).
//
// Skipped when no SQLcl is reachable, which is most machines and every CI
// runner today; the jar-driven test above is the one CI is wired for.
func TestIntegration_StatementTagFromSQLcl(t *testing.T) {
	sqlcl := oracleTestSQLcl(t)
	if sqlcl == "" {
		t.Skipf("no SQLcl: set %s to a `sql` launcher, or put one on PATH", sqlclEnv)
	}

	t.Logf("SQLcl: %s", sqlcl)

	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	script := filepath.Join(t.TempDir(), "sqlcl_tag_probe.sql")
	require.NoError(t, os.WriteFile(script, []byte(sqlclTagProbeScript), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), jdbcTagDeadline)
	defer cancel()

	out, err := exec.CommandContext(ctx, sqlcl, "-S",
		env.ociConnectStringAt(env.host, env.port), "@"+script).CombinedOutput()

	output := string(out)
	require.NoErrorf(t, err, "SQLcl through a tagging proxy:\n%s", output)

	assert.NotContains(t, output, "ORA-03146", "%s", output)
	assert.NotContains(t, output, "ORA-03120", "%s", output)
	assert.Contains(t, output, "probe=1", "the statement must have run:\n%s", output)
	assert.Contains(t, output, "tagged=1",
		"Oracle's own view of SQLcl's statement must carry the tag:\n%s", output)
}
