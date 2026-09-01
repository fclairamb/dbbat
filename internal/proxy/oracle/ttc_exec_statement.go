package oracle

import (
	"strings"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

// This file replaces guessing with reading. Everything else that pulls SQL out
// of an execute message — the 40-70 and 50-75 length-prefix windows, the
// keyword scan — searches for something that *looks like* a statement. The
// execute header says how long the statement is, so the search space collapses
// to "the text run of exactly that length, bounded at both ends".
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
//     TestSurveyAlterSessionMisreadAsSet computes it; the `ALTER SESSION` in
//     every other recording is the AUTH_ALTER_SESSION key/value of the phase-2
//     AUTH message — func 0x03 sub-op 0x73, not a statement-carrying op, so the
//     gate never sees it). `ALTER` is in writeKeywords and ddlKeywords; `SET` is
//     in neither, so read_only and block_ddl did not fire.
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
	stmt, ok := decodeExecStatementText(ttcPayload)

	return stmt.Text, ok
}

// execStatement is one decoded statement in both the forms callers need.
//
// Text is what the gate matches on and the store keeps: repaired for storage by
// sanitizeSQLRun. Raw is the run exactly as it sat on the wire, and exists for
// the one caller that needs to find the statement *back* in the payload by byte
// comparison — extractPiggybackBinds anchors the bind scan on it. Handing that
// caller the repaired text would break the anchor whenever repair happened (a
// U+FFFD is three bytes where the wire had one), and a broken anchor there does
// not mean "no binds", it means binds read from the wrong offset.
//
// They are the same string whenever nothing needed repairing, which is every
// frame in testdata/.
//
// End is the offset just past the statement's last wire byte in the payload the
// decode ran on (the CLR terminator included, for a chunked statement), or 0
// when the locate could not say. It exists for the chunked form, where the
// statement does not sit contiguously in the payload, so anchoring the bind
// scan by searching for Raw cannot work — see extractPiggybackBinds.
type execStatement struct {
	Text string
	Raw  string
	End  int
}

// decodeExecStatementText is decodeExecStatement keeping the verbatim run.
func decodeExecStatementText(ttcPayload []byte) (execStatement, bool) {
	if stmt, ok := decodeExecStatementAt(ttcPayload); ok {
		return stmt, true
	}

	if end, ok := closeCursorsEnd(ttcPayload); ok {
		if stmt, ok := decodeExecStatementAt(ttcPayload[end:]); ok {
			if stmt.End > 0 {
				stmt.End += end
			}

			return stmt, true
		}
	}

	return execStatement{}, false
}

// decodeExecStatementAt is decodeExecStatementText for a payload that must
// already begin at the exec op header.
func decodeExecStatementAt(body []byte) (execStatement, bool) {
	if !isPiggybackExecHeader(body) || len(body) < execHeaderMinLen {
		return execStatement{}, false
	}

	sqlLen, ok := execSQLLength(body)
	if !ok {
		return execStatement{}, false
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

	// Guarded rather than relying on the caller's execHeaderMinLen check: this
	// function has a second entry point in the tests, and an unconditional
	// index is the shape that cost a panic below.
	if len(body) <= 3 {
		return 0, false
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
func locateExecSQLText(body []byte, sqlLen int) (execStatement, bool) {
	if sqlLen <= 0 || sqlLen > len(body) {
		return execStatement{}, false
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

			stmt, ok := execRunTextAt(body, i, n, sqlLen)
			if !ok {
				continue
			}

			stmt.End = i + n

			return stmt, true
		}
	}

	return locateChunkedExecSQLText(body, sqlLen)
}

// clrChunkedMinLen is the smallest statement the CLR long form can carry: below
// 252 bytes every client writes the single-length-byte short form, so a shorter
// declared length can never legitimately be chunked and the scan is skipped.
const clrChunkedMinLen = 0xFC

// locateChunkedExecSQLText finds a statement the client sent in CLR long form:
// a 0xFE marker followed by length-prefixed chunks whose contents concatenate
// to exactly sqlLen bytes, closed by a zero-length terminator.
//
// This is not an exotic layout — it is how every thin client writes a
// statement past its chunk size. python-oracledb, go-ora and JDBC thin all
// chunk at 32767 bytes once the server advertises UseBigClrChunks (which every
// supported server does), so a statement of 32768+ bytes has a chunk-length
// prefix *in the middle of its text* and the contiguous scan above
// structurally cannot find it. That was measured in production on 2026-09-01:
// a 33241-byte MERGE declared `sqlLen=0x81d9` arrived as `FE 02 7F FF <32767
// bytes> 02 01 DA <474 bytes> 00`, the contiguous scan failed, and the
// last-resort keyword scan handed the gate a prefix cut at the chunk boundary
// — refused whenever the cut landed inside a string literal, silently gated
// short otherwise. Statements up to 32767 bytes are a single chunk and the
// contiguous scan finds them, which is exactly why the reported failure
// boundary sat at 32768.
//
// Chunk lengths come in the two encodings the AUTH leg already reads
// (readCLRVariant): TTC compressed integers under UseBigClrChunks, single raw
// bytes without it. Both are tried — the walk is self-validating enough that
// trying the wrong one cannot mis-answer: every chunk must be printable
// statement text, the totals must hit sqlLen exactly, the terminator must be
// there, and the result must open with a SQL verb.
func locateChunkedExecSQLText(body []byte, sqlLen int) (execStatement, bool) {
	if sqlLen < clrChunkedMinLen {
		return execStatement{}, false
	}

	for i := 0; i+1 < len(body); i++ {
		if body[i] != 0xFE {
			continue
		}

		for _, bigChunks := range [...]bool{true, false} {
			if stmt, ok := chunkedExecRunAt(body, i, sqlLen, bigChunks); ok {
				return stmt, true
			}
		}
	}

	return execStatement{}, false
}

// chunkedExecRunAt walks the CLR long form opening at the 0xFE marker and
// reports the statement it carries, when — and only when — the chunks
// concatenate to exactly sqlLen bytes of statement text.
func chunkedExecRunAt(body []byte, marker, sqlLen int, bigChunks bool) (execStatement, bool) {
	// Allocated on the first chunk rather than up front: most 0xFE bytes in a
	// payload are not the marker (the OCI pointer sentinel alone is eight of
	// them) and fail the walk within a byte or two, which must not cost a
	// statement-sized allocation each.
	var raw []byte

	pos := marker + 1

	for {
		var chunkLen, n int

		if bigChunks {
			chunkLen, n = readCompressedInt(body[pos:])
			if n == 0 {
				return execStatement{}, false
			}
		} else {
			if pos >= len(body) {
				return execStatement{}, false
			}

			chunkLen, n = int(body[pos]), 1
		}

		pos += n

		if chunkLen == 0 {
			break
		}

		if pos+chunkLen > len(body) || len(raw)+chunkLen > sqlLen {
			return execStatement{}, false
		}

		if raw == nil {
			raw = make([]byte, 0, sqlLen)
		}

		raw = append(raw, body[pos:pos+chunkLen]...)
		pos += chunkLen
	}

	if len(raw) != sqlLen {
		return execStatement{}, false
	}

	text, ok := sanitizeSQLRun(string(raw))
	if !ok || !startsWithSQLVerb(text) {
		return execStatement{}, false
	}

	return execStatement{Text: text, Raw: string(raw), End: pos}, true
}

// execValidRunAt is execTextRunStartsHere plus the SQL-verb requirement: the
// whole test for "the statement starts here".
func execValidRunAt(body []byte, i, n, declared int) bool {
	_, ok := execRunTextAt(body, i, n, declared)

	return ok
}

// execRunTextAt is execValidRunAt that also hands back the statement, in both
// the repaired-for-storage and verbatim forms (see execStatement).
func execRunTextAt(body []byte, i, n, declared int) (execStatement, bool) {
	if i < 0 || n < 0 || i+n > len(body) {
		return execStatement{}, false
	}

	if !execTextRunStartsHere(body, i, n, declared) {
		return execStatement{}, false
	}

	raw := string(body[i : i+n])

	text, ok := sanitizeSQLRun(raw)
	if !ok || !startsWithSQLVerb(text) {
		return execStatement{}, false
	}

	return execStatement{Text: text, Raw: raw}, true
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

// sanitizeSQLRun reports whether s is statement text end to end, and returns
// the text to hand on.
//
// It is deliberately **not** a "valid UTF-8 or drop" check, for the reason
// docs/oracle.md already gives for OER diagnostics: dbbat does not know the
// session charset, so a perfectly ordinary `INSERT INTO t VALUES ('café')` from
// a WE8ISO8859P1 session — the common case on European estates — is not valid
// UTF-8. An earlier revision of this dropped such a run, and the consequence was
// not merely lost fidelity: the decoder fell through to the keyword scan, which
// truncated the statement at the first accented byte, so the gate saw
// `INSERT INTO t VALUES ('caf` and a blocked pattern or approval pattern in the
// tail stopped matching. That is the precise-enforcement hole this file exists
// to close, left open for exactly the client population the docs call common.
//
// So it reuses the answer this repo already shipped for the same question:
// shared.SanitizeStatementText — no control bytes, and no more than a quarter
// of the runes undecodable, with the undecodable ones repaired to U+FFFD.
// Binary has control bytes and is undecodable throughout; a sentence with
// accents in it is neither. Repairing rather than keeping the raw bytes is also
// what makes the result storable: `queries.sql_text` is a Postgres `text`
// column.
//
// The header-declared length, the two-sided run boundary and the leading verb
// are what actually discriminate a statement from binary here; this is the
// belt on top of those braces, which is why it can afford to be charitable.
//
// No recording carries non-ASCII SQL, so the corpus survey structurally cannot
// see any of this — TestNonASCIIStatementSurvivesIntact does.
func sanitizeSQLRun(s string) (string, bool) {
	for i := range len(s) {
		if !isPrintableSQLByte(s[i]) {
			return "", false
		}
	}

	return shared.SanitizeStatementText(s)
}

// isPrintableSQLRun is sanitizeSQLRun as a predicate, for the boundary tests
// that only ask whether a run is text.
func isPrintableSQLRun(s string) bool {
	_, ok := sanitizeSQLRun(s)

	return ok
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
//
// Leading comments are stepped over first, because a statement is allowed to
// open with one and the database ignores it: `-- header\nSELECT …` is a SELECT
// to Oracle. Refusing to see the verb behind a comment is not a tighter check —
// it made the header-anchored decode reject the true run and fall through to
// the keyword scan, which then started the "statement" at whatever verb the
// comment happened to contain. A `-- MERGE s'execute` header line thus became a
// statement opening at MERGE whose apostrophe opened a string that never
// closed, and the client was refused with "a quoted run was left open"
// (measured in production, 2026-09-01).
func startsWithSQLVerb(s string) bool {
	upper := strings.ToUpper(skipLeadingSQLComments(s))

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

// skipLeadingSQLComments returns s with leading whitespace, `-- …` line
// comments and `/* … */` block comments removed, so the caller sees the first
// byte the server would parse. A remainder that is nothing but an unterminated
// comment (or an unterminated block comment) comes back empty, which no verb
// matches — the fail-closed direction.
func skipLeadingSQLComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n\v\f")

		switch {
		case strings.HasPrefix(s, "--"):
			nl := strings.IndexByte(s, '\n')
			if nl < 0 {
				return ""
			}

			s = s[nl+1:]

		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s[2:], "*/")
			if end < 0 {
				return ""
			}

			s = s[2+end+2:]

		default:
			return s
		}
	}
}
