//go:build integration

package oracle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The live half of the 64-bit exec-header decode: a statement long enough that
// the window scan cannot read it, run from a real sqlplus through dbbat, read
// back out of the `queries` table.
//
// It is the measurement in sql_extraction_survey_test.go without the one thing
// that survey cannot supply. That survey's long statements are produced by
// dbbat's own rewriter — sound for the header, which it reproduces byte for
// byte, but the long CLR form it writes is dbbat's encoding rather than a
// recorded client's, and no fixture carries a 64-bit statement past 251 bytes.
// Here the client writes every byte itself.

// The markers bracket the statement, and where the second one sits is the whole
// point: the window scan keeps exactly 252 bytes, so a tail marker past that
// offset is present in the recorded text only if the header-anchored decode
// read the statement.
const (
	ociLongProbeHead = "dbbat_lp_head"
	ociLongProbeTail = "dbbat_lp_tail"
)

// ociLongProbeStatement is a single-line statement of 322 bytes whose tail
// marker starts at byte 292.
func ociLongProbeStatement() string {
	return "SELECT '" + ociLongProbeHead + "' AS h, '" + strings.Repeat("x", 250) +
		"' AS body, '" + ociLongProbeTail + "' AS t FROM dual"
}

// TestIntegration_OCILongStatementIsRecordedWhole runs it and asserts the
// `queries` row is the client's statement, not its first 252 bytes.
//
// On the **64-bit** OCI dialect (`ORACLE_TEST_OCI_CLIENT=container`, which is
// what CI runs) this is the test that was red before the exec decode was told
// the dialect: the 4-byte and thin walks both refuse that header, so the decode
// declined and the frame fell through to the 40-70 offset window, which reads
// the CLR long form's first bytes as a length and keeps 252 bytes without
// reporting a truncation. The statement ran either way — this is about what the
// gate enforced against and what the audit trail kept.
//
// On the 4-byte dialect (an Instant Client on PATH) it passes before and after:
// `execSQLLengthWideField` already read that header. Keeping one test for both
// is deliberate — the property is "a statement is recorded whole", not "this
// dialect is special".
func TestIntegration_OCILongStatementIsRecordedWhole(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{
		reachableFromContainers: plannedOCIClient() == ociClientContainer,
	})

	client := requireOCIClient(t, env)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	probe := ociLongProbeStatement()

	out, err := client.run(t, ctx, "SET PAGESIZE 0\nSET FEEDBACK OFF\nSET LINESIZE 1000\n"+probe+";\nEXIT\n")
	require.NoErrorf(t, err, "sqlplus running a %d-byte statement:\n%s", len(probe), out)

	assert.Contains(t, out, ociLongProbeHead, "the statement must have run:\n%s", out)

	var matched []string

	for _, sqlText := range recordedStatements(t, env) {
		if strings.Contains(sqlText, ociLongProbeHead) {
			matched = append(matched, sqlText)
		}
	}

	require.Lenf(t, matched, 1,
		"exactly one recorded statement carries the head marker; got %d", len(matched))

	recorded := matched[0]

	assert.Containsf(t, recorded, ociLongProbeTail,
		"the recorded statement stops at %d bytes of %d — the tail marker starts past the 252 "+
			"bytes the window scan keeps, so this is the window scan's prefix rather than the "+
			"header-anchored decode's statement: %q",
		len(recorded), len(probe), truncateSQL(recorded, 80))
	assert.Equalf(t, probe, recorded,
		"the recorded statement must be the client's own, byte for byte (%d recorded, %d sent)",
		len(recorded), len(probe))
}
