package oracle

import (
	"strings"
	"unicode/utf8"
)

// This file replaces guessing with reading. Everything else that pulls SQL out
// of an execute message — the 40-70 and 50-75 length-prefix windows, the
// keyword scan — searches for something that *looks like* a statement. The
// execute header says how long the statement is, so the search space collapses
// to "the printable run of exactly that length".
//
// The difference is not academic. Measured across every recording in testdata/
// (internal/proxy/oracle/sql_extraction_survey_test.go), the window scan
// returned a mid-statement fragment for **48 of the 137** execute ops: an ASCII
// byte *inside* the statement is a perfectly good length prefix (a space is 32,
// `T` is 84), the old looksLikeSQL matched a keyword with no word boundary, and
// nothing required the run it named to be text at all. Three of those readings
// were enforcement failures rather than cosmetic ones:
//
//   - `ALTER SESSION SET CURRENT_SCHEMA=…` read as `SET CURRENT_SCHEMA=…`
//     (9 ops, five distinct statements, all DBeaver connection setup —
//     TestSurveyAlterSessionMisreadAsSet computes it). `ALTER` is in
//     writeKeywords and ddlKeywords; `SET` is in neither, so read_only and
//     block_ddl did not fire.
//   - go-ora's `UPDATE … SET name = …` read as `SET name = …`. Same bypass.
//   - `… WHERE GRANTED_ROLE='DBA'` read as a `GRANT` statement.
//
// The decode is the same shape as every other walk in this package: bounded,
// validated, and returning "no" rather than a guess. A frame it cannot read
// falls back to the old scan, so nothing that used to be gated stops being
// gated.

// execHeaderMinLen is the smallest payload that could carry an exec header plus
// a one-byte statement.
const execHeaderMinLen = 12

// execMaxSQLLen bounds a plausible statement length. Oracle's own limit is
// 64K for the SQL text of a single statement; anything past 1MB means the walk
// landed on the wrong bytes.
const execMaxSQLLen = 1 << 20

// isPiggybackExecHeader reports whether ttcPayload opens with the v315+
// piggyback execute-with-SQL op header (`03 5e`).
func isPiggybackExecHeader(ttcPayload []byte) bool {
	return len(ttcPayload) > 1 &&
		TTCFunctionCode(ttcPayload[0]) == TTCFuncPiggyback &&
		ttcPayload[1] == PiggybackSubExecSQL
}

// decodeExecStatement reads the statement out of an execute message by the
// length its own header declares, and reports false when it cannot.
//
// It accepts the frame either as the exec op itself or as a close-cursors
// piggyback with the exec stapled behind it, because that is the shape dbbat
// has always called "the JDBC exec": every `11 69` in the corpus that carries
// SQL is a close list followed by `03 5e <exec>`, and the `11 98` sub-op in
// dbbat's anchor list appears in no recording at all.
func decodeExecStatement(ttcPayload []byte) (string, bool) {
	if sql, ok := decodeExecStatementAt(ttcPayload); ok {
		return sql, true
	}

	if end, ok := closeCursorsEnd(ttcPayload); ok {
		if sql, ok := decodeExecStatementAt(ttcPayload[end:]); ok {
			return sql, true
		}
	}

	return "", false
}

// decodeExecStatementAt is decodeExecStatement for a payload that must already
// begin at the exec op header.
func decodeExecStatementAt(body []byte) (string, bool) {
	if !isPiggybackExecHeader(body) || len(body) < execHeaderMinLen {
		return "", false
	}

	sqlLen, ok := execSQLLength(body)
	if !ok {
		return "", false
	}

	return locateExecSQLText(body, sqlLen)
}

// execSQLLength walks an exec op header to the field that declares the
// statement's byte length.
//
// Thin encoding (go-ora, python-oracledb thin, JDBC thin, DBeaver):
//
//	[0]    0x03           piggyback
//	[1]    0x5e           execute with SQL
//	[2]    seq            TTC sequence number
//	[3]    0x00           present only from TTC version 18 (v315+) on
//	[..]   options        TTC compressed int
//	[..]   cursorID       TTC compressed int
//	[..]   flag           one byte: 1 when the cursor id is 0
//	[..]   sqlLen         TTC compressed int   <- this
//
// The v315 pad is told from the options field the same way decodeCursorReexec
// and decodeCloseCursors tell it: a compressed int opens with its own byte
// count, which is never zero, so a zero at [3] can only be the pad.
//
// OCI wide encoding (sqlplus, SQL*Developer via OCI, Instant Client) — the
// header shape isCloseCursorsWideHeader already knows, with the length as a
// little-endian ub4 after the pointer sentinel:
//
//	[0..2] 03 5e seq
//	[3]    0x01           constant
//	[4]    seq+1          the NEXT TTC message's sequence number
//	[5..12] options       8 bytes
//	[13..20] fe x8        pointer sentinel
//	[21..24] sqlLen*3     uint32 little-endian   <- this
func execSQLLength(body []byte) (int, bool) {
	if n, ok := execSQLLengthWide(body); ok {
		return n, true
	}

	pos := 3
	if body[3] == 0 {
		pos = 4
	}

	// options
	_, n := readCompressedInt(body[pos:])
	if n == 0 {
		return 0, false
	}

	pos += n

	// cursor id
	_, n = readCompressedInt(body[pos:])
	if n == 0 {
		return 0, false
	}

	pos += n

	// The "cursor id is zero" flag. readCompressedInt can consume the buffer
	// exactly, so stepping over this byte unchecked would slice past the end —
	// `03 5e 01 04 aa bb cc dd 03 ee ff 00` is twelve bytes that clear every
	// other guard and does it. The recover in interceptClientMessage would
	// contain the panic, but containment means the frame is forwarded ungated,
	// which is the bypass class this decode exists to close.
	if pos >= len(body) {
		return 0, false
	}

	pos++

	sqlLen, n := readCompressedInt(body[pos:])
	if n == 0 || sqlLen <= 0 || sqlLen > execMaxSQLLen {
		return 0, false
	}

	return sqlLen, true
}

// execSQLLengthWide reads the statement length out of the OCI wide exec header.
// The `[0x01][seq+1]` pad plus the 8-byte pointer sentinel is the same shape
// isCloseCursorsWideHeader validates, so a payload that does not fit it is read
// as the thin encoding instead of guessed at.
//
// The field is the statement's length **times three** — the client sizes the
// buffer for its widest character encoding rather than reporting the byte
// count, the same 3x UTF-8 max-expansion convention the wide AUTH preamble uses
// for user_id_len (findUserIDLenPos, phase1_forward.go). Measured on
// testdata/sqlplus_cursor_reexec.pcapng: 0x117 (279) for a
// 93-byte statement and 0x45 (69) for a 23-byte one, in five frames. A value
// that is not a multiple of three is refused rather than rounded, so a header
// shape this reading does not fit falls through to the legacy scan instead of
// producing a length that would slice the statement.
func execSQLLengthWide(body []byte) (int, bool) {
	const (
		optionsLen   = 8
		sentinelAt   = 5 + optionsLen
		sqlLenAt     = sentinelAt + 8
		minWideBytes = sqlLenAt + 4
	)

	if len(body) < minWideBytes {
		return 0, false
	}

	if body[3] != closeCursorsPointer || body[4] != body[2]+1 {
		return 0, false
	}

	for i, b := range closeCursorsWideSentinel {
		if body[sentinelAt+i] != b {
			return 0, false
		}
	}

	const wideCharWidth = 3

	buffered := int(body[sqlLenAt]) | int(body[sqlLenAt+1])<<8 |
		int(body[sqlLenAt+2])<<16 | int(body[sqlLenAt+3])<<24
	if buffered <= 0 || buffered%wideCharWidth != 0 {
		return 0, false
	}

	sqlLen := buffered / wideCharWidth
	if sqlLen > execMaxSQLLen {
		return 0, false
	}

	return sqlLen, true
}

// locateExecSQLText finds the statement of exactly sqlLen bytes inside an exec
// op body.
//
// Knowing the length is most of the work; the rest is refusing to start inside
// a longer run of text, which is precisely how the window scan produced
// `SET CURRENT_SCHEMA=…` out of `ALTER SESSION SET CURRENT_SCHEMA=…`. So a
// candidate start must be preceded either by a non-text byte (the TTC framing)
// or by the CLR length prefix itself — go-ora repeats the length as a raw byte
// immediately before the text, and that byte is printable whenever the
// statement is 32..126 bytes long, which would otherwise slide the match one
// byte to the left.
//
// The run must also be text throughout and open with a SQL verb, which is what
// keeps a run of bind values or a client-identifier string from answering.
// The OCI client counts a trailing NUL in the length it declares (measured:
// a 92-character statement declared as 93 in
// testdata/sqlplus_cursor_reexec.pcapng, the NUL right there on the wire) while
// the same client declares a 23-character one as 23. So both readings are
// accepted, and the terminator — which is not statement text — is dropped.
func locateExecSQLText(body []byte, sqlLen int) (string, bool) {
	if sqlLen <= 0 || sqlLen > len(body) {
		return "", false
	}

	for _, n := range [...]int{sqlLen, sqlLen - 1} {
		if n <= 0 {
			continue
		}

		for i := 0; i+n <= len(body); i++ {
			if n == sqlLen-1 && (i+n >= len(body) || body[i+n] != 0x00) {
				continue
			}

			// A statement of 32..126 bytes has a CLR length prefix that is
			// itself a printable character, so the run can appear to start one
			// byte early — `SELECT COUNT(*) FROM user_tables` is 32 bytes and
			// its prefix is 0x20, a space. When shifting past that byte still
			// yields a valid run, it is the real one.
			if body[i] == byte(sqlLen) && execValidRunAt(body, i+1, n, sqlLen) {
				i++
			}

			if !execValidRunAt(body, i, n, sqlLen) {
				continue
			}

			return string(body[i : i+n]), true
		}
	}

	return "", false
}

// execValidRunAt is execTextRunStartsHere plus the SQL-verb requirement: the
// whole test for "the statement starts here".
func execValidRunAt(body []byte, i, n, declared int) bool {
	if i < 0 || i+n > len(body) {
		return false
	}

	return execTextRunStartsHere(body, i, n, declared) && startsWithSQLVerb(string(body[i:i+n]))
}

// execTextRunStartsHere reports whether a run of n printable bytes is exactly a
// text run — it neither continues one that started earlier nor stops in the
// middle of one that keeps going. declared is the length the header named,
// which is what the CLR prefix byte immediately before the text carries.
//
// Both ends are checked, and the far end is the one that matters most. A
// derived length that is too *long* fails on its own — the run would have to
// swallow the framing bytes behind the statement — but a length that is too
// *short* would happily return a silent prefix, with ok=true, and the gate
// would enforce against (and /queries would record) truncated text while
// believing it read the whole statement. A pattern in the tail would simply not
// be there to match. That is a worse failure than the fragment this decode
// replaced, because the fragment at least announced itself as a guess.
//
// It bites hardest on the OCI-wide length, whose x3 convention rests on two
// distinct values in one recording: `%3 != 0` rejects only about two thirds of
// wrong readings, so this boundary is the real guard. A reading it rejects
// falls through to the legacy scan rather than answering short.
func execTextRunStartsHere(body []byte, i, n, declared int) bool {
	if i > 0 && isPrintableSQLByte(body[i-1]) && body[i-1] != byte(declared) {
		return false
	}

	if i+n < len(body) && isPrintableSQLByte(body[i+n]) {
		return false
	}

	return isPrintableSQLRun(string(body[i : i+n]))
}

// isPrintableSQLByte reports whether c can appear in statement text: printable
// ASCII, the whitespace clients embed in multi-line SQL, or a byte of a
// non-ASCII character — see isPrintableSQLRun, which is what decides whether
// those bytes actually spell one.
func isPrintableSQLByte(c byte) bool {
	return c == '\t' || c == '\n' || c == '\r' || (c >= 0x20 && c <= 0x7e) || c >= 0x80
}

// isPrintableSQLRun reports whether s is statement text end to end.
//
// Non-ASCII is admitted only as **valid UTF-8**, which is what keeps this from
// being a hole: a run of arbitrary high bytes is binary, a run of well-formed
// multi-byte sequences is a string literal or a quoted identifier. An earlier
// revision of this check capped at 0x7e, and the effect was that
// `INSERT INTO t VALUES ('café')` failed the precise decode *and* the window
// scan and reached the gate as `INSERT INTO t VALUES ('caf` — the verb survived
// so read_only still fired, but a blocked-pattern or approval-pattern match in
// the tail did not. No recording carries non-ASCII SQL, so the corpus survey
// structurally cannot see that; TestNonASCIIStatementSurvivesIntact does.
//
// A client whose session charset is not UTF-8 (a Latin-1 statement, say) still
// fails here and falls through to the legacy scan. That is a deliberate
// limitation rather than an oversight: dbbat does not negotiate the charset, so
// admitting arbitrary high bytes would trade a known-good validity test for a
// guess, and the fallback is the behavior such a client had all along.
func isPrintableSQLRun(s string) bool {
	for i := range len(s) {
		if !isPrintableSQLByte(s[i]) {
			return false
		}
	}

	return utf8.ValidString(s)
}

// sqlStatementVerbs are the verbs a statement can open with. It is looksLikeSQL's
// list, named so the survey can measure against the same set.
var sqlStatementVerbs = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP",
	"ALTER", "TRUNCATE", "MERGE", "CALL", "BEGIN", "DECLARE", "WITH", "GRANT", "REVOKE",
	"EXPLAIN", "SET", "COMMIT", "ROLLBACK", "SAVEPOINT", "LOCK", "COMMENT",
}

// startsWithSQLVerb reports whether s opens with a SQL verb that ends at a word
// boundary. The boundary is what stops `GRANTED_ROLE='DBA'` reading as a GRANT
// and `DELETE_RULE, …` as a DELETE — both measured on real DBeaver frames.
func startsWithSQLVerb(s string) bool {
	upper := strings.ToUpper(strings.TrimSpace(s))

	for _, kw := range sqlStatementVerbs {
		if !strings.HasPrefix(upper, kw) {
			continue
		}

		if len(upper) == len(kw) || !isSQLWordByte(upper[len(kw)]) {
			return true
		}
	}

	return false
}
