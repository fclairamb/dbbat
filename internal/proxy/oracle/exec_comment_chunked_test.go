package oracle

import (
	"fmt"
	"strings"
	"testing"
)

// These tests pin the two decode failures reported on 2026-09-01 (both measured
// in production captures against a python-oracledb thin client):
//
//  1. A statement opening with a SQL comment failed startsWithSQLVerb, so the
//     header-anchored decode rejected the true run and the keyword scan started
//     the "statement" at a verb *inside* the comment. `-- MERGE s'execute` thus
//     produced a statement opening at MERGE whose apostrophe opened a string
//     that never closed, and the client was refused with ErrStatementNotFullyRead.
//  2. A statement of 32768+ bytes is written in the CLR long form with 32767-byte
//     chunks, so a chunk-length prefix sits mid-text and the contiguous run of
//     exactly sqlLen bytes does not exist. The decode fell through to the keyword
//     scan, which handed the gate a prefix cut at the chunk boundary.

// The two-line reproduction from the report, byte for byte.
const commentLedRepro = "-- MERGE s'execute\nSELECT 1 FROM dual"

func TestSkipLeadingSQLComments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"SELECT 1", "SELECT 1"},
		{"  \n\tSELECT 1", "SELECT 1"},
		{"-- header\nSELECT 1", "SELECT 1"},
		{"-- one\n-- two\nSELECT 1", "SELECT 1"},
		{"/* block */ SELECT 1", "SELECT 1"},
		{"/* multi\nline */\n-- and a line\nSELECT 1", "SELECT 1"},
		{"/*+ FULL(t) */ SELECT 1", "SELECT 1"},
		{"-- only a comment", ""},
		{"/* left open SELECT 1", ""},
		{commentLedRepro, "SELECT 1 FROM dual"},
	}

	for _, tc := range cases {
		if got := skipLeadingSQLComments(tc.in); got != tc.want {
			t.Errorf("skipLeadingSQLComments(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStartsWithSQLVerb_LeadingComments(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		commentLedRepro,
		"-- TRANSFO: ATTR_MIGRATE\n-- CLE DU MERGE = (num_attr)\nMERGE INTO t USING (SELECT 1 FROM dual) s ON (1=1) WHEN MATCHED THEN UPDATE SET x = 1",
		"/* generated */ INSERT INTO t VALUES (1)",
	} {
		if !startsWithSQLVerb(s) {
			t.Errorf("startsWithSQLVerb(%.40q…) = false, want true", s)
		}
	}

	// A run that is a comment through and through names no verb — the keyword
	// scan's word-boundary discipline must not regress here either.
	for _, s := range []string{"-- MERGE only ever mentioned", "/* SELECT */"} {
		if startsWithSQLVerb(s) {
			t.Errorf("startsWithSQLVerb(%q) = true, want false", s)
		}
	}
}

// TestDecodeExecStatement_CommentLedStatement is bug 1: the header-anchored
// decode must accept the run its own header names even when the statement opens
// with a comment, instead of falling through to the keyword scan.
func TestDecodeExecStatement_CommentLedStatement(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		commentLedRepro,
		"-- un en-tête généré\n-- MERGE s'execute (pas la date de generation).\nMERGE INTO t USING (SELECT 'x' AS v FROM dual) s ON (1=1) WHEN MATCHED THEN UPDATE SET x = s.v",
	} {
		payload := buildLongPiggybackExec(sql)

		got, ok := decodeExecStatement(payload)
		if !ok {
			t.Fatalf("decodeExecStatement failed on a comment-led statement %.40q…", sql)
		}

		if got != sql {
			t.Fatalf("decodeExecStatement = %.60q…, want the full statement %.60q…", got, sql)
		}
	}
}

// TestDecodePiggybackExec_CommentLedStatement_NotTruncated pins the whole
// consequence: the decode returns the complete statement, not a keyword-scan
// fragment marked (or worse, not marked) as truncated.
func TestDecodePiggybackExec_CommentLedStatement_NotTruncated(t *testing.T) {
	t.Parallel()

	result, err := decodePiggybackExecSQL(padExecPayload(buildLongPiggybackExec(commentLedRepro)))
	if err != nil {
		t.Fatalf("decodePiggybackExecSQL: %v", err)
	}

	if result.SQL != commentLedRepro {
		t.Fatalf("SQL = %q, want %q", result.SQL, commentLedRepro)
	}

	if result.Truncated {
		t.Fatal("Truncated = true for a statement that arrived whole")
	}
}

// padExecPayload grows a synthetic exec frame past decodePiggybackExecSQL's
// 52-byte minimum without touching the statement bytes.
func padExecPayload(payload []byte) []byte {
	if len(payload) >= 52 {
		return payload
	}

	return append(payload, make([]byte, 52-len(payload))...)
}

// buildChunkedPiggybackExec is buildLongPiggybackExec with the statement
// written in the CLR long form: 0xFE marker, then length-prefixed chunks of
// chunkSize bytes, closed by a zero terminator. bigChunks selects the
// compressed-int chunk-length encoding (UseBigClrChunks — what every modern
// thin client negotiates) over single length bytes.
func buildChunkedPiggybackExec(sql string, chunkSize int, bigChunks bool) []byte {
	out := make([]byte, 0, 64+len(sql))
	out = append(out, byte(TTCFuncPiggyback), PiggybackSubExecSQL, 0x00)
	out = append(out, 0x02, 0x81, 0x21) // options
	out = append(out, 0x00)             // cursor id 0
	out = append(out, 0x01)             // the cursor-id-is-zero flag
	out = append(out, encodeCompressedInt(len(sql))...)
	out = append(out, 0x01, 0x01, 0x0d)
	out = append(out, make([]byte, 24)...)

	out = append(out, 0xFE)

	for start := 0; start < len(sql); start += chunkSize {
		end := min(start+chunkSize, len(sql))

		if bigChunks {
			out = append(out, encodeCompressedInt(end-start)...)
		} else {
			out = append(out, byte(end-start))
		}

		out = append(out, sql[start:end]...)
	}

	out = append(out, 0x00)       // terminator
	out = append(out, 0x00, 0x00) // trailing framing

	return out
}

// chunkedMergeStatement builds a MERGE of exactly total bytes in the shape the
// production refusal had: a comment-led header with accents up front, then
// ASCII rows with quoted literals, one of which straddles the chunk boundary (a
// cut inside a literal is what read as an unterminated quoted run). The tail is
// ASCII so the exact-size truncation never splits a rune.
func chunkedMergeStatement(t *testing.T, total int) string {
	t.Helper()

	var b strings.Builder

	b.WriteString("-- TRANSFO: ATTR_MIGRATE · généré\nMERGE INTO attribut_propriete d\nUSING (\n  SELECT 0 AS num_attr, 'fin' AS lib FROM dual\n")

	for i := 1; b.Len() < total; i++ {
		fmt.Fprintf(&b, "  UNION ALL SELECT %d AS num_attr, 'Quantite (u) no%d' AS lib FROM dual\n", i, i)
	}

	sql := b.String()[:total]
	if len(sql) != total {
		t.Fatalf("built %d bytes, want %d", len(sql), total)
	}

	return sql
}

// TestDecodeExecStatement_ChunkedCLR is bug 2: a statement past the 32767-byte
// chunk size must decode whole from its chunked wire form, in both chunk-length
// encodings.
func TestDecodeExecStatement_ChunkedCLR(t *testing.T) {
	t.Parallel()

	const chunkSize = 32767

	cases := []struct {
		name      string
		size      int
		chunkSize int
		bigChunks bool
	}{
		// The production boundary: first refused size is one byte past a chunk.
		{"32768 big chunks", chunkSize + 1, chunkSize, true},
		// The measured production frame: 33241 bytes, two chunks.
		{"33241 big chunks", 33241, chunkSize, true},
		// Well past the boundary: 117449 bytes was the largest reported file.
		{"117449 big chunks", 117449, chunkSize, true},
		// The pre-UseBigClrChunks encoding: single length bytes, 64-byte chunks.
		{"small chunks", 300, 64, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql := chunkedMergeStatement(t, tc.size)
			payload := buildChunkedPiggybackExec(sql, tc.chunkSize, tc.bigChunks)

			got, ok := decodeExecStatement(payload)
			if !ok {
				t.Fatal("decodeExecStatement failed on a chunked statement")
			}

			if got != sql {
				t.Fatalf("decodeExecStatement returned %d bytes, want the full %d-byte statement", len(got), len(sql))
			}
		})
	}
}

// TestDecodeExecStatement_ChunkedCLR_WrongTotalRefused pins the walk's
// discrimination: chunks that do not sum to the declared length answer nothing
// (the keyword scan then owns the frame, marked as the guess it is).
func TestDecodeExecStatement_ChunkedCLR_WrongTotalRefused(t *testing.T) {
	t.Parallel()

	sql := chunkedMergeStatement(t, 33000)
	payload := buildChunkedPiggybackExec(sql, 32767, true)

	// Truncate the payload mid-second-chunk: the declared length can no longer
	// be covered.
	payload = payload[:len(payload)-300]

	if _, ok := decodeExecStatement(payload); ok {
		t.Fatal("decodeExecStatement accepted a chunked statement whose chunks do not cover the declared length")
	}
}

// TestDecodeExecStatement_ChunkedSetsBindFloor pins that a chunked statement's
// bind scan cannot walk back into the statement text: the locate reports where
// the statement ends on the wire.
func TestDecodeExecStatement_ChunkedSetsBindFloor(t *testing.T) {
	t.Parallel()

	sql := chunkedMergeStatement(t, 33000)
	payload := buildChunkedPiggybackExec(sql, 32767, true)

	stmt, ok := decodeExecStatementText(payload)
	if !ok {
		t.Fatal("decodeExecStatementText failed")
	}

	if stmt.End == 0 {
		t.Fatal("End = 0: the chunked locate must report where the statement ends")
	}

	// End points just past the CLR terminator, which sits before the two
	// trailing framing bytes the builder appends.
	if want := len(payload) - 2; stmt.End != want {
		t.Fatalf("End = %d, want %d", stmt.End, want)
	}
}
