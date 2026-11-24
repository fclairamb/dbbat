//go:build integration

package oracle

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// pythonArrayDMLWorkload is the end-to-end twin of the python_thin_array_dml
// recording: an executemany, an execute() loop of the same UPDATE, and a SELECT
// of VARCHAR2 values past the 252-byte short form. Every value is synthetic.
const pythonArrayDMLWorkload = `
import sys
import oracledb

host, port, service, user, password = sys.argv[1:6]
conn = oracledb.connect(user=user, password=password,
                        dsn=oracledb.makedsn(host, int(port), service_name=service))
cur = conn.cursor()
cur.execute("BEGIN EXECUTE IMMEDIATE 'DROP TABLE dbbat_py_array_dml'; EXCEPTION WHEN OTHERS THEN NULL; END;")
cur.execute("CREATE TABLE dbbat_py_array_dml (id NUMBER PRIMARY KEY, name VARCHAR2(60), note VARCHAR2(400))")
cur.executemany("INSERT INTO dbbat_py_array_dml (id, name) VALUES (:1, :2)",
                [(i, "Bloc %d" % i) for i in range(1, 61)])
conn.commit()

upd = "UPDATE dbbat_py_array_dml SET name = :nouveau WHERE id = :n AND name = :ancien"
cur.executemany(upd, [{"nouveau": "Bloc %d bis" % i, "n": i if i != 13 else 999, "ancien": "Bloc %d" % i}
                      for i in range(1, 31)])
assert cur.rowcount == 29, cur.rowcount
for i in range(41, 46):
    cur.execute(upd, nouveau="Bloc %d ter" % i, n=i, ancien="Bloc %d" % i)
    assert cur.rowcount == 1, cur.rowcount
conn.commit()

cur.execute("UPDATE dbbat_py_array_dml SET note = RPAD('Bâtiment ' || id, 300, '.') WHERE id <= 6")
conn.commit()
cur.execute("SELECT id, note, name FROM dbbat_py_array_dml WHERE id <= 8 ORDER BY id")
assert len(cur.fetchall()) == 8
cur.execute("DROP TABLE dbbat_py_array_dml")
conn.close()
print("ok")
`

// TestIntegration_PythonThinArrayDMLIsLoggedPerExecution runs the workload
// through the proxy against a real Oracle and reads back what the store holds:
// the executemany with the server's row count and every bind row, each loop
// execution as its own statement with its own binds and row count, and the long
// VARCHAR2 values decoded rather than stored as hex.
func TestIntegration_PythonThinArrayDMLIsLoggedPerExecution(t *testing.T) {
	if err := exec.Command("python3", "-c", "import oracledb").Run(); err != nil {
		t.Skip("python-oracledb not installed (pip install oracledb)")
	}

	env := startOracleThroughProxyWith(t, oracleFixtureOptions{
		queryStorage: config.QueryStorageConfig{
			StoreResults:   true,
			MaxResultRows:  100,
			MaxResultBytes: 1 << 20,
		},
	})

	runPythonThinWorkload(t, env, pythonArrayDMLWorkload)

	const upd = "UPDATE dbbat_py_array_dml SET name = :nouveau WHERE id = :n AND name = :ancien"

	var updates []store.Query

	require.Eventually(t, func() bool {
		queries, err := env.store.ListQueries(context.Background(), store.QueryFilter{Limit: 500})
		require.NoError(t, err)

		updates = updates[:0]

		for _, q := range queries {
			if q.SQLText == upd && q.DurationMs != nil {
				updates = append(updates, q)
			}
		}

		return len(updates) == 6
	}, 15*time.Second, 100*time.Millisecond, "1 executemany + 5 loop executions must be recorded, got %d", len(updates))

	byFirstBind := map[string]store.Query{}

	for _, q := range updates {
		require.NotNil(t, q.Parameters, "every execution carries its binds")
		byFirstBind[q.Parameters.Values[0]] = q
	}

	batch := byFirstBind["Bloc 1 bis"]
	require.NotNil(t, batch.RowsAffected)
	assert.Equal(t, int64(29), *batch.RowsAffected)
	assert.Equal(t, 30, batch.Parameters.BatchSize)
	require.Len(t, batch.Parameters.Batch, 30)
	assert.Equal(t, []string{"Bloc 13 bis", "999", "Bloc 13"}, batch.Parameters.Batch[12])

	for i := 41; i <= 45; i++ {
		id := strconv.Itoa(i)
		q, ok := byFirstBind["Bloc "+id+" ter"]
		require.Truef(t, ok, "loop execution n=%s must be recorded", id)
		assert.Equal(t, []string{"Bloc " + id + " ter", id, "Bloc " + id}, q.Parameters.Values)
		require.NotNil(t, q.RowsAffected)
		assert.Equal(t, int64(1), *q.RowsAffected)
	}

	rows := awaitQueryRows(t, env, "SELECT id, note, name FROM dbbat_py_array_dml", 8)

	for i, row := range rows {
		id := i + 1
		assert.Equal(t, strconv.Itoa(id), row["ID"])
		assert.Contains(t, row["NAME"], "Bloc "+strconv.Itoa(id))

		if id <= 6 {
			prefix := "Bâtiment " + strconv.Itoa(id)
			assert.Equal(t, prefix+strings.Repeat(".", 300-len([]rune(prefix))), row["NOTE"], "row %d", id)
		}
	}
}

// awaitQueryRows waits for the statement whose text starts with sqlPrefix to
// complete and returns its captured rows, in order.
func awaitQueryRows(t *testing.T, env *oracleThroughProxy, sqlPrefix string, want int) []map[string]string {
	t.Helper()

	ctx := context.Background()

	var out []map[string]string

	require.Eventually(t, func() bool {
		queries, err := env.store.ListQueries(ctx, store.QueryFilter{Limit: 500})
		require.NoError(t, err)

		for _, q := range queries {
			if !strings.HasPrefix(q.SQLText, sqlPrefix) || q.DurationMs == nil {
				continue
			}

			result, err := env.store.GetQueryRows(ctx, q.UID, "", 100)
			require.NoError(t, err)

			out = out[:0]

			for _, row := range result.Rows {
				var decoded map[string]any
				require.NoError(t, json.Unmarshal(row.RowData, &decoded))

				flat := map[string]string{}
				for k, v := range decoded {
					if s, ok := v.(string); ok {
						flat[k] = s
					}
				}

				out = append(out, flat)
			}

			return len(out) == want
		}

		return false
	}, 15*time.Second, 100*time.Millisecond, "the statement %q must capture %d rows, got %d", sqlPrefix, want, len(out))

	return out
}
