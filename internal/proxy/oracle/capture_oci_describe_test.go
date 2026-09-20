//go:build capture

// Capture tooling for the OCI describe fixture.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	go test -tags capture -run TestCapture_SQLPlusDescribe ./internal/proxy/oracle/
//
// A describe (TTC message 0x10) is where a query's column names and types come
// from. An OCI client gets the same records as a thin one, marshaled fixed-width
// — see describeColumnLayoutWide and TestOCIDescribeRecordsParse.
package oracle

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCapture_SQLPlusDescribe records sqlplus describing that query and keeps
// every describe response of the session, as hex lines.
func TestCapture_SQLPlusDescribe(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := filepath.Join(t.TempDir(), "sqlplus_describe.pcapng")

	requireOracleReachable(t, oracleAddr)

	sqlplus, err := exec.LookPath("sqlplus")
	if err != nil {
		t.Skipf("sqlplus unavailable: %v", err)
	}

	w := newCaptureWriter(t, outPath, "capture-sqlplus-describe")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	body := `CREATE OR REPLACE TYPE dbbat_cap_obj AS OBJECT (a NUMBER, b VARCHAR2(10));
/
SET PAGESIZE 0
SET FEEDBACK OFF
` + ociDescribeQuery + `
DROP TYPE dbbat_cap_obj;
EXIT
`

	script := writeTempScript(t, body)

	cmd := exec.CommandContext(t.Context(), sqlplus, "-S",
		fmt.Sprintf("system/oracle@//%s/%s", relayAddr, oracleService), "@"+script)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "sqlplus failed: %s", out)
	t.Logf("sqlplus: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	writeDescribeHexFixture(t, outPath, ociDescribeFixture)
}
