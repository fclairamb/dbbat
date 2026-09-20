//go:build integration

package oracle

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// sqlplusRefCursorScript is the shape a thick-client user actually types: bind a
// `REFCURSOR` host variable, hand it to a procedure, then `PRINT` it. The two
// `PRINT`s are the drives — the frames that name a cursor id dbbat never saw
// parsed, because `OPEN p FOR …` ran inside the procedure body.
//
// WHENEVER SQLERROR is deliberately not set to EXIT: a refusal that killed the
// session would be indistinguishable from one that was merely reported, and the
// closing SELECT is what says the session survived either way.
const sqlplusRefCursorScript = `SET PAGESIZE 0
SET FEEDBACK OFF
VARIABLE rc REFCURSOR
BEGIN dbbat_oci_refcur(:rc); END;
/
PRINT rc
BEGIN dbbat_oci_refcur(:rc); END;
/
PRINT rc
SELECT 'survived=' || 42 FROM dual;
EXIT
`

// TestIntegration_RefCursorFromSQLPlusUnderReadOnly is the live half of the OCI
// REF-cursor evidence: sqlplus drives a `SYS_REFCURSOR` under a `read_only`
// grant, and both ids come out of the calls' bind output — through the real
// session gate, off a shape learned from the real upstream, with no fixture in
// sight.
//
// It now claims the other half too. The frame sqlplus drives a cursor with is an
// exec op in the OCI wide header declaring no statement, and
// execNoStatementCursorAt used to refuse to read that header — so the drive
// reached neither refuseUnknownCursor nor the grant, and was forwarded ungated.
// execWideNoStatementCursor closes that, which is why the drives must now be
// *gated*: each one resolves to the cursor learned above, runs through
// regateCursor, and comes back with its rows.
//
// The two halves are what make the assertions meaningful together. Gating
// without the ids learned would turn every sqlplus REF cursor into the
// ORA-01031 this whole feature exists to prevent, so "gated twice" and "refused
// never" have to hold at once.
//
// Both halves are the **4-byte OCI dialect** only, and the test says so out
// loud rather than asserting them at whichever client the runner happens to
// have. "The OCI encoding" is two encodings (see isCloseCursorsWide8Header):
// the Instant Client this work was captured from writes 4-byte integers after a
// 5-byte op header, and the sqlplus bundled in gvenzl/oracle-free 23.26 — which
// is the client CI uses, reached back over host.docker.internal — writes 8-byte
// ones after a 17-byte header. Measured on 2026-09-20, the 64-bit dialect
// learns no REF cursor id and so gates no drive; that gap is filed in
// specs/todos/ and pinned below as the *measured* state, so closing it fails
// this test rather than passing unnoticed.
func TestIntegration_RefCursorFromSQLPlusUnderReadOnly(t *testing.T) {
	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	ctx := context.Background()

	_, err := env.db.ExecContext(ctx, `CREATE OR REPLACE PROCEDURE dbbat_oci_refcur(p OUT SYS_REFCURSOR) AS
BEGIN
  OPEN p FOR SELECT 'refcur-' || LEVEL AS label FROM dual CONNECT BY LEVEL <= 3;
END;`)
	require.NoError(t, err, "the procedure must be creatable under the permissive grant the fixture starts with")

	defer func() { _, _ = env.db.ExecContext(ctx, "DROP PROCEDURE dbbat_oci_refcur") }()

	env.replaceGrant(t, []string{store.ControlReadOnly})

	runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
	defer cancel()

	output, runErr := oci.run(t, runCtx, sqlplusRefCursorScript)
	require.NoErrorf(t, runErr, "%s never came back from the REF cursor:\n%s", oci.label, output)

	t.Logf("%s output:\n%s", oci.label, output)

	assert.NotContains(t, output, "ORA-01031",
		"a REF cursor handed back by a procedure must not be refused under read_only:\n%s", output)
	assert.Contains(t, output, "refcur-3",
		"the drive must return the cursor's rows:\n%s", output)
	assert.Contains(t, output, "survived=42",
		"the session must still answer afterwards:\n%s", output)

	// Whichever dialect this client speaks, the one thing that must hold on both
	// is that no drive is refused: an id dbbat cannot resolve under read_only is
	// the ORA-01031 above.
	assert.Zero(t, env.logs.count(logMsgUntrackedCursorRefused),
		"no drive may name a cursor dbbat could not resolve")

	if ociSessionSpeaksWide64(t, env) {
		t.Log("64-bit OCI dialect: pinning the measured gap, not the claim")

		assert.Zero(t, env.logs.count(logMsgLearnedRefCursorID),
			"the bind-output walk does not fit this dialect yet; if it does now, the claims "+
				"below must replace this pin (see specs/todos/, oracle wide64 REF cursor)")
		assert.Zero(t, env.logs.count(logMsgReexecGated),
			"and with no id learned there is nothing for a drive to resolve to")

		return
	}

	assert.Equal(t, 2, env.logs.count(logMsgLearnedRefCursorID),
		"the two calls must each have had their REF cursor id read out of the bind output")
	assert.Equal(t, 2, env.logs.count(logMsgReexecGated),
		"each PRINT drives a cursor with a statement-less wide exec, and both must reach the "+
			"re-execution gate rather than be forwarded undecoded")
	assert.Zero(t, env.logs.count(logMsgUntrackedCursorForwarded),
		"a drive resolved to a cursor learned in this very session is never waved through")
}

// ociSessionSpeaksWide64 reports which of the two OCI dialects the sqlplus
// session negotiated, read off the proxy's own AUTH log rather than guessed from
// which side of the container boundary the client came from — the gvenzl images
// bundle different client versions, so the flavor is not the dialect.
//
// It fails rather than defaults when the log cannot say: a test that guesses
// here proves whichever half it guessed. Any wide-64 session in the run is the
// sqlplus one; the fixture's own go-ora connections are thin and never set it.
func ociSessionSpeaksWide64(t *testing.T, env *oracleThroughProxy) bool {
	t.Helper()

	// The AUTH challenge the proxy built, which is where it records both
	// encoding decisions it took for the client (session.go).
	const logMsgAuthChallenge = "sending AUTH challenge"

	seen := env.logs.boolsFor(logMsgAuthChallenge, "wide_encoding_64")
	require.NotEmpty(t, seen,
		"the proxy must have logged the AUTH challenge it built, or the dialect is unknowable")

	return slices.Contains(seen, true)
}

// sqlplusRepeatedStatementScript re-runs one ordinary statement, twice, with a
// bind variable so the client has every reason to keep the cursor. It is the
// second live case the OCI gating needs: a REF cursor is the exotic shape, and
// the ordinary one is what every sqlplus user actually does all day.
//
// The write and the read are separate scripts rather than one, so a refusal in
// the middle cannot be mistaken for the reason the other half behaved.
const (
	sqlplusRepeatedReadScript = `SET PAGESIZE 0
SET FEEDBACK OFF
VARIABLE n NUMBER
EXEC :n := 7
SELECT 'read=' || :n FROM dual;
SELECT 'read=' || :n FROM dual;
SELECT 'survived=' || 42 FROM dual;
EXIT
`

	sqlplusRepeatedWriteScript = `SET PAGESIZE 0
SET FEEDBACK OFF
VARIABLE n NUMBER
EXEC :n := 7
INSERT INTO dbbat_oci_reexec VALUES (:n);
INSERT INTO dbbat_oci_reexec VALUES (:n);
SELECT 'survived=' || 42 FROM dual;
EXIT
`
)

// TestIntegration_RepeatedStatementFromSQLPlusUnderReadOnly is the other half of
// the gating claim, and the one that would have caught the fix going too far.
//
// Reading the wide header's cursor id is what lets a re-execution be gated; a
// false positive on that reading gates a *parse* against whatever those four
// bytes hold, and the visible result is an ORA-01031 on ordinary read-only work
// that used to run. So the read case is not a nicety — it is the regression
// floor — and the write case is the enforcement it exists to make possible:
// read_only lands on the repeat exactly as it lands on the first execution.
func TestIntegration_RepeatedStatementFromSQLPlusUnderReadOnly(t *testing.T) {
	env := startOracleThroughProxyForOCI(t, nil)
	oci := requireOCIClient(t, env)

	ctx := context.Background()

	_, err := env.db.ExecContext(ctx, "CREATE TABLE dbbat_oci_reexec (n NUMBER)")
	require.NoError(t, err, "the table must be creatable under the permissive grant the fixture starts with")

	defer func() { _, _ = env.db.ExecContext(ctx, "DROP TABLE dbbat_oci_reexec") }()

	env.replaceGrant(t, []string{store.ControlReadOnly})

	t.Run("a repeated read is allowed both times", func(t *testing.T) {
		runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
		defer cancel()

		output, runErr := oci.run(t, runCtx, sqlplusRepeatedReadScript)
		require.NoErrorf(t, runErr, "%s never came back:\n%s", oci.label, output)

		t.Logf("%s output:\n%s", oci.label, output)

		assert.NotContains(t, output, "ORA-01031",
			"re-running a SELECT under read_only must not be refused:\n%s", output)
		assert.Equal(t, 2, strings.Count(output, "read=7"),
			"both executions must return their row:\n%s", output)
		assert.Contains(t, output, "survived=42",
			"the session must still answer afterwards:\n%s", output)
		assert.Zero(t, env.logs.count(logMsgUntrackedCursorRefused),
			"no execution may be refused as naming a cursor dbbat could not resolve")
	})

	t.Run("a repeated write is refused both times", func(t *testing.T) {
		runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
		defer cancel()

		output, runErr := oci.run(t, runCtx, sqlplusRepeatedWriteScript)
		require.NoErrorf(t, runErr, "%s never came back:\n%s", oci.label, output)

		t.Logf("%s output:\n%s", oci.label, output)

		assert.Equal(t, 2, strings.Count(output, "ORA-01031"),
			"read_only must refuse the repeat as well as the first execution:\n%s", output)
		assert.Contains(t, output, "survived=42",
			"a refusal ends the statement, never the session:\n%s", output)

		var rows int

		require.NoError(t, env.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbbat_oci_reexec").Scan(&rows))
		assert.Zero(t, rows, "neither INSERT may have reached the database")
	})
}
