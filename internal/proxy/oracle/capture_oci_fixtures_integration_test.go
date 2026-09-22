//go:build integration

package oracle

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ociFixtureCaptureEnv opts a run into re-recording the OCI hex fixtures. It is
// off by default because this is capture tooling wearing the integration tag,
// not a test: it rewrites files under testdata/ that every other test in the
// package is pinned against.
const ociFixtureCaptureEnv = "ORACLE_CAPTURE_OCI_FIXTURES"

// TestCapture_OCIFixturesThroughDBBat re-records the OCI REF-cursor fixture
// pair, and the scalar-out-bind negative beside it, from a session that goes
// **through dbbat** rather than through a bare relay.
//
//	ORACLE_CAPTURE_OCI_FIXTURES=1 ORACLE_TEST_OCI_CLIENT=container \
//	  go test -tags integration -run TestCapture_OCIFixturesThroughDBBat ./internal/proxy/oracle/
//
// It exists because on the 64-bit OCI dialect those are not the same bytes —
// see ociFixtureProvenance, which is the measurement, not the preference. The
// standalone harness in capture_refcursor_test.go stays the cheaper route for
// the 4-byte dialect and for anything about what a client marshals; this one is
// the route for anything dbbat has to *read*.
//
// The recording tap sits in front of the proxy, so what lands in the fixture is
// byte-for-byte what interceptClientMessage receives. Which fixture pair it
// lands in is decided by the recorded frames themselves (recordedDialectIsWide64),
// never by which client was asked for.
func TestCapture_OCIFixturesThroughDBBat(t *testing.T) {
	if os.Getenv(ociFixtureCaptureEnv) != "1" {
		t.Skipf("set %s=1 to re-record the OCI fixtures", ociFixtureCaptureEnv)
	}

	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	ctx := context.Background()

	_, err := env.db.ExecContext(ctx, refCursorCaptureProcedure)
	require.NoError(t, err)

	defer func() { _, _ = env.db.ExecContext(ctx, "DROP PROCEDURE dbbat_cap_refcur") }()

	_, err = env.db.ExecContext(ctx, scalarOutBindCaptureProcedure)
	require.NoError(t, err)

	defer func() { _, _ = env.db.ExecContext(ctx, "DROP PROCEDURE dbbat_cap_scalarout") }()

	// Three calls rather than two, with a statement of its own between them: the
	// server hands out a fresh cursor per OPEN but happily reuses the id it just
	// freed, and a pairing where every id is the same number is a pairing that
	// would survive a locator latching onto the first one. The intervening
	// SELECT takes a cursor out of circulation so the ids actually differ.
	refCursorDump := recordOCIScriptThroughProxy(t, env, oci, "capture-oci-refcursor", `SET PAGESIZE 0
SET FEEDBACK OFF
VARIABLE rc REFCURSOR
BEGIN dbbat_cap_refcur(:rc); END;
/
PRINT rc
SELECT 'spacer-a' FROM dual;
BEGIN dbbat_cap_refcur(:rc); END;
/
PRINT rc
SELECT 'spacer-b' FROM dual;
BEGIN dbbat_cap_refcur(:rc); END;
/
PRINT rc
EXIT
`)

	scalarDump := recordOCIScriptThroughProxy(t, env, oci, "capture-oci-scalarout", `SET PAGESIZE 0
SET FEEDBACK OFF
VARIABLE n NUMBER
VARIABLE s VARCHAR2(32)
VARIABLE m NUMBER
BEGIN dbbat_cap_scalarout(:n, :s, :m); END;
/
BEGIN dbbat_cap_scalarout(:n, :s, :m); END;
/
PRINT n
EXIT
`)

	for _, ddl := range ociDescribeObjectTypes {
		_, err = env.db.ExecContext(ctx, ddl)
		require.NoErrorf(t, err, "creating the describe query's object types: %s", ddl)
	}

	defer func() {
		for _, name := range ociDescribeObjectTypeNames {
			_, _ = env.db.ExecContext(ctx, "DROP TYPE "+name)
		}
	}()

	describeDump := recordOCIScriptThroughProxy(t, env, oci, "capture-oci-describe", `SET PAGESIZE 0
SET FEEDBACK OFF
`+ociDescribeQuery+`
`+ociDescribeTypedQuery+`
EXIT
`)

	bindOutputs, drives, scalars := ociRefCursorBindOutputFixture, ociRefCursorDrivesFixture, ociScalarOutBindFixture
	describe := ociDescribeFixture
	wide64 := recordedDialectIsWide64(t, refCursorDump)

	if wide64 {
		bindOutputs, drives, scalars =
			oci64RefCursorBindOutputFixture, oci64RefCursorDrivesFixture, oci64ScalarOutBindFixture
		describe = oci64DescribeFixture
	}

	writeBindOutputHexFixture(t, refCursorDump, bindOutputs, drives)
	writeBindOutputHexFixture(t, scalarDump, scalars, "")
	writeDescribeHexFixture(t, describeDump, describe)

	if wide64 {
		writeStatementFrameHexFixture(t, refCursorDump, oci64ParseExecsFixture, "dbbat_cap_refcur")
	}
}

// TestCapture_OCILOBFetchThroughDBBat re-records the LOB fetch fixture.
//
//	ORACLE_CAPTURE_OCI_FIXTURES=1 ORACLE_TEST_OCI_CLIENT=container \
//	  go test -tags integration -run TestCapture_OCILOBFetchThroughDBBat ./internal/proxy/oracle/
//
// It is a capture of its own rather than a fourth script in the test above
// because it needs nothing that one sets up — no procedures, no object types —
// and because the fixture it writes is the one a failure in any of the others
// must not take down with it.
func TestCapture_OCILOBFetchThroughDBBat(t *testing.T) {
	if os.Getenv(ociFixtureCaptureEnv) != "1" {
		t.Skipf("set %s=1 to re-record the OCI fixtures", ociFixtureCaptureEnv)
	}

	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	lobDump := recordOCIScriptThroughProxy(t, env, oci, "capture-oci-lob", `SET PAGESIZE 0
SET FEEDBACK OFF
`+ociLOBQuery+`
EXIT
`)

	fixture := ociLOBFixture
	if recordedDialectIsWide64(t, lobDump) {
		fixture = oci64LOBFixture
	}

	writeLOBFetchHexFixture(t, lobDump, fixture)
}

// TestCapture_OCILongFetchThroughDBBat records a fetch whose select list
// carries a genuine LONG and a genuine LONG RAW column, on whichever OCI
// dialect the client speaks.
//
//	ORACLE_CAPTURE_OCI_FIXTURES=1 \
//	  go test -tags integration -run TestCapture_OCILongFetchThroughDBBat ./internal/proxy/oracle/
//
// It is the OCI half of the LONG evidence, and it had to be recorded rather
// than assumed: both OCI dialects frame a *LOB* column their own way, two bytes
// apart from each other and nothing like the thin one, so there was no reason
// to expect them to agree with thin about a LONG one either.
//
// Unlike every other capture here it needs tables — Oracle allows one LONG
// column per table and none in a `FROM dual` expression list — so it builds
// them through the proxy's own go-ora connection first.
func TestCapture_OCILongFetchThroughDBBat(t *testing.T) {
	if os.Getenv(ociFixtureCaptureEnv) != "1" {
		t.Skipf("set %s=1 to re-record the OCI fixtures", ociFixtureCaptureEnv)
	}

	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	ctx := context.Background()

	for _, ddl := range ociLongTableDropDDL {
		if _, err := env.db.ExecContext(ctx, ddl); err != nil {
			t.Logf("%s: %v (expected on a first run)", ddl, err)
		}
	}

	for _, ddl := range ociLongTableDDL {
		_, err := env.db.ExecContext(ctx, ddl)
		require.NoErrorf(t, err, "building the LONG tables: %s", ddl)
	}

	defer func() {
		for _, ddl := range ociLongTableDropDDL {
			_, _ = env.db.ExecContext(ctx, ddl)
		}
	}()

	dump := recordOCIScriptThroughProxy(t, env, oci, "capture-oci-long", `SET PAGESIZE 0
SET FEEDBACK OFF
SET LONG 200
`+ociLongQuery+`
`+ociLongRawQuery+`
EXIT
`)

	fixture := ociLongFixture
	if recordedDialectIsWide64(t, dump) {
		fixture = oci64LongFixture
	}

	writeLongFetchHexFixture(t, dump, fixture)
}

// ociLongTableDropDDL / ociLongTableDDL and ociLongQuery / ociLongRawQuery are
// longTableDDL and longQuery spelled for sqlplus — the same two tables, the
// same two rows apiece, the same alternation of six-character scalars around
// the column under test — with the trailing semicolon sqlplus needs and go-ora
// reads as a syntax error. They are duplicated rather than shared for the
// reason the capture procedures above are: that file is behind the `capture`
// tag and this one is behind `integration`.
var (
	ociLongTableDropDDL = []string{
		`DROP TABLE dbbat_cap_long`,
		`DROP TABLE dbbat_cap_longraw`,
	}

	ociLongTableDDL = []string{
		`CREATE TABLE dbbat_cap_long (n NUMBER, l1 LONG)`,
		`CREATE TABLE dbbat_cap_longraw (n NUMBER, r1 LONG RAW)`,
		`INSERT INTO dbbat_cap_long VALUES (1, 'longvalue-0123456789')`,
		`INSERT INTO dbbat_cap_long VALUES (2, NULL)`,
		`INSERT INTO dbbat_cap_longraw VALUES (1, HEXTORAW('DEADBEEF'))`,
		`INSERT INTO dbbat_cap_longraw VALUES (2, NULL)`,
	}
)

const (
	ociLongQuery = `SELECT 'aaaaaa' AS c1, l1, 'bbbbbb' AS c2, 'cccccc' AS c3
  FROM dbbat_cap_long ORDER BY n;`

	ociLongRawQuery = `SELECT 'dddddd' AS c4, r1, 'eeeeee' AS c5, 'ffffff' AS c6
  FROM dbbat_cap_longraw ORDER BY n;`
)

// TestCapture_OCISevenColumnFetchThroughDBBat re-records the seven-column
// describe, in whichever OCI dialect the client speaks.
//
//	ORACLE_CAPTURE_OCI_FIXTURES=1 \
//	  go test -tags integration -run TestCapture_OCISevenColumnFetchThroughDBBat ./internal/proxy/oracle/
//
// It is a capture of its own for the same reason the LOB one is: it needs none
// of the procedures or object types the fixture test above sets up, and the
// column count is the entire point of the frames it keeps — folding it into a
// script that also runs the type-rich describes would put a second query's
// frames in the same file and leave "which one is the seven" to a reader.
func TestCapture_OCISevenColumnFetchThroughDBBat(t *testing.T) {
	if os.Getenv(ociFixtureCaptureEnv) != "1" {
		t.Skipf("set %s=1 to re-record the OCI fixtures", ociFixtureCaptureEnv)
	}

	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	dump := recordOCIScriptThroughProxy(t, env, oci, "capture-oci-sevencols", `SET PAGESIZE 0
SET FEEDBACK OFF
`+sevenColumnQuery+`;
EXIT
`)

	fixture := ociSevenColumnFixture
	if recordedDialectIsWide64(t, dump) {
		fixture = oci64SevenColumnFixture
	}

	writeDescribeHexFixture(t, dump, fixture)
}

// refCursorCaptureProcedure and scalarOutBindCaptureProcedure are the two
// procedures the hex fixtures are recorded against. They are spelled out here
// rather than shared with capture_refcursor_test.go because that file is behind
// the `capture` tag and this one is behind `integration`; the bodies have to
// stay identical, which is what the comment is for.
const (
	refCursorCaptureProcedure = `CREATE OR REPLACE PROCEDURE dbbat_cap_refcur(p OUT SYS_REFCURSOR) AS
BEGIN
  OPEN p FOR SELECT LEVEL AS n, 'row-' || LEVEL AS label FROM dual CONNECT BY LEVEL <= 5;
END;`

	scalarOutBindCaptureProcedure = `CREATE OR REPLACE PROCEDURE dbbat_cap_scalarout(
  n OUT NUMBER, s OUT VARCHAR2, m OUT NUMBER) AS
BEGIN
  n := 7;
  s := 'seven';
  m := 42;
END;`
)

// recordOCIScriptThroughProxy runs one sqlplus script against a recording relay
// placed in front of the proxy, and returns the recording's path.
func recordOCIScriptThroughProxy(
	t *testing.T, env *oracleThroughProxy, oci *ociClient, sessionID, script string,
) string {
	t.Helper()

	dir := captureFixtureDir(t)
	outPath := filepath.Join(dir, sessionID+".pcapng")

	w := newCaptureWriter(t, outPath, sessionID)

	bind := "127.0.0.1"
	if oci.kind == ociClientContainer {
		bind = "0.0.0.0"
	}

	relayAddr := startCaptureRelayOn(t, bind, net.JoinHostPort(env.host, strconv.Itoa(env.port)), w)

	_, portText, err := net.SplitHostPort(relayAddr)
	require.NoError(t, err)

	port, err := strconv.Atoi(portText)
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(context.Background(), refusalDeadline)
	defer cancel()

	out, runErr := oci.runAt(t, runCtx, script, oci.proxyHost, port)
	require.NoErrorf(t, runErr, "%s never came back:\n%s", oci.label, out)

	t.Logf("%s output:\n%s", oci.label, out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	return outPath
}

// captureFixtureDir is where the raw recordings land. They are scratch — only
// the hex distilled out of them is kept — but CAPTURE_KEEP_DUMP_DIR leaves them
// somewhere durable, which is what you want the first time a client turns out
// to speak a dialect the distillation does not expect.
func captureFixtureDir(t *testing.T) string {
	t.Helper()

	if dir := os.Getenv("CAPTURE_KEEP_DUMP_DIR"); dir != "" {
		return dir
	}

	return t.TempDir()
}
