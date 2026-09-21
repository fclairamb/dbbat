package oracle

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// oerInfo holds fields decoded from a TTC OER (error/status) message.
// An OER follows every execute on v315+ connections: for successful DML it
// carries the affected-row count in CurRowNumber; for failed statements it
// carries the ORA error code and message text.
type oerInfo struct {
	CallStatus   int // call-status flags; bit 0x010000 = end-of-call
	SeqNumber    int
	CurRowNumber int // rows processed (rows affected for DML, 0 for DDL)
	ErrorCode    int // 0 = success, 1403 = end-of-data, else the ORA-NNNNN code
	CursorID     int // cursor the server assigned to the statement, 0 when none
	ErrorMessage string
}

// oerEndOfCallBit is set in CallStatus on some calls and not others. It reads
// like a client trait on the successful ones — every OER a go-ora session
// carries has it, none of a python-oracledb thin session's does — but that is
// a coincidence of which calls were captured first: on a *failing* call the two
// clients agree, and only a failed DDL carries it (see decodeErrorOER).
//
// Byte runs inside a row stream that happen to start with 0x04 don't carry it
// either, which is what makes it worth keeping as the discriminator wherever
// row bytes are what a false positive would be made of and nothing else stands
// in for it: anything scanned at an arbitrary offset inside a Response mid-fetch
// is data. Outside those, see findPlausibleOERInResponse, decodeErrorOER and
// midFetchOERNamesTheStreamingCursor.
//
// The fixed-width OCI encoding has no equivalent of it to demand — measured,
// *every* standalone summary object in the OCI recordings reports its status
// with a bare CallStatus 0x1 — so a standalone func=0x04 reporting success or
// ORA-01403 is read there under the layout anchors and the cursor bounds
// instead, and only at byte 0 of a packet — with a second bound inside a row
// stream, where only the end-of-data half may end the call. See
// decodeFixedStatusOERAt and session.statusOERMayEndTheCall.
const oerEndOfCallBit = 0x010000

// oraNoDataFound is ORA-01403, the normal end-of-data status — not an error.
const oraNoDataFound = 1403

// oerFieldMaxSizes bounds the encoded size of each leading OER field:
// callStatus, seqNum, curRowNumber, errNum, arrayElemWErr, arrayElemErrNo,
// cursorID. ORA error codes go up to 99999 (3 bytes); row counts get the
// full 8 bytes.
var oerFieldMaxSizes = [...]int{4, 2, 8, 4, 2, 2, 4}

// decodeOERAt decodes an OER message whose 0x04 marker sits at payload[offset].
// Field layout (all TTC compressed integers):
//
//	[0x04] callStatus seqNum curRowNumber errNum arrayElemWErr arrayElemErrNo cursorID ...
//
// Returns nil when the bytes do not validate as an OER (decode failure,
// oversized field, or missing end-of-call bit).
//
// It has a second half, for the fixed-width OCI encoding, and that half is
// offered **only at offset 0** — see decodeFixedStatusOERAt for the measurement
// that draws the line there. The bit stays the whole discriminator on the
// compressed reading, at every offset, unchanged.
func decodeOERAt(shape oerShape, payload []byte, offset int) *oerInfo {
	if info := decodeCompressedEndOfCallOERAt(payload, offset); info != nil {
		return info
	}

	return decodeFixedStatusOERAt(shape, payload, offset)
}

// decodeCompressedEndOfCallOERAt is the reading decodeOERAt has always had: the
// seven leading fields as TTC compressed integers, accepted only when the call
// status carries the end-of-call bit.
func decodeCompressedEndOfCallOERAt(payload []byte, offset int) *oerInfo {
	info, rest := decodeOERFieldsAt(payload, offset)
	if info == nil || info.CallStatus&oerEndOfCallBit == 0 {
		return nil
	}

	if info.ErrorCode != 0 {
		info.ErrorMessage = extractORAMessage(payload[rest:])
	}

	return info
}

// decodeFixedStatusOERAt reads a **fixed-width** summary object reporting
// success or ORA-01403 — the status OER that ends a call on an OCI client — and
// is decodeOERAt's answer to the end-of-call bit not existing for it.
//
// Demanding the bit here was the conservative option, and it was measured to be
// worth nothing: replayed through a real session, *every* standalone summary
// object in the two OCI recordings carries CallStatus `0x1`. The five
// `SELECT 1 AS n FROM dual` re-executions of sqlplus_cursor_reexec.pcapng and
// the `SELECT DECODE(USER, …)` login probe both recordings open with report
// ORA-01403 with a bare `0x1`; the only OCI call status with the bit in the
// corpus is the ORA-00942 of a failed DROP (`0x10001`), which decodeErrorOER
// already reads. A bit-demanding fixed-width reading would therefore complete
// exactly nothing — the very frames this exists for would stay pending until the
// next statement's flushPendingQuery closed them, which is the symptom.
//
// What replaces the bit is the bound set findPlausibleOERInResponse already
// trusts on this very object, on top of decodeOERFixedFieldsAt's own RetCode
// anchor: the code must be success or end-of-data, the ECID sequence must fit
// its 16-bit field, and the cursor id must be a plausible one. A *failure*
// reported fixed-width is deliberately not accepted here — decodeErrorOER holds
// it to the stronger proof of a diagnostic naming its own code.
//
// Two restrictions keep that from widening what row bytes can be mistaken for,
// and both are measured rather than argued:
//
//   - **offset 0 only.** Across the recordings in testdata/, 649 server packets
//     arrive while a row stream is active; run at every 0x04 offset *inside*
//     them, this predicate accepts 149 — and they are not junk. An OCI fetch
//     response carries a real summary object of exactly this shape at a constant
//     offset in *every* continuation packet, naming the streaming cursor and
//     reporting the running row count (13001, 13101, … 14901 in
//     sqlplus_midfetch_fail.pcapng), and two of them even carry the end-of-call
//     bit. Accepting one ends the call in the middle of the fetch. So a 0x04
//     that a scan *found* is never read this way; only one that was the packet's
//     own leading byte, which is what the router already believed. That is what
//     leaves findOERInResponse's mid-row-stream scan exactly as strict as it was.
//   - **inside a row stream, end-of-data only**, which is the caller's half of
//     the same bound: see session.statusOERMayEndTheCall. At offset 0 the corpus
//     is unambiguous — of those 649 mid-stream packets 11 lead with 0x04, 4 the
//     genuine mid-fetch ORA-01722 failures (which this predicate refuses by
//     their code alone) and 7 the ORA-01403 an OCI fetch *ends* on — while all
//     149 objects that genuinely travel inside the stream report success with a
//     running count. So a bit-less success is still refused there and a bit-less
//     end-of-data naming the streaming cursor ends the call, which is what it
//     means.
//
// A third restriction is about *ordering* rather than row bytes: the shape must be
// **learned**, so the unlearned two-layout fallback decodeOERFixedFieldsAt offers
// is not available here. handleOERStatus runs this reading before decodeErrorOER,
// and a status accepted early is a diagnostic never proven — so on a session that
// has not yet learned its encoding, a bit-less *error* OER that happened to
// satisfy one of the two layouts' anchors could be completed as a status, losing
// its ORA text. It costs nothing: readUpstreamAuthMessages learns the shape off
// the AUTH exchange, long before any statement runs, and in both OCI recordings
// every standalone status OER arrives with the shape already learned
// (TestDecodeFixedStatusOERAt_KeepsItsAnchors pins the refusal).
func decodeFixedStatusOERAt(shape oerShape, payload []byte, offset int) *oerInfo {
	if offset != 0 || !shape.tailLearned || !shape.fixedWidth {
		return nil
	}

	info, _ := decodeOERFixedFieldsAt(shape, payload, offset)
	if !plausibleStatusOER(info) {
		return nil
	}

	return info
}

// decodeOERFieldsAt decodes the leading integer fields of an OER whose 0x04
// marker sits at payload[offset], without judging whether the result is a real
// OER. It returns the fields and the offset just past them, or nil.
//
// Split out of decodeOERAt because the end-of-call bit is not universal. It
// looks like a client trait on the *successful* calls that first showed it —
// go-ora's server leg carries it there, while python-oracledb's connections get
// OERs with CallStatus 1–2 (testdata/python_thin_cursor_reexec.pcapng) — but it
// is not one: on a *failing* call the two clients agree, and only a failed DDL
// carries it (testdata/{python_thin,go_ora}_failed_stmt.pcapng, and
// measuredFailures in failed_stmt_replay_test.go). Either way the conclusion is
// the same, which is why it survived the correction: neither cursor-id learning
// nor the completion of a call that has already left its row stream can afford
// to demand the bit; the paths where row bytes could impersonate an OER still
// do, through decodeOERAt.
func decodeOERFieldsAt(payload []byte, offset int) (*oerInfo, int) {
	if offset >= len(payload) || payload[offset] != 0x04 {
		return nil, 0
	}

	pos := offset + 1

	var fields [len(oerFieldMaxSizes)]int

	for i, maxSize := range oerFieldMaxSizes {
		val, n := readCompressedInt(payload[pos:])
		if n == 0 || n-1 > maxSize {
			return nil, 0
		}

		fields[i] = val
		pos += n
	}

	return &oerInfo{
		CallStatus:   fields[0],
		SeqNumber:    fields[1],
		CurRowNumber: fields[2],
		ErrorCode:    fields[3],
		CursorID:     fields[6],
	}, pos
}

// decodeOERFieldsAtLayout is decodeOERFieldsAt for the **fixed-width**
// OCI/sqlplus encoding: the same summary object, marshaled as little-endian
// integers at constant offsets instead of TTC compressed ones. It is the
// reading half of encodeOERFixedWidth, and it reads the fields back at the very
// offsets that encoder writes them to.
//
// Without it, dbbat could write this encoding and not read it — so on an OCI
// client (sqlplus, Instant Client, SQL*Developer over OCI) decodeOERFieldsAt
// returned nil for every summary object the server sent, decodeErrorOER refused
// them all, and *every* failing statement was recorded in `queries` as a
// success. Cursor-id learning went blind on the same sessions for the same
// reason, which is why nothing there had a streaming cursor to compare against.
//
// It anchors rather than trusting a length, exactly as oerFixedWidthTailFieldsAt
// already does for the shape it learns: the error number at layout.errNum must
// be repeated as the RetCode at layout.retCode. That single invariant is what
// makes a *prefix length* self-validating, and it is the only structural proof
// available here — every other field in the prefix is legitimately zero, so
// there is nothing else to check the layout against. The two layouts put their
// RetCode 66 bytes apart, so a block written for one cannot satisfy the other.
//
// A run of zeroes satisfies the repetition trivially, so the call status has to
// be non-zero as well; every OCI summary object measured carries one (0x1
// mid-fetch, the end-of-call word otherwise). Callers add their own proof on top
// — the ORA diagnostic naming the code in decodeErrorOER, the cursor bounds in
// findPlausibleOERInResponse — precisely as they do on the compressed path.
//
// Returns the fields and the offset just past the row count, or nil.
func decodeOERFieldsAtLayout(payload []byte, offset int, layout oerFixedLayout) (*oerInfo, int) {
	if offset >= len(payload) || payload[offset] != byte(TTCFuncOERR) {
		return nil, 0
	}

	block := payload[offset:]
	if len(block) < layout.prefixLen+max(layout.rowCountWidth, 1) {
		return nil, 0
	}

	errCode := int(binary.LittleEndian.Uint16(block[layout.errNum:]))
	if int(binary.LittleEndian.Uint32(block[layout.retCode:])) != errCode {
		return nil, 0
	}

	callStatus := binary.LittleEndian.Uint32(block[layout.callStatus:])
	if callStatus == 0 {
		return nil, 0
	}

	rowCount, width, ok := decodeOERFixedRowCount(block, layout)
	if !ok {
		return nil, 0
	}

	return &oerInfo{
		CallStatus:   int(callStatus),
		SeqNumber:    int(binary.LittleEndian.Uint16(block[layout.ecid:])),
		CurRowNumber: rowCount,
		ErrorCode:    errCode,
		CursorID:     int(binary.LittleEndian.Uint16(block[layout.cursorID:])),
	}, offset + layout.prefixLen + width
}

// decodeOERFixedRowCount reads the row count that follows a fixed-width prefix
// and returns it with its encoded width. The 32-bit layout leaves it a TTC
// compressed integer — there is no fixed form for it on the wire there — while
// the 64-bit one writes a plain 8-byte field. See oerFixedLayout.rowCountWidth.
func decodeOERFixedRowCount(block []byte, layout oerFixedLayout) (int, int, bool) {
	if layout.rowCountWidth == 0 {
		val, n := readCompressedInt(block[layout.prefixLen:])
		if n == 0 {
			return 0, 0, false
		}

		return val, n, true
	}

	raw := binary.LittleEndian.Uint64(block[layout.prefixLen:])
	if raw > math.MaxInt {
		return 0, 0, false
	}

	return int(raw), layout.rowCountWidth, true
}

// decodeOERFixedFieldsAt decodes a fixed-width summary object at payload[offset]
// under the layouts `shape` admits — the decision of *which* layout, made by
// asking the session rather than by sniffing.
//
// The session learns fixedWidth / fixedWidth64 from the upstream's own OERs
// (learnOERShape), and that upstream negotiated with this very client's
// forwarded capabilities, so a learned shape is an observation and not a guess.
// A session that has learned it speaks the compressed encoding is not offered
// the fixed-width reading at all — which is what keeps this from widening what a
// thin client's row bytes could be mistaken for.
//
// The ordering trap is that the shape is learned from a *server* OER, so the
// first OER of a session can arrive before anything is known.
// readUpstreamAuthMessages makes that mostly moot — it learns off the AUTH
// exchange, long before any statement runs — and where it does not, the fallback
// is to try **both** layouts under the RetCode anchor rather than to accept the
// wrong one. Widest first, for the reason oerFixedWidthTailFieldsAt gives: the
// 64-bit layout's own error number sits where the 32-bit one has zeroes.
func decodeOERFixedFieldsAt(shape oerShape, payload []byte, offset int) (*oerInfo, int) {
	if shape.tailLearned {
		if !shape.fixedWidth {
			return nil, 0
		}

		return decodeOERFieldsAtLayout(payload, offset, shape.layoutFor())
	}

	if info, rest := decodeOERFieldsAtLayout(payload, offset, oerFixed64Layout); info != nil {
		return info, rest
	}

	return decodeOERFieldsAtLayout(payload, offset, oerFixed32Layout)
}

// decodeOERFieldsForShape decodes the leading fields of an OER at
// payload[offset] in whichever of the two encodings this session's upstream
// speaks, without judging whether the result is a real OER.
//
// It is for the callers that have no validator of their own to fall back on. The
// two that do — decodeErrorOER and findPlausibleOERInResponse — try each
// encoding under their own proof instead, because a fixed-width block decodes as
// a run of zero-valued compressed fields (its call status `01 00 00 00` reads as
// a one-byte field holding 0), so "compressed first, fixed only if that returns
// nil" would stop at a bogus success and never reach the real fields.
func decodeOERFieldsForShape(shape oerShape, payload []byte, offset int) (*oerInfo, int) {
	if shape.tailLearned && shape.fixedWidth {
		return decodeOERFixedFieldsAt(shape, payload, offset)
	}

	if info, rest := decodeOERFieldsAt(payload, offset); info != nil {
		return info, rest
	}

	return decodeOERFixedFieldsAt(shape, payload, offset)
}

// decodeErrorOER decodes a standalone OER that *reports a failure*, without
// requiring the end-of-call bit, and proves the bytes are a diagnostic before
// handing them back.
//
// The bit was believed to be a client trait — set on every OER a go-ora session
// carries, absent on python-oracledb thin. Measured against Oracle Free 23ai it
// is neither: it tracks the *call*, not the client. Of the six failure shapes in
// testdata/{python_thin,go_ora}_failed_stmt.pcapng, exactly one (a failed DDL)
// carries it, on both clients; the SELECT on a missing table, the unique-key
// violation, the divide-by-zero, the PL/SQL RAISE and the PL/SQL compile error
// all arrive with CallStatus 1 or 5. decodeOERAt refuses every one of them, so
// their ORA text never reached queries.error on any client at all.
//
// What replaces the bit here is proof rather than a looser bound, the same
// standard decodeTTCResponse is held to (legacyResponseErrorMessage):
//
//   - the seven leading fields must decode as compressed ints;
//   - the error code must be a real failure — not success, not ORA-01403 — and
//     inside the range an Oracle code can occupy;
//   - the tail must carry a printable ORA-/PLS-/TNS- diagnostic;
//   - and that diagnostic must *name the same code* the fields reported.
//
// The last one is what makes this safe on a path routed by byte 0 alone: a run
// of row bytes that happens to decode as seven ints would also have to be
// followed by the ASCII spelling of the number its fourth field landed on.
// Every measured diagnostic satisfies it, PL/SQL included (errNum 6550 →
// "ORA-06550: line 1, column 7:"), which is why it is a check and not a hope.
//
// Mid-row-stream, where a 0x04 lead byte can be a row value's length prefix
// rather than a function code, this proof is necessary but not sufficient: the
// caller must additionally require the OER to name the streaming cursor. See
// handleOERStatus and midFetchOERNamesTheStreamingCursor.
//
// Both encodings are tried, each under the full proof above rather than one
// after the other's field decode: the compressed reading of a fixed-width block
// succeeds with every field at zero, so trying it first and stopping there is
// how an OCI client's diagnostics were lost. The fixed-width reading is offered
// only for the layouts `shape` admits (decodeOERFixedFieldsAt), and it carries
// the RetCode anchor on top of the diagnostic proof — the tail must still spell
// the code its fields report, on both paths.
func decodeErrorOER(shape oerShape, payload []byte) *oerInfo {
	compressed, rest := decodeOERFieldsAt(payload, 0)
	if info := provenErrorOER(payload, compressed, rest); info != nil {
		return info
	}

	fixed, rest := decodeOERFixedFieldsAt(shape, payload, 0)

	return provenErrorOER(payload, fixed, rest)
}

// provenErrorOER applies decodeErrorOER's proof to one candidate decode: the
// code must be a real failure inside the range an Oracle code can occupy, and
// the tail at `rest` must carry a printable diagnostic naming that very code.
// Returns info with its message filled in, or nil.
func provenErrorOER(payload []byte, info *oerInfo, rest int) *oerInfo {
	if info == nil {
		return nil
	}

	if info.ErrorCode <= 0 || info.ErrorCode == oraNoDataFound || info.ErrorCode >= maxPlausibleORACode {
		return nil
	}

	msg := extractORAMessage(payload[rest:])
	if !looksLikeOracleDiagnostic(msg) || !diagnosticNamesCode(msg, info.ErrorCode) {
		return nil
	}

	info.ErrorMessage = msg

	return info
}

// diagnosticNamesCode reports whether msg opens with the diagnostic code the
// OER's fields reported — "ORA-00942" for 942, "ORA-06550" for 6550. Oracle
// zero-pads to five digits, and maxPlausibleORACode keeps the code inside that
// width.
func diagnosticNamesCode(msg string, code int) bool {
	want := fmt.Sprintf("%05d", code)

	for _, prefix := range oracleDiagnosticPrefixes {
		if strings.HasPrefix(msg, prefix+want) {
			return true
		}
	}

	return false
}

// oerMaxSeqNumber bounds a believable OER sequence number.
//
// It used to be 255, on the belief that TTC numbers calls with a single byte
// that wraps. It does not: this field is the end-to-end ECID sequence
// (go-ora's `SummaryObject.EndToEndECIDSequence`), a **uint16** that counts up
// across the whole session and rolls over at 65535, not at 255. Measured
// against Oracle Free 23ai, a session crosses 255 after a few dozen
// statements, and from that point every single OER was rejected by this bound
// — so `findCursorIDInResponse` stopped learning cursor ids for the rest of the
// session. See docs/oracle.md, "Cursor-id learning".
//
// The bound is therefore the field's real width. `oerFieldMaxSizes` already
// caps its encoding at two bytes, so this is belt and braces; it is kept
// spelled out because the value is what the anchor means, not an accident of
// the encoding.
const oerMaxSeqNumber = 0xFFFF

// findPlausibleOERInResponse scans a server payload for the OER that ends the
// call just executed, *without* requiring the end-of-call bit — the bit is not
// universal (python-oracledb thin sessions get CallStatus 1–2), so a locator
// that insists on it is blind to those clients entirely.
//
// The scan is anchored rather than trusting: the run must decode as seven
// compressed ints, the error code must be success or end-of-data (an OER
// reporting a real failure assigns no cursor), the sequence number must fit its
// 16-bit field, and the cursor id must be a plausible 16-bit id. First match
// wins, which is what keeps a later run of row bytes that happens to parse from
// overriding the genuine one.
//
// Every one of those bounds is load-bearing in both directions: too loose and a
// run of row bytes is mistaken for the OER, too tight and the genuine OER is
// skipped. The sequence-number bound was the second of those for a while — see
// oerMaxSeqNumber.
//
// Both callers share this one scan on purpose. Cursor-id learning has always
// used these bounds, and completion used to demand the bit on top of them —
// which meant dbbat read a python-oracledb OER well enough to learn cursor 4
// off it while refusing to believe the CurRowNumber sitting three fields
// earlier. Two locators meant two sets of bounds to keep honest; there is now
// one, and what separates the callers is *where* they are allowed to run it
// (see handleResponse), not how much they trust the same bytes.
// It reads both encodings, each under the same bounds: an OCI client marshals
// this very OER fixed-width, so a locator that only decodes compressed integers
// learns no cursor id at all on those sessions — which is what left the
// mid-fetch anchor with nothing to compare against there. The fixed-width
// reading is offered only for the layouts `shape` admits, and adds the RetCode
// anchor to the bounds below; see decodeOERFixedFieldsAt.
func findPlausibleOERInResponse(shape oerShape, payload []byte) *oerInfo {
	info, _ := locatePlausibleOER(shape, payload)

	return info
}

// locatePlausibleOER is findPlausibleOERInResponse's scan, reporting *where* the
// accepted candidate sat as well as what it decoded to. The offset is -1 when
// nothing was accepted.
//
// The offset is not decoration. Every acceptance this scan makes is at a
// non-zero offset by construction (the loop starts at 1), which is to say every
// one of them is a *scan hit* — a 0x04 found somewhere inside a payload — and
// never the packet's own leading function code. That is the distinction the
// measurement in cursor_learning_source_replay_test.go reports.
//
// The encoding check is symmetric with decodeOERFixedFieldsAt's own: a session
// that has learned the fixed-width encoding is never offered the compressed
// reading here, because a server speaks one encoding — the converse of the rule
// that keeps a compressed session's row bytes from being read as a fixed-width
// block. A compressed acceptance on a learned fixed-width session cannot be a
// server OER, and the corpus agrees: none of the 22 ids learned on fixed-width
// sessions is ever read as compressed. The locator is shared with
// handleResponse's out-of-stream fall-through, so the measurement covers
// completion as well as learning
// (TestDumpReplay_MidStreamOERFalsePositiveRate and the per-client completion
// tests are the guard).
func locatePlausibleOER(shape oerShape, payload []byte) (*oerInfo, int) {
	tryCompressed := !shape.tailLearned || !shape.fixedWidth

	for i := 1; i < len(payload); i++ {
		if payload[i] != 0x04 {
			continue
		}

		if tryCompressed {
			if info, _ := decodeOERFieldsAt(payload, i); plausibleStatusOER(info) {
				return info, i
			}
		}

		if info, _ := decodeOERFixedFieldsAt(shape, payload, i); plausibleStatusOER(info) {
			return info, i
		}
	}

	return nil, -1
}

// plausibleStatusOER is the bound set findPlausibleOERInResponse applies to one
// candidate decode, in whichever encoding it came from: the error code must be
// success or end-of-data (an OER reporting a real failure assigns no cursor),
// the sequence number must fit its 16-bit field, and the cursor id must be a
// plausible 16-bit id.
func plausibleStatusOER(info *oerInfo) bool {
	if info == nil {
		return false
	}

	if info.ErrorCode != 0 && info.ErrorCode != oraNoDataFound {
		return false
	}

	return info.SeqNumber <= oerMaxSeqNumber && info.CursorID > 0 && info.CursorID <= cursorReexecMaxID
}

// cursorIDSource ranks the evidence behind a learned cursor id, weakest first.
//
// The ranking exists because learning used to be a flat one-shot: the first
// value that passed plausibleStatusOER won and was never revisited. That is
// fine when the first value comes off the server's own end-of-call OER and
// catastrophic when it does not — measured live against a real 23ai server, a
// sqlplus fetch whose terminator correctly named cursor **2** ran on a session
// that had already latched **17744**.
//
// Where that 17744 came from is worth being exact about, because the obvious
// bound does not reach it. It was not scanned out of *row* bytes: it was scanned
// out of the QueryResult's **describe records**, on the packet that opens the
// fetch — and learnCursorID runs before handleQueryResultV2, so at that moment
// rowStreamActive() is still false. A blanket "do not learn while a row stream
// is open" would have left it exactly as it was. What reaches it is the other
// end of the ranking: the fetch's own terminator, at byte 0 of a later packet,
// is better evidence and is now allowed to say so.
//
// 17744 is inside cursorReexecMaxID and passes every bound the scan applies;
// nothing distinguishes it after the fact. It is also what
// rememberCursor files the statement under, so it is what a later re-execution
// naming a recycled id would be gated against — the wrong statement's text,
// silently, rather than the fail-closed refusal an *unknown* cursor gets.
//
// So the value is no longer latched, it is *ranked*: a stronger source replaces
// a weaker one, and a weaker one can never displace a stronger. Which keeps the
// property the one-shot rule was written for — row-stream bytes cannot churn the
// id for the rest of a fetch, because a scan hit never outranks a scan hit.
type cursorIDSource uint8

const (
	// cursorIDUnlearned is the zero value: no id has been read at all.
	cursorIDUnlearned cursorIDSource = iota

	// cursorIDFromMidStreamScan is a 0x04 the scan found *inside* a packet while
	// rows were on the wire. It is the weakest reading dbbat accepts, and the one
	// 17744 came from: mid-fetch, a payload is row data, so a run of bytes that
	// happens to decode as seven bounded ints is exactly what the corpus is full
	// of (see midStreamBitlessStatusAcceptances).
	//
	// It is still accepted, because refusing it outright is measurably wrong: of
	// the 176 ids the testdata corpus learns, one is learned this way and it is
	// genuine — a dbeaver/JDBC SELECT whose end-of-call OER arrives six packets
	// into its own row stream, naming the cursor the client then fetches by. See
	// TestDumpReplay_CursorIDLearningSource.
	cursorIDFromMidStreamScan

	// cursorIDFromDescribeScan is a scan hit on a packet that cannot carry the
	// call's own OER at all — above all the QueryResult (func=0x10) whose
	// payload is the server's describe records, which decode as seven bounded
	// ints just as happily as row data does. That is literally where the live
	// 17744 came from, and it is what learnCursorID sees first on a fetch, since
	// it runs before handleQueryResultV2.
	//
	// It is ranked below cursorIDFromScan rather than refused, for two measured
	// reasons. A client that re-executes between this packet and its terminator
	// would otherwise meet refuseUnknownCursor; and on the thin clients the same
	// packet carries the genuine OER bundled behind the describe records (see
	// oerResponseSeqUnderAByte), so the id it yields is usually the right one.
	// The rank says only that it is a guess about *where* the OER sits, not a
	// reading of the object that ends the call — so any later reading of any
	// other kind outranks it, which a flat scan rank could not express (a second
	// scan hit never outranks a scan hit).
	cursorIDFromDescribeScan

	// cursorIDFromScan is the same anchored scan run on a packet that *does*
	// carry the call's OER: the standalone OER message, the Response's embedded
	// one behind the return-parameter block, or the OVERSION answer an
	// execute-with-version piggyback is answered by. The reading that learns
	// most of the corpus's ids, and the one every thin client relies on.
	//
	// It is "not row data", not "trustworthy" — the 17744 measurement proved
	// that much — but its packet is at least one whose structure puts an OER
	// there, which is what the describe scan above cannot say.
	cursorIDFromScan

	// cursorIDFromCallBoundary is byte 0 of the packet under decodeOERAt's own
	// anchors: the end-of-call bit on the compressed encoding, or the RetCode
	// anchor plus plausibleStatusOER on the fixed-width status object an OCI call
	// ends with. It is the server naming the cursor in the object whose whole
	// purpose is to end the call, at the one offset the router already treats as
	// a function code rather than as data.
	//
	// The scan cannot reach it — findPlausibleOERInResponse starts at offset 1 —
	// which is why an OCI fetch's own terminator never got a say before this
	// ranking existed, and a mid-stream scan hit did.
	cursorIDFromCallBoundary
)

// String names the source for the debug record learnCursorID writes, which is
// how the live suites read the provenance back
// (TestIntegration_CursorIDLearningMissRate).
func (c cursorIDSource) String() string {
	switch c {
	case cursorIDFromCallBoundary:
		return "call_boundary"
	case cursorIDFromScan:
		return "scan"
	case cursorIDFromDescribeScan:
		return "describe_scan"
	case cursorIDFromMidStreamScan:
		return "mid_stream_scan"
	case cursorIDUnlearned:
		return "unlearned"
	default:
		return "unknown"
	}
}

// findCursorIDInResponse returns the cursor id the server assigned to the
// statement just executed.
//
// dbbat needs this because the modern execute paths (the v315+ piggyback exec
// and the JDBC thin exec) send the statement text with *no* cursor id — the
// server picks one and reports it back, and the client then re-runs the
// statement by that id alone. Without reading it here, a re-execution names a
// cursor dbbat has no statement for.
//
// `rowStream` is the session's own notion of whether rows are in flight
// (session.rowStreamActive); it does not change what is accepted, only how
// strongly the acceptance is rated. `funcCode` is the packet's TTC function
// code — payload[0] for everything the production callers pass — and it only
// feeds the rating too: a scan hit on a packet that structurally carries the
// call's OER (OERR, Response, OVERSION — all three measured) outranks one on a
// packet whose payload is data, the QueryResult's describe records above all.
// See cursorIDSource.
func findCursorIDInResponse(shape oerShape, payload []byte, rowStream bool, funcCode TTCFunctionCode) (uint16, cursorIDSource) {
	// Byte 0 first, and not merely as an optimisation: it is better evidence
	// than anything the scan can return, so a packet that carries both must be
	// read here. decodeOERAt is the router's own reading at that offset, and
	// plausibleStatusOER on top of it is the bound the scan applies — an OER
	// reporting a real failure assigns no cursor.
	//
	// No explicit gate on funcCode is needed: both of decodeOERAt's halves
	// demand a 0x04 at the offset, and payload[0] is the function code byte
	// itself — so a byte-0 acceptance is by construction a standalone OER's own
	// marker, never a describe record that happens to lead with one.
	if info := decodeOERAt(shape, payload, 0); plausibleStatusOER(info) {
		return uint16(info.CursorID), cursorIDFromCallBoundary
	}

	info, _ := locatePlausibleOER(shape, payload)
	if info == nil {
		return 0, cursorIDUnlearned
	}

	if rowStream {
		return uint16(info.CursorID), cursorIDFromMidStreamScan
	}

	// The carrier set is deliberately an allowlist of the three packet kinds
	// the corpus measured carrying the call's OER, so an *unknown* function
	// code defaults to the weaker rank — the same fail-safe direction the
	// mid-stream rank takes.
	//
	//nolint:exhaustive // only the packets measured to carry the call's OER rank as scan
	switch funcCode {
	case TTCFuncOERR, TTCFuncResponse, TTCFuncOVersion:
		return uint16(info.CursorID), cursorIDFromScan
	default:
		return uint16(info.CursorID), cursorIDFromDescribeScan
	}
}

// findOERInResponse scans a Response (func=0x08) payload for the embedded OER
// message that follows the return-parameter block. payload starts at the
// function code byte. Returns nil when no valid OER is found.
//
// It takes the session's summary-object shape because decodeOERAt does, but the
// scan starts at offset 1 and decodeOERAt's fixed-width half is offered at
// offset 0 only — so this locator is, by construction, the same end-of-call-bit
// reading it has always been. That is a **measured** refusal rather than an
// omission: an OCI fetch response carries a genuine fixed-width summary object
// at a constant offset inside every continuation packet, naming the streaming
// cursor and reporting the fetch's running row count, and a bit-less scan of the
// corpus's 649 mid-row-stream packets accepts 149 of them (2 even carry the bit).
// Mid-fetch, this scan runs on exactly those bytes, and the first acceptance
// would end the call at row 13001 of 20000 — silently truncating capture,
// stopping quota enforcement mid-stream and mis-charging the rest of the fetch,
// which is the production incident handleResponse's row-stream guard descends
// from. Outside a row stream nothing is lost by refusing: the
// findPlausibleOERInResponse fall-through right behind it reads the fixed-width
// encoding under the cursor bounds. See decodeFixedStatusOERAt and
// docs/oracle.md, "the OER end-of-call bit is not universal".
func findOERInResponse(shape oerShape, payload []byte) *oerInfo {
	for i := 1; i < len(payload); i++ {
		if payload[i] != 0x04 {
			continue
		}

		if info := decodeOERAt(shape, payload, i); info != nil {
			return info
		}
	}

	return nil
}

// extractORAMessage finds the "ORA-..." error text in the remaining OER
// payload (skipping the binary fields between the error code and the
// length-prefixed message). Truncates at the first non-printable byte.
func extractORAMessage(data []byte) string {
	idx := findBytes(data, []byte("ORA-"))
	if idx < 0 {
		return ""
	}

	end := idx
	for end < len(data) && data[end] >= 0x20 && data[end] <= 0x7e {
		end++
	}

	return strings.TrimSpace(string(data[idx:end]))
}
