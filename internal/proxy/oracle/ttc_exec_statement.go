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

// execMaxSQLLen bounds a plausible statement length: anything past 1MB means
// the walk landed on the wrong bytes.
//
// It used to justify itself with "Oracle's own limit is 64K for the SQL text of
// a single statement". That was folklore and it is wrong — a real 23ai parses a
// 128 MB statement (measured; see maxTaggableStatementBytes and docs/oracle.md).
// 1MB is dbbat's own choice about what it is willing to *read*, not a limit of
// the server's, and it is what bounds reassembly (maxStatementReassembly) and
// tagging (maxTaggableStatementBytes) too. A statement past it is still
// forwarded — it is decoded partially, and not tagged.
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
	field, ok := execSQLLengthField(body)

	return field.value, ok
}

// execSQLLenField is where an exec op declares its statement's length, and in
// which of the two encodings execSQLLength knows.
//
// It exists for the rewriter, which has to put a *different* number in that
// field: a decoder only needs the value, an encoder needs the exact span it
// occupies, because widening the field shifts every byte behind it. See
// ttc_statement_rewrite.go.
type execSQLLenField struct {
	value int
	at    int
	width int
	kind  stmtLenKind
}

// execSQLLengthField is execSQLLength keeping the field's position. The two
// share one walk deliberately: a second implementation of "where does this
// header declare its length" is exactly the drift that would let the gate read
// one field and the rewriter overwrite another.
func execSQLLengthField(body []byte) (execSQLLenField, bool) {
	if field, ok := execSQLLengthWideField(body); ok {
		return field, true
	}

	header, ok := execThinHeader(body)
	if !ok || header.sqlLen.value <= 0 {
		return execSQLLenField{}, false
	}

	return header.sqlLen, true
}

// execThinHeaderFields is a thin (compressed-int) exec header walked to the end
// of its statement-length field.
//
// It exists because a length of **zero** is not the same answer as "this header
// does not walk", and the two used to be spelled the same way. A `03 5e`
// declaring no statement at all is a client re-executing a cursor it already
// parsed — ojdbc6 sends exactly that, see execNoStatementCursor — so the walk
// reports the value it read and lets each caller decide: the decoder wants a
// statement and refuses zero, the re-execution reading wants zero and nothing
// else.
type execThinHeaderFields struct {
	// cursorID is the cursor the exec names, as the header declares it. It is
	// 0 on a parse that asks the server to allocate one.
	cursorID int
	// sqlLen is the statement-length field, value included; value 0 means the
	// header declared no statement.
	sqlLen execSQLLenField
}

// execThinHeader walks the thin exec header of body. See execSQLLength for the
// layout, and execThinHeaderFields for why it is one walk rather than two.
func execThinHeader(body []byte) (execThinHeaderFields, bool) {
	// Guarded rather than relying on the caller's execHeaderMinLen check: this
	// function has a second entry point in the tests, and an unconditional
	// index is the shape that cost a panic below.
	if len(body) <= 3 {
		return execThinHeaderFields{}, false
	}

	pos := 3
	if body[3] == 0 {
		pos = 4
	}

	// options
	_, n := readCompressedInt(body[pos:])
	if n == 0 {
		return execThinHeaderFields{}, false
	}

	pos += n

	// cursor id
	cursorID, n := readCompressedInt(body[pos:])
	if n == 0 {
		return execThinHeaderFields{}, false
	}

	pos += n

	// The "cursor id is zero" flag. readCompressedInt can consume the buffer
	// exactly, so stepping over this byte unchecked would slice past the end —
	// `03 5e 01 04 aa bb cc dd 03 ee ff 00` is twelve bytes that clear every
	// other guard and does it. The recover in interceptClientMessage would
	// contain the panic, but containment means the frame is forwarded ungated,
	// which is the bypass class this decode exists to close.
	if pos >= len(body) {
		return execThinHeaderFields{}, false
	}

	pos++

	sqlLen, n := readCompressedInt(body[pos:])
	if n == 0 || sqlLen < 0 || sqlLen > execMaxSQLLen {
		return execThinHeaderFields{}, false
	}

	return execThinHeaderFields{
		cursorID: cursorID,
		sqlLen:   execSQLLenField{value: sqlLen, at: pos, width: n, kind: stmtLenCompressed},
	}, true
}

// execNoStatementCursor reports the cursor id of an execute op that declares a
// **zero-length** statement — the client re-running a cursor it already parsed,
// with no statement text on the wire.
//
// This is a third re-execution shape, alongside the SQL-less OALL8
// (decodeOALL8 → OALL8NoSQLError) and the `03 4e` / `03 04` piggyback
// (decodeCursorReexec). It was found on 2026-09-19 in an **ojdbc6 11.2.0.4**
// recording (testdata/ojdbc6_legacy.pcapng, packet #20): that driver re-executes
// a PreparedStatement by resending the parse op, `03 5e`, with its statement
// length set to zero:
//
//	03 5e 06 02 80 60 01 03 00 00 01 01 0d 00 …
//	      ^seq  ^options  ^cursor 3 ^flag ^sqlLen = 0
//
// Until then that frame decoded as "could not find SQL text", and a decode
// failure is forwarded ungated — so from the second execution of any prepared
// statement on, read_only, block_ddl, ValidateOracleQuery, the approval
// patterns, the `queries` row and the quota all applied to the parse alone.
//
// The reading is deliberately narrow, because the cost of a false positive here
// is a refused re-execution on a client that was working: the header must walk
// cleanly in the thin encoding, its length field must be an explicit zero, and
// the cursor id it names must be plausible. Anything else is left to the
// statement decoders, which is why callers consult this only once those have
// failed to find a statement.
//
// The frame is accepted either as the exec op itself or with the exec stapled
// behind a close-cursors piggyback, the same two forms decodeExecStatementText
// reads — a `11 69` twin re-executes just as ungated as a bare `03 5e` would.
func execNoStatementCursor(ttcPayload []byte) (uint16, bool) {
	if cursorID, ok := execNoStatementCursorAt(ttcPayload); ok {
		return cursorID, true
	}

	if end, ok := closeCursorsEnd(ttcPayload); ok && end < len(ttcPayload) {
		return execNoStatementCursorAt(ttcPayload[end:])
	}

	return 0, false
}

// execNoStatementCursorAt is execNoStatementCursor for a payload that must
// already begin at the exec op header.
func execNoStatementCursorAt(body []byte) (uint16, bool) {
	if !isPiggybackExecHeader(body) {
		return 0, false
	}

	// A wide header that declares a statement is a parse, not a re-execution —
	// the same answer the thin walk gives for a non-zero length, and it must be
	// taken before the SQL-less wide reading below, whose own guard is the
	// absence of that statement.
	if _, wide := execSQLLengthWideField(body); wide {
		return 0, false
	}

	// The OCI wide header carries its cursor id in the header prefix rather than
	// where the thin walk looks, so it gets its own reading. See
	// execWideNoStatementCursor.
	if cursorID, ok := execWideNoStatementCursor(body); ok {
		return cursorID, true
	}

	header, ok := execThinHeader(body)
	if !ok || header.sqlLen.value != 0 {
		return 0, false
	}

	// Cursor 0 means "allocate one", which is a parse, not a re-execution —
	// the same reading decodeCursorReexec refuses, and for the same reason.
	if header.cursorID <= 0 || header.cursorID > cursorReexecMaxID {
		return 0, false
	}

	return uint16(header.cursorID), true
}

// The OCI wide exec header, by offset. execSQLLengthWideField and
// execWideNoStatementCursor read different fields of the same header, so the
// layout is written once: a second copy of these numbers is how the gate would
// come to read one field while the length walk validates another.
//
//	[0..2]   03 5e seq
//	[3]      0x01                  constant
//	[4]      seq+1                 the NEXT TTC message's sequence number
//	[5..8]   options               uint32 little-endian
//	[9..12]  cursorID              uint32 little-endian   <- the re-execution
//	[13..20] fe x8                 the statement's pointer sentinel
//	[21..24] sqlLen*3              uint32 little-endian   <- the parse
const (
	execWideCursorIDAt = 9
	execWideSentinelAt = 13
	execWideSQLLenAt   = execWideSentinelAt + 8
)

// execWideNoStatementCursor reports the cursor id of an OCI (wide) execute op
// that declares **no statement** — sqlplus, Instant Client or SQL*Developer
// over OCI re-running a cursor it already parsed, which is how every `PRINT rc`
// of a REF cursor and every repeat of an ordinary prepared statement reaches
// the wire on those clients.
//
// It is the wide twin of the thin reading in execNoStatementCursorAt, and it
// exists because that reading refused this header outright: the frame decoded
// as "could not find SQL text", a decode failure is forwarded ungated, and so
// from the second execution of any cursor on an OCI session read_only,
// block_ddl, the approval patterns, the `queries` row and the quota all applied
// to the parse alone.
//
// Where the id sits was measured, not guessed. The eight bytes after the pad —
// which execSQLLengthWideField only ever had to skip, and so calls "options" —
// are two little-endian uint32s, and the second is the cursor id. Two witnesses
// agree, independently of this walk:
//
//   - across the whole recorded corpus, that field is zero on every wide exec
//     that carries a statement (a parse asks the server to allocate a cursor)
//     and non-zero on exactly the two that carry none;
//   - those two ids, 2 and 5, are the ids refCursorIDsInBindOutput reads out of
//     the call responses recorded beside them in the same session and in the
//     same order — a part of the wire this walk never touches. See
//     TestDumpReplay_OCIRefCursorIDsMatchTheCursorsTheClientDrives.
//
// The reading is as narrow as the thin one, for the same reason: a false
// positive refuses a re-execution on a client that was working. The header pad
// must fit, the statement's pointer sentinel must be **absent** (a SQL-less
// frame writes zeros where a parse writes `fe x8`), and the id must be plausible
// — non-zero, because zero is a parse, and within cursorReexecMaxID.
func execWideNoStatementCursor(body []byte) (uint16, bool) {
	if len(body) < execWideSQLLenAt {
		return 0, false
	}

	if body[3] != closeCursorsPointer || body[4] != body[2]+1 {
		return 0, false
	}

	// Zeros where a statement-carrying frame puts the sentinel. Requiring them
	// rather than merely taking execSQLLengthWideField's refusal is what keeps a
	// header this walk does not understand — a truncated one, or a shape a future
	// client invents — from being gated against whatever those four bytes hold.
	for i := range closeCursorsWideSentinel {
		if body[execWideSentinelAt+i] != 0 {
			return 0, false
		}
	}

	cursorID := uint32(body[execWideCursorIDAt]) | uint32(body[execWideCursorIDAt+1])<<8 |
		uint32(body[execWideCursorIDAt+2])<<16 | uint32(body[execWideCursorIDAt+3])<<24
	if cursorID == 0 || cursorID > cursorReexecMaxID {
		return 0, false
	}

	return uint16(cursorID), true
}

// execSQLLengthWideField reads the statement length out of the OCI wide exec header.
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
// wideCharWidth is the multiplier the OCI exec header applies to the statement
// length: the client sizes the buffer for its widest character encoding rather
// than reporting the byte count.
const wideCharWidth = 3

// It keeps the field's position as well as its value, for the same reason
// execSQLLengthField does: a decoder needs the number, an encoder needs the span.
func execSQLLengthWideField(body []byte) (execSQLLenField, bool) {
	const (
		sentinelAt   = execWideSentinelAt
		sqlLenAt     = execWideSQLLenAt
		minWideBytes = sqlLenAt + 4
	)

	if len(body) < minWideBytes {
		return execSQLLenField{}, false
	}

	if body[3] != closeCursorsPointer || body[4] != body[2]+1 {
		return execSQLLenField{}, false
	}

	for i, b := range closeCursorsWideSentinel {
		if body[sentinelAt+i] != b {
			return execSQLLenField{}, false
		}
	}

	buffered := int(body[sqlLenAt]) | int(body[sqlLenAt+1])<<8 |
		int(body[sqlLenAt+2])<<16 | int(body[sqlLenAt+3])<<24
	if buffered <= 0 || buffered%wideCharWidth != 0 {
		return execSQLLenField{}, false
	}

	sqlLen := buffered / wideCharWidth
	if sqlLen > execMaxSQLLen {
		return execSQLLenField{}, false
	}

	return execSQLLenField{value: sqlLen, at: sqlLenAt, width: 4, kind: stmtLenWideUB4}, true
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
