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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// ociDescribeFixture is where TestCapture_SQLPlusDescribe leaves the describe
// responses of the session below.
const ociDescribeFixture = "testdata/oci_describe.hex"

// ociDescribeQuery is deliberately wide in types rather than in rows: each
// column exercises a different corner of the describe record — a NUMBER with a
// real precision and scale, a VARCHAR2 whose maximum length does not fit a
// byte, a NUMBER whose scale is the -127 float sentinel, temporal types, a
// fixed CHAR, a RAW, and an object whose record carries a non-null 16-byte type
// OID (the one column that proves the toID field is a DLC with a four-byte
// length rather than a bare CLR).
const ociDescribeQuery = `SELECT CAST(1 AS NUMBER(10,2)) AS n2,
       CAST('x' AS VARCHAR2(4000)) AS big,
       1/3 AS flt,
       SYSDATE AS d,
       SYSTIMESTAMP AS ts,
       CAST('ab' AS CHAR(5)) AS c5,
       UTL_RAW.CAST_TO_RAW('zz') AS r,
       dbbat_cap_obj(1, 'x') AS obj
  FROM dual;`

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

// writeDescribeHexFixture keeps every server payload that leads with a describe
// message, as one hex line each.
func writeDescribeHexFixture(t *testing.T, dumpPath, outPath string) {
	t.Helper()

	r, err := dump.OpenReader(dumpPath)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	body := "# Every describe response (TTC message 0x10) of an sqlplus session through\n" +
		"# dbbat against Oracle 23ai Free: the TNS Data payload, two data-flag bytes\n" +
		"# first, exactly as extractTTCPayload receives it.\n" +
		"#\n" +
		"# The last one describes a deliberately wide set of column types — see\n" +
		"# ociDescribeQuery — and is what pins describeColumnLayoutWide and the\n" +
		"# fixed-width column record against real values rather than against runs of\n" +
		"# zeros.\n" +
		"#\n" +
		"# Regenerate with:\n" +
		"#   go test -tags capture -run TestCapture_SQLPlusDescribe ./internal/proxy/oracle/\n"

	frames := 0

	for {
		pkt, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		if pkt.Direction != dump.DirServerToClient {
			continue
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if ttc := extractTTCPayload(tns.Payload); len(ttc) == 0 || ttc[0] != byte(TTCFuncQueryResult) {
			continue
		}

		body += hex.EncodeToString(tns.Payload) + "\n"
		frames++
	}

	require.Positive(t, frames, "the sqlplus session must have described at least one query")
	require.NoError(t, os.WriteFile(outPath, []byte(body), 0o600))

	t.Logf("%d describe responses written to %s", frames, outPath)
}
