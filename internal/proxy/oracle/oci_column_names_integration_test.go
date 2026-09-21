//go:build integration

package oracle

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// sqlplusColumnNamesScript is the shape the heuristic scanner cannot get right,
// typed at a real client: a one-character alias (which the scanner's identifier
// bound skips) and an unnamed expression (which has no identifier in the payload
// at all, only its own text). The aliased columns either side of them are what
// says the positions did not simply shift.
const sqlplusColumnNamesScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SELECT 'before' AS lead, 1 AS d, UPPER('x') || '-ok', 'after' AS tail FROM dual;
EXIT
`

// ociColumnNamesSQLFragment identifies that statement among everything sqlplus
// runs on its own (a login probe, a couple of settings queries).
const ociColumnNamesSQLFragment = "UPPER('x')"

// TestIntegration_OCIRowCaptureCarriesRealColumnNames is the live half of the
// describe-record reading: a real sqlplus session, through the real proxy,
// against a real 23ai server, and the column names that land in `query_rows`
// come from the server's own describe records.
//
// The two columns that matter are the ones a heuristic scan gets wrong. `D` is
// one character long, which the scanner's identifier bound skips outright, and
// `UPPER('x') || '-ok'` is an unnamed expression whose name in the record is its
// own text — there is no identifier anywhere in the payload to find. Under the
// scanner those two positions were filled by padding and by whatever other
// length-prefixed ASCII the record happened to carry (the schema and the type
// name are the usual culprits), so a captured row was filed under keys that were
// not its columns.
//
// Result capture is on in this fixture only — `query_rows` is where a column
// name is actually observable, and nothing else in the pipeline records one.
func TestIntegration_OCIRowCaptureCarriesRealColumnNames(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{
		reachableFromContainers: plannedOCIClient() == ociClientContainer,
		queryStorage: config.QueryStorageConfig{
			StoreResults:   true,
			MaxResultRows:  100,
			MaxResultBytes: 1 << 20,
		},
	})

	oci := requireOCIClient(t, env)

	ctx := context.Background()

	runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
	defer cancel()

	output, runErr := oci.run(t, runCtx, sqlplusColumnNamesScript)
	require.NoErrorf(t, runErr, "%s never came back:\n%s", oci.label, output)

	t.Logf("%s output:\n%s", oci.label, output)
	require.Contains(t, output, "X-ok", "the statement must actually have returned its row")

	captured := awaitCapturedRow(t, env, ociColumnNamesSQLFragment)

	names := make([]string, 0, len(captured))
	for name := range captured {
		names = append(names, name)
	}

	sort.Strings(names)
	t.Logf("captured column names: %q", names)

	assert.Equal(t, map[string]interface{}{
		"LEAD":              "before",
		"D":                 "1",
		"UPPER('X')||'-OK'": "X-ok",
		"TAIL":              "after",
	}, captured,
		"every key must be the describe record's own name — the one-character alias and the "+
			"unnamed expression included, which is exactly what the heuristic scanner could not do")

	// The other half of the same change, live. Reading the records puts the
	// session in a row stream, so the ORA-01403 that ends the fetch now arrives
	// *inside* one; a session that refused it would leave the statement pending
	// for the next one's flushPendingQuery, and this is where that shows.
	assert.Zerof(t, env.logs.count(logMsgMidStreamStatusRefused),
		"no fetch terminator may be refused for arriving inside the row stream it ends; refused "+
			"ORA codes were %v",
		env.logs.intsFor(logMsgMidStreamStatusRefused, "ora_code"))

	completed := awaitCompletedQuery(t, env, ociColumnNamesSQLFragment)
	assert.NotNil(t, completed.DurationMs,
		"the statement must have been completed by its own OER rather than left pending")
	assert.Nil(t, completed.Error, "and completed as a success, not as an error")
}

// awaitCompletedQuery polls for the statement whose SQL contains fragment, once
// it carries a completion.
func awaitCompletedQuery(t *testing.T, env *oracleThroughProxy, fragment string) store.Query {
	t.Helper()

	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)

	for {
		queries, err := env.store.ListQueries(ctx, store.QueryFilter{UserID: &env.user.UID, Limit: 200})
		require.NoError(t, err)

		for _, q := range queries {
			if contains(q.SQLText, fragment) && q.DurationMs != nil {
				return q
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("no completed query for a statement containing %q", fragment)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// awaitCapturedRow finds the statement whose SQL contains fragment and returns
// its first captured row, decoded. Both the query row and its result rows are
// written asynchronously (the row writer batches), so it polls.
func awaitCapturedRow(t *testing.T, env *oracleThroughProxy, fragment string) map[string]interface{} {
	t.Helper()

	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)

	for {
		queries, err := env.store.ListQueries(ctx, store.QueryFilter{UserID: &env.user.UID, Limit: 200})
		require.NoError(t, err)

		for _, q := range queries {
			if !contains(q.SQLText, fragment) {
				continue
			}

			rows, err := env.store.GetQueryRows(ctx, q.UID, "", 10)
			require.NoError(t, err)

			if len(rows.Rows) == 0 {
				continue
			}

			var decoded map[string]interface{}
			require.NoError(t, json.Unmarshal(rows.Rows[0].RowData, &decoded))

			return decoded
		}

		if time.Now().After(deadline) {
			t.Fatalf("no captured row for a statement containing %q", fragment)
		}

		time.Sleep(200 * time.Millisecond)
	}
}
