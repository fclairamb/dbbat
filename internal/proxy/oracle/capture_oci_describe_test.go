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
//
// Like its siblings in capture_refcursor_test.go, this harness records the
// **4-byte** dialect only, and the recording is held to that before a byte is
// written — see requireRecordedDialect.
package oracle

import (
	"testing"
)

// TestCapture_SQLPlusDescribe records sqlplus describing that query and keeps
// every describe response of the session, as hex lines.
func TestCapture_SQLPlusDescribe(t *testing.T) {
	client := sqlplusCaptureClient(t)

	body := ociDescribeObjectTypeScript() + `SET PAGESIZE 0
SET FEEDBACK OFF
` + ociDescribeQuery + `
` + ociDescribeTypedQuery + `
` + ociDescribeObjectDropScript() + `EXIT
`

	outPath := runSQLPlusCapture(t, client, "capture-sqlplus-describe", body)

	// ociDescribeFixture is the 4-byte set's describe evidence, and
	// writeDescribeHexFixture stamps whatever it is handed with a header naming
	// the dialect. A 64-bit sqlplus on PATH would otherwise overwrite audited
	// 4-byte bytes with the other dialect's, under the 4-byte name — the
	// overwrite the guard exists to refuse, reached through this entry point
	// instead of the REF-cursor one. The 64-bit describe fixture is recorded by
	// TestCapture_OCIFixturesThroughDBBat (`-tags integration`).
	requireRecordedDialect(t, client, outPath)

	writeDescribeHexFixture(t, outPath, ociDescribeFixture)
}
