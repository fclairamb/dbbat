//go:build capture

// Capture tooling for the **thin** dialect's half of the LOB evidence.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 300s -run TestCapture_GoOraLOB ./internal/proxy/oracle/
//
// The two OCI dialects are recorded by TestCapture_OCILOBFetchThroughDBBat
// (`-tags integration`); this is the compressed encoding a thin client speaks,
// and it goes through a bare relay because nothing in it depends on what dbbat
// rewrites — see ociFixtureProvenance for why the 64-bit one cannot.
package oracle

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCapture_GoOraLOBInline records ociLOBQuery driven by go-ora with its
// **default** LOB policy, and the default is the finding: a thin client asks
// for the LOB bodies up front, so the server inlines them and the row carries
// no locator to step over at all.
func TestCapture_GoOraLOBInline(t *testing.T) {
	captureGoOraLOB(t, "capture-go-ora-lob", "testdata/"+goOraLOBFixture, "")
}

// TestCapture_GoOraLOBStream records the same query with `lob fetch=post`,
// which is the thin client asking for locators instead — the shape the row walk
// has framing to skip in, and therefore the one that says whether a thin
// session spells that framing the way the 64-bit OCI one does.
func TestCapture_GoOraLOBStream(t *testing.T) {
	captureGoOraLOB(t, "capture-go-ora-lob-stream", "testdata/"+goOraLOBStreamFixture,
		"?lob+fetch=post&lob+read=no")
}

// captureGoOraLOB runs ociLOBQuery through a recording relay and writes the
// session to outPath. Values are logged rather than asserted: what the columns
// scan into is the driver's business, and the fixture is about the bytes.
func captureGoOraLOB(t *testing.T, sessionID, outPath, dsnOptions string) {
	t.Helper()

	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, sessionID)
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s%s", relayAddr, oracleService, dsnOptions)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	rows, err := db.QueryContext(t.Context(), goOraLOBQuery)
	require.NoError(t, err)

	cols, err := rows.Columns()
	require.NoError(t, err)
	require.Len(t, cols, len(goOraLOBColumns), "the fixture is worthless at any other column count")

	t.Logf("columns: %v", cols)

	got := 0

	for rows.Next() {
		values := make([]interface{}, len(cols))
		into := make([]interface{}, len(cols))

		for i := range values {
			into[i] = &values[i]
		}

		require.NoError(t, rows.Scan(into...))

		for i, v := range values {
			t.Logf("  %s = %T %v", cols[i], v, v)
		}

		got++
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, 1, got, "ociLOBQuery selects from dual and returns exactly one row")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}
