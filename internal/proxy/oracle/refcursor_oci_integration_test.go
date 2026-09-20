//go:build integration

package oracle

import (
	"context"
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
// **What it deliberately does not claim** is that the rows only arrive because
// of that. They arrived before this existed too, and for a reason that is a
// finding rather than a reassurance: the frame sqlplus drives a cursor with is
// an exec op in the OCI wide header declaring no statement, which
// execNoStatementCursorAt refuses to read — so the drive reaches
// neither refuseUnknownCursor nor the grant, and is forwarded ungated. That gap
// is filed in specs/todos, and it is filed *after* this change on purpose:
// closing it without the ids learned here would turn every sqlplus REF cursor
// into the ORA-01031 this whole feature exists to prevent.
//
// So the assertions below are in two halves — the ids learned (the claim), and
// the session's continued good behaviour (the regression floor).
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

	assert.Equal(t, 2, env.logs.count(logMsgLearnedRefCursorID),
		"the two calls must each have had their REF cursor id read out of the bind output")
	assert.Zero(t, env.logs.count(logMsgUntrackedCursorRefused),
		"no drive may name a cursor dbbat could not resolve")
}
