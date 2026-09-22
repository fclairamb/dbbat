//go:build integration

package oracle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

// sqlplusLOBScript is the shape that used to cost a fetch every row it had: a
// CLOB and an XMLTYPE in the select list, with ordinary columns either side of
// them.
//
// It is deliberately the *smallest* version of the query — one LOB, one opaque
// type, two strings — rather than the thirteen-column one the hex fixture is
// cut from. The fixture is where the framing is measured; this is where the
// claim is checked end to end against a real 23ai server, through the real
// proxy, with the row read back out of query_rows.
const sqlplusLOBScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'before' AS lead, TO_CLOB('body') AS doc, XMLTYPE('<a/>') AS x, 'after' AS tail FROM dual;
EXIT
`

// ociLOBSQLFragment identifies that statement among everything sqlplus runs on
// its own.
const ociLOBSQLFragment = "TO_CLOB('body')"

// TestIntegration_OCIRowCaptureSurvivesALOBInTheSelectList is the live half of
// the LOB row walk, and it is the test that failed the way the spec described:
// before the fix it timed out waiting for a captured row, because there was
// never going to be one.
//
// Two things had to be true at once for that. Oracle turns row prefetch off
// when a LOB is in the select list, so the describe came back with no values
// and the whole fetch followed in a packet dbbat could not find the row data
// in; and a locator is not a length-prefixed scalar, so even once found, the
// walk drifted at the CLOB and dropped the row it was halfway through.
//
// `LEAD` and `TAIL` are the assertion. They sit either side of the two columns
// dbbat cannot render, and the bug was never that those two came back wrong —
// it was that these two came back not at all.
func TestIntegration_OCIRowCaptureSurvivesALOBInTheSelectList(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{
		reachableFromContainers: plannedOCIClient() == ociClientContainer,
		queryStorage: config.QueryStorageConfig{
			StoreResults:   true,
			MaxResultRows:  100,
			MaxResultBytes: 1 << 20,
		},
	})

	oci := requireOCIClient(t, env)

	runCtx, cancel := context.WithTimeout(context.Background(), refusalDeadline)
	defer cancel()

	output, runErr := oci.run(t, runCtx, sqlplusLOBScript)
	require.NoErrorf(t, runErr, "%s never came back:\n%s", oci.label, output)

	t.Logf("%s output:\n%s", oci.label, output)
	require.Contains(t, output, "after", "the statement must actually have returned its row")

	captured := awaitCapturedRow(t, env, ociLOBSQLFragment)

	assert.Equal(t, "before", captured["LEAD"],
		"the column in front of the CLOB must survive it")
	assert.Equal(t, "after", captured["TAIL"],
		"and so must the one behind the XMLTYPE — losing these was the whole defect")
	assert.Equal(t, "<CLOB locator>", captured["DOC"],
		"the LOB itself is named, never fetched: dbbat does not issue reads of its own")
	assert.Contains(t, captured, "X",
		"and the opaque column is present rather than dropped")
}
