//go:build integration

package oracle

import (
	"context"
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

// The end-to-end half of the SDU-fragmentation fix: a statement larger than the
// negotiated SDU (8192, the default on both ends) arrives as several TNS Data
// packets, and before dbbat reassembled them the gate, the audit trail and the
// refusal path all worked off the first fragment alone.
//
// The fixture is the incident's own traffic in miniature: a generated MERGE with
// literal values and French labels — accents, so UTF-8 bytes past the fragment
// boundary — and no dynamic SQL anywhere.

// fragProbeTable is the table the fragmented MERGE below merges into.
const fragProbeTable = "dbbat_frag_probe"

// fragMergeRows is chosen so the statement clears 8192 bytes comfortably: each
// generated row is ~70 bytes.
const fragMergeRows = 200

// fragTailLabel is the last row's label. It sits well past the first fragment,
// so finding it in the database (and in `queries`) is what proves the whole
// statement was read, forwarded and recorded.
const fragTailLabel = "Périmètre chauffé — dernière pièce"

// fragmentedMergeSQL builds the >8KB MERGE. It is deliberately literal-valued
// rather than bound: bind values travel in their own part of the message, and
// what this suite is about is statement *text* past the SDU.
func fragmentedMergeSQL() string {
	var b strings.Builder

	fmt.Fprintf(&b, "MERGE INTO %s d\nUSING (\n", fragProbeTable)
	fmt.Fprintf(&b, "  SELECT 1 AS num_attr, 'Surface Pièce' AS lib FROM dual\n")

	for i := 2; i < fragMergeRows; i++ {
		fmt.Fprintf(&b, "  UNION ALL SELECT %d AS num_attr, 'Périmètre chauffé n°%d' AS lib FROM dual\n", i, i)
	}

	fmt.Fprintf(&b, "  UNION ALL SELECT %d AS num_attr, '%s' AS lib FROM dual\n",
		fragMergeRows, fragTailLabel)
	fmt.Fprintf(&b, ") s ON (d.num_attr = s.num_attr)\n")
	fmt.Fprintf(&b, "WHEN MATCHED THEN UPDATE SET d.lib = s.lib\n")
	fmt.Fprintf(&b, "WHEN NOT MATCHED THEN INSERT (num_attr, lib) VALUES (s.num_attr, s.lib)")

	return b.String()
}

// TestIntegration_FragmentedStatementIsExecutedRecordedAndRefusableWhole is the
// spec's integration bullet, in one fixture because every Oracle container start
// costs minutes.
//
// Three properties, in the order they failed in production:
//
//  1. the statement executes — before the fix it was refused, erratically, with
//     "dbbat cannot check dynamic SQL that is itself built from dynamic SQL" for
//     a statement carrying none;
//  2. it is recorded whole — /queries used to hold a 74-to-879-character prefix
//     of an 8-15KB statement, and the query-chain MAC sealed that prefix;
//  3. refusing it under a read_only grant leaves the connection usable — the
//     orphan continuation packet used to be forwarded upstream on its own after
//     the refusal, desynchronizing the session (the caller saw DPY-4011 at its
//     next call rather than the refusal).
func TestIntegration_FragmentedStatementIsExecutedRecordedAndRefusableWhole(t *testing.T) {
	ctx := context.Background()

	env := startOracleThroughProxy(t, nil)

	mergeSQL := fragmentedMergeSQL()
	require.Greater(t, len(mergeSQL), 8192,
		"the fixture has to exceed the default SDU or it proves nothing")

	_, _ = env.db.ExecContext(ctx, "DROP TABLE "+fragProbeTable)

	_, err := env.db.ExecContext(ctx,
		"CREATE TABLE "+fragProbeTable+" (num_attr NUMBER, lib VARCHAR2(200))")
	require.NoError(t, err)

	// 1. It runs.
	_, err = env.db.ExecContext(ctx, mergeSQL)
	require.NoError(t, err,
		"a statement larger than the SDU must be gated on its whole text, not refused on a fragment")

	// And it reached the upstream *whole*: the tail label only exists in the
	// database if the bytes past the first fragment were forwarded.
	var stored string

	require.NoError(t, env.db.QueryRowContext(ctx,
		"SELECT lib FROM "+fragProbeTable+" WHERE num_attr = "+strconv.Itoa(fragMergeRows)).Scan(&stored))
	assert.Equal(t, fragTailLabel, stored,
		"the accented literal past the fragment boundary must survive to the upstream intact")

	var rows int

	require.NoError(t, env.db.QueryRowContext(ctx,
		"SELECT count(*) FROM "+fragProbeTable).Scan(&rows))
	assert.Equal(t, fragMergeRows, rows)

	// 2. It is recorded whole.
	recorded := env.awaitQueryContaining(t, "MERGE INTO "+fragProbeTable)
	assert.Equal(t, len(mergeSQL), len(recorded.SQLText),
		"the audit trail must hold the statement, not a fragment of it")
	assert.Contains(t, recorded.SQLText, fragTailLabel)
	assert.NotContains(t, recorded.SQLText, partialStatementNote,
		"a reassembled statement is not a partial one")

	// 3. Refusing it leaves the session usable.
	t.Run("refused under read_only, connection survives", func(t *testing.T) {
		env.replaceGrant(t, []string{store.ControlReadOnly})

		client := env.newClient(t)

		env.mustRefuse(t, client, mergeSQL)
		env.mustStaySurvivable(t, client)

		var after int

		require.NoError(t, client.QueryRowContext(ctx,
			"SELECT count(*) FROM "+fragProbeTable).Scan(&after))
		assert.Equal(t, fragMergeRows, after,
			"the refused MERGE must never have reached upstream")
	})
}

// awaitQueryContaining returns the single `queries` row whose SQL text contains
// fragment, waiting for the asynchronous write.
func (e *oracleThroughProxy) awaitQueryContaining(t *testing.T, fragment string) store.Query {
	t.Helper()

	var matches []store.Query

	require.Eventually(t, func() bool {
		queries, err := e.store.ListQueries(context.Background(), store.QueryFilter{Limit: 500})
		if err != nil {
			return false
		}

		matches = nil

		for i := range queries {
			if strings.Contains(queries[i].SQLText, fragment) {
				matches = append(matches, queries[i])
			}
		}

		return len(matches) > 0
	}, 30*time.Second, 250*time.Millisecond, "no queries row ever carried %q", fragment)

	require.Len(t, matches, 1, "the statement must be recorded exactly once")

	return matches[0]
}

// pythonFragmentScript drives the same >8KB MERGE from python-oracledb thin —
// the client the incident was reported on, and the one whose SDU-sized writes
// produced the fragmentation in the first place. It then keeps using the
// connection, which is what the DPY-4011 half of the incident broke.
const pythonFragmentScript = `
import sys
import oracledb

host, port, service, user, password, table, tail = sys.argv[1:8]

rows = ["  SELECT 1 AS num_attr, 'Surface Pièce' AS lib FROM dual"]
for i in range(2, 200):
    rows.append("  UNION ALL SELECT %d AS num_attr, 'Périmètre chauffé n°%d' AS lib FROM dual" % (i, i))
rows.append("  UNION ALL SELECT 200 AS num_attr, '%s' AS lib FROM dual" % tail)

sql = ("MERGE INTO %s d\nUSING (\n" % table + "\n".join(rows) +
       "\n) s ON (d.num_attr = s.num_attr)\n"
       "WHEN MATCHED THEN UPDATE SET d.lib = s.lib\n"
       "WHEN NOT MATCHED THEN INSERT (num_attr, lib) VALUES (s.num_attr, s.lib)")

print("sql-bytes", len(sql.encode("utf-8")), flush=True)

conn = oracledb.connect(user=user, password=password,
                        dsn=oracledb.makedsn(host, int(port), service_name=service))
cur = conn.cursor()

cur.execute(sql)
conn.commit()
print("merged", flush=True)

cur.execute("SELECT lib FROM %s WHERE num_attr = 200" % table)
print("tail:", cur.fetchall()[0][0], flush=True)

# The half that used to die: after the big statement, an ordinary call on the
# same connection. A desynchronized upstream shows up here as DPY-4011.
cur.execute("SELECT 42 FROM dual")
print("survived", cur.fetchall()[0][0], flush=True)

conn.close()
print("done", flush=True)
`

// TestIntegration_FragmentedStatementFromPythonThin runs the fixture from the
// client that reported the incident. Skipped when python-oracledb is not
// installed, which is the case in CI; the go-ora half above carries the
// assertions there.
func TestIntegration_FragmentedStatementFromPythonThin(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	if err := exec.Command("python3", "-c", "import oracledb").Run(); err != nil {
		t.Skip("python-oracledb not installed (pip install oracledb)")
	}

	ctx := context.Background()

	env := startOracleThroughProxy(t, nil)

	_, _ = env.db.ExecContext(ctx, "DROP TABLE "+fragProbeTable)

	_, err := env.db.ExecContext(ctx,
		"CREATE TABLE "+fragProbeTable+" (num_attr NUMBER, lib VARCHAR2(200))")
	require.NoError(t, err)

	script := filepath.Join(t.TempDir(), "fragment.py")
	require.NoError(t, os.WriteFile(script, []byte(pythonFragmentScript), 0o600))

	runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "python3", script,
		env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey,
		fragProbeTable, fragTailLabel)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python-oracledb never came back from the fragmented statement:\n%s", out)

	output := string(out)
	assert.Contains(t, output, "merged", "the >8KB MERGE must execute:\n%s", output)
	assert.Contains(t, output, "tail: "+fragTailLabel,
		"the accented literal past the fragment boundary must round-trip:\n%s", output)
	assert.Contains(t, output, "survived 42",
		"the connection must still work after a fragmented statement:\n%s", output)
	assert.Contains(t, output, "done", "the client must close cleanly:\n%s", output)

	recorded := env.awaitQueryContaining(t, "MERGE INTO "+fragProbeTable)
	assert.Contains(t, recorded.SQLText, fragTailLabel,
		"the audit trail must hold the whole statement python-oracledb sent")
}
