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
// no scan can read it, run from a real sqlplus through dbbat, read back out of
// the `queries` table.
//
// It is the measurement in sql_extraction_survey_test.go without the one thing
// that survey cannot supply. That survey's long statements are produced by
// dbbat's own rewriter — sound for the header, which it reproduces byte for
// byte, but the CLR long form it writes is dbbat's encoding rather than a
// recorded client's, and no fixture carries a 64-bit statement past 251 bytes.
// Here the client writes every byte itself.
//
// The size was chosen by running this test against a build deliberately told
// the wrong dialect, which is the only honest way to pick it, and what came back
// was not the answer the survey predicted:
//
//   - A **322-byte** statement passes either way. sqlplus writes it as one run,
//     the text is contiguous and printable, and the last-resort keyword scan
//     walks it end to end. The survey's 253-byte boundary is real for the CLR
//     long form dbbat's own rewriter writes; this client does not write that
//     form at that size.
//   - A **40KB** one fails, and not where the survey looked. It fails because it
//     never gets reassembled: the message is larger than the negotiated SDU, so
//     `collectStatementMessage` asks `execFragmentShortfall` how much more is
//     owed, that walk could not read this dialect's header either, and the
//     continuation packets were therefore never collected. Measured: **7877
//     bytes of 40610** recorded, cut at the fragment boundary. That is the
//     2026-08-31 incident exactly (see reassembly.go), still live on this one
//     client family, and it is why `execSQLLength`'s reassembly caller takes the
//     dialect too rather than only the decode.
//
// Which is also why the gate is whole again as soon as the bytes are: with
// reassembly reading the header, the keyword scan finds the complete statement
// contiguous and reads it whole. The decode being precise is what makes that
// stop being luck.

const (
	// ociLongProbeHead opens the statement and ociLongProbeTail closes it, well
	// past any fragment boundary, so a statement cut at one is missing the
	// second marker.
	ociLongProbeHead = "dbbat_lp_head"
	ociLongProbeTail = "dbbat_lp_tail"

	// ociLongProbeLineWidth keeps each script line well inside sqlplus's own
	// per-line limit; the statement is built out of many of them.
	ociLongProbeLineWidth = 900

	// ociLongProbeMinBytes is the floor the probe has to clear to be testing
	// anything: a statement that fits one TNS Data packet is never reassembled,
	// so it cannot show the reassembly gap. The default SDU is 8192 and a
	// session may negotiate more, so the floor is set well above it rather than
	// at it.
	ociLongProbeMinBytes = 32767

	// ociLongProbeFiller is how much comment sits between the two markers.
	ociLongProbeFiller = 40000
)

// ociLongProbeStatement is a single statement of roughly 40KB, carried as a
// comment so Oracle parses it without hitting the 4000-byte limit on a SQL
// string literal. The newlines are the client's own: sqlplus sends its buffer
// verbatim, so they are part of the statement text on the wire and part of what
// the `queries` row must match.
func ociLongProbeStatement() string {
	var b strings.Builder

	b.WriteString("SELECT '" + ociLongProbeHead + "' AS h, /*")

	for written := 0; written < ociLongProbeFiller; written += ociLongProbeLineWidth {
		b.WriteString("\n" + strings.Repeat("x", ociLongProbeLineWidth))
	}

	b.WriteString("\n*/ '" + ociLongProbeTail + "' AS t FROM dual")

	return b.String()
}

// TestIntegration_OCILongStatementIsRecordedWhole asserts the `queries` row is
// the client's statement, not the prefix a scan cut out of it.
//
// On the **64-bit** OCI dialect (`ORACLE_TEST_OCI_CLIENT=container`, which is
// what CI runs) this is red before the header reading is told the dialect: the
// message is never reassembled, so `findSQLInPayload` hands the gate the ~8KB
// the first packet carried. The statement ran either way; this is about what
// the controls were evaluated against and what the audit trail kept — and
// everything past the fragment boundary (`oracleBlockedPatterns`, the approval
// patterns, the dynamic-SQL scan) was evadable by padding a statement past the
// SDU, which is the 2026-08-31 incident's own wording.
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
	require.Greater(t, len(probe), ociLongProbeMinBytes,
		"the probe must outgrow one TNS Data packet, or it never reaches the reassembly path "+
			"this is about")

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
		"the recorded statement stops at %d bytes of %d — the tail marker sits past every "+
			"fragment boundary, so this is a prefix the gate enforced against rather than the "+
			"statement: %q",
		len(recorded), len(probe), truncateSQL(recorded, 80))
	assert.Equalf(t, len(probe), len(recorded),
		"the recorded statement must be the client's own, byte for byte")
	assert.Equal(t, probe, recorded, "and the same bytes, not merely the same length")
}
