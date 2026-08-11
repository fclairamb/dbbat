package oracle

import "bytes"

// This file is the writing half of ttc_oer.go: it builds the OER (message type
// 0x04) an Oracle server sends to end a call, so a statement dbbat refuses
// comes back to the client as an ORA error instead of hanging.
//
// The body is the TTC "summary object". Every integer in it is a TTC
// *compressed* integer — a length byte followed by that many big-endian value
// bytes, so zero is the single byte 0x00 — except for a handful of raw
// single-byte fields. That distinction is what makes this encoder tractable:
// dbbat writes zero into every field it has nothing to say about, and a
// compressed zero and a raw zero are the same byte on the wire. So the fields
// whose width is conditioned on the negotiated TTC version cost nothing to get
// "wrong" as long as they are zero, and only the fields dbbat actually fills in
// (the call status, the error code and the message) have to be placed exactly.
//
// The layout below was read off the wire from Oracle 23ai Free and cross-checked
// against the two client parsers dbbat is verified against:
//
//   - go-ora v3 `network.NewSummary` (network/summary_object.go)
//   - python-oracledb thin `_process_error_info`
//     (src/oracledb/impl/thin/messages/base.pyx)
//
// They agree field for field up to the trailing wide RetCode/row-count pair.
// Past that they do not, and that is the one variation this encoder cannot
// derive from first principles — see oerShape.extraTailFields.

// oerShape describes the negotiated variations in the summary object's layout.
//
// The first three are read straight off the capability exchange dbbat relays
// (see observeOERCapabilities). The fourth is *learned from the upstream's own
// OERs*, because no rule dbbat could find predicts it: measured against one
// Oracle 23ai Free instance, go-ora v3 and python-oracledb thin both negotiate
// CompileTimeCaps[7] = 24, and the server nonetheless sends python-oracledb two
// extra fields (a SQL type and a server checksum, the "fields added in Oracle
// Database 20c" python-oracledb's parser reads) and go-ora none. Patching each
// differing capability byte of one client's array to the other's did not move
// the boundary, so dbbat does not guess: it copies what the upstream — which
// negotiated with this very client's forwarded capabilities — actually emits.
type oerShape struct {
	// hasEndOfCallStatus is ServerCompileTimeCaps[15]&1. When clear, the
	// summary opens at the ECID sequence instead of the call status.
	hasEndOfCallStatus bool

	// hasECIDSequence is ServerCompileTimeCaps[16]&1 (and TTC version >= 3).
	hasECIDSequence bool

	// ttcVersion is min(client, server) CompileTimeCaps[7]. Only the >= 7
	// boundary changes the encoding here: below it the batch-error blocks and
	// the wide RetCode/row-count pair are replaced by three bare length fields.
	ttcVersion int

	// extraTailFields is how many additional compressed integers sit between
	// the wide RetCode/row-count pair and the error message. 0 for go-ora, 2
	// for python-oracledb thin. Learned; see learnOERShape.
	extraTailFields int

	// tailLearned records that extraTailFields came from a real upstream OER
	// rather than the default.
	tailLearned bool
}

// defaultOERShape is what dbbat assumes before it has seen anything: the shape
// every server measured (19c and 23ai) advertises, with no extra tail fields.
// A wrong tail costs the *message text*, never the end of the call — a client
// that expects no extra fields reads dbbat's zero-valued ones as an empty CLR
// and still returns the ORA code — so 0 is also the safe default.
func defaultOERShape() oerShape {
	return oerShape{
		hasEndOfCallStatus: true,
		hasECIDSequence:    true,
		ttcVersion:         oerModernTTCVersion,
	}
}

// oerModernTTCVersion is the TTC field version assumed until the capability
// exchange says otherwise. Any value >= 7 selects the same encoding; 12 is what
// a 19c server advertises, the lowest of the servers dbbat is tested against.
const oerModernTTCVersion = 12

// oerMaxExtraTailFields bounds the tail-field search in learnOERShape. Two is
// what Oracle 23ai sends python-oracledb; the bound leaves room for a future
// pair without letting a misparse run away.
const oerMaxExtraTailFields = 4

// oerSummary is the subset of the summary object dbbat fills in. Everything
// else is written as zero.
type oerSummary struct {
	// CallStatus is the end-of-call status word. encodeOER always sets
	// oerEndOfCallBit in it — this package's own decoder keys completion off
	// that bit, and it is what a server sends once dbbat has stripped the 23ai
	// end-of-response marker from the Accept.
	CallStatus int

	// SeqNumber is the end-to-end ECID sequence.
	SeqNumber int

	// CursorID is the cursor the error is attributed to, 0 when none.
	CursorID int

	// ErrorCode is the ORA-NNNNN code; 0 would make this a success status.
	ErrorCode int

	// ErrorMessage is the full text, "ORA-01031: ..." included. Written as a
	// CLR only when ErrorCode is non-zero, exactly as both client parsers read
	// it.
	ErrorMessage string
}

// encodeOER builds a complete TTC OER message: the 0x04 marker followed by the
// summary object. The result is a TTC message body — the caller frames it in a
// TNS Data packet behind the two data-flag bytes.
func encodeOER(shape oerShape, sum oerSummary) []byte {
	buf := make([]byte, 0, 64+len(sum.ErrorMessage))
	buf = append(buf, byte(TTCFuncOERR))

	putInt := func(n int) { buf = append(buf, ttcCompressedUint(uint64(n))...) }
	putZero := func(count int) {
		for range count {
			buf = append(buf, 0x00)
		}
	}

	if shape.hasEndOfCallStatus {
		putInt(sum.CallStatus | oerEndOfCallBit)
	}

	if shape.hasECIDSequence && shape.ttcVersion >= 3 {
		putInt(sum.SeqNumber)
	}

	putZero(1)              // curRowNumber — nothing ran
	putInt(sum.ErrorCode)   // errNum
	putZero(2)              // arrayElemWithError, arrayElemErrno
	putInt(sum.CursorID)    // cursorID
	putZero(1)              // errorPos
	putZero(2)              // sqlType, oerFatal (raw bytes)
	putZero(2)              // flags, userCursorOPT
	putZero(2)              // upiParam, warningFlag (raw bytes)
	putZero(oerRowIDFields) // rba, partitionID, tableID, blockNumber, slotNumber
	putZero(1)              // osError
	putZero(2)              // stmtNumber, callNumber (raw bytes)
	putZero(2)              // padding, successIterations
	putZero(1)              // oerrdd (logical rowid), a zero-length DLC

	if shape.ttcVersion < 7 {
		// Pre-12c framing: three bare DLC lengths where the batch-error blocks
		// and the wide pair sit on a modern connection, and the message follows
		// straight after.
		putZero(3)

		if sum.ErrorCode != 0 {
			buf = append(buf, ttcClr([]byte(sum.ErrorMessage))...)
		}

		return buf
	}

	putZero(3) // batch error codes / offsets / messages — all empty

	putInt(sum.ErrorCode) // RetCode, repeated at full width
	putZero(1)            // row count, repeated at full width
	putZero(shape.extraTailFields)

	if sum.ErrorCode != 0 {
		buf = append(buf, ttcClr([]byte(sum.ErrorMessage))...)
	}

	return buf
}

// oerRowIDFields is the field count of the rowid the summary carries: rba,
// partition id, table id, block number, slot number.
const oerRowIDFields = 5

// learnOERShape refines shape.extraTailFields from a real OER the upstream
// sent. payload is a TTC message body (it starts at the function-code byte) and
// may be a standalone OER or a message with one embedded; ok reports whether
// anything was learned.
//
// The upstream leg negotiated with the *client's* own forwarded capabilities,
// so an OER it sends is by definition shaped the way this client parses. That
// is the whole reason this is learned rather than derived.
//
// Two discriminators, in order of strength:
//
//   - an OER that carries an error: the tail is followed by a CLR whose text
//     starts with "ORA-". Only one field count puts the length byte there.
//   - an OER that ends the payload: the leftover byte count is the number of
//     extra (zero-valued, hence single-byte) fields.
//
// A success OER that does *not* end the payload teaches nothing and is skipped.
//
// The scan keeps the LAST offset that decodes rather than the first: a server
// response is row data, return parameters or AUTH key/value pairs followed by
// the OER, so the genuine one is at the end and any coincidental match is
// before it.
func learnOERShape(shape *oerShape, payload []byte) bool {
	extra, found := 0, false

	for i := range payload {
		if payload[i] != byte(TTCFuncOERR) {
			continue
		}

		if n, ok := oerTailFieldsAt(*shape, payload, i); ok {
			extra, found = n, true
		}
	}

	if !found {
		return false
	}

	shape.extraTailFields = extra
	shape.tailLearned = true

	return true
}

// oerTailFieldsAt decodes the OER whose marker sits at payload[offset] with the
// given shape and returns how many extra compressed integers separate the wide
// RetCode/row-count pair from the message (or the end of the payload).
func oerTailFieldsAt(shape oerShape, payload []byte, offset int) (int, bool) {
	if shape.ttcVersion < 7 {
		return 0, false
	}

	pos, errCode, ok := skipOERFixedFields(shape, payload, offset)
	if !ok {
		return 0, false
	}

	for extra := 0; extra <= oerMaxExtraTailFields; extra++ {
		if pos > len(payload) {
			return 0, false
		}

		if errCode == 0 {
			if pos == len(payload) {
				return extra, true
			}
		} else if msg, n := readCLR(payload[pos:]); n > 0 && bytes.HasPrefix(msg, []byte("ORA-")) {
			return extra, true
		}

		// Not the message (or not the end) yet — step over one more candidate
		// field. They are compressed integers like everything else here, so a
		// non-zero one is wider than a byte: Oracle 23ai sends python-oracledb a
		// two-byte SQL type followed by a one-byte zero checksum.
		_, n := readCompressedInt(payload[pos:])
		if n == 0 || n-1 > oerMaxTailFieldWidth {
			return 0, false
		}

		pos += n
	}

	return 0, false
}

// oerMaxTailFieldWidth bounds a trailing field's value width. Both fields
// observed there are 4-byte integers.
const oerMaxTailFieldWidth = 4

// skipOERFixedFields walks the version-independent middle of a summary object —
// everything from the marker through the wide RetCode/row-count pair — and
// returns the offset just past it plus the error code it read. ok=false when
// the bytes do not decode as a summary at all.
//
// The walk mirrors both client parsers field for field, raw single bytes
// included: `callNumber` and `sqlType` are routinely non-zero on the wire, so
// reading either as a compressed integer would swallow the bytes that follow.
// The wide RetCode must repeat the leading one, which is the check that makes a
// stray 0x04 inside row data fail rather than yield a plausible tail count.
func skipOERFixedFields(shape oerShape, payload []byte, offset int) (int, int, bool) {
	if offset >= len(payload) || payload[offset] != byte(TTCFuncOERR) {
		return 0, 0, false
	}

	w := oerWalker{payload: payload, pos: offset + 1, ok: true}

	if shape.hasEndOfCallStatus {
		w.readInt(4)
	}

	if shape.hasECIDSequence && shape.ttcVersion >= 3 {
		w.readInt(2)
	}

	w.readInt(8)             // curRowNumber
	errCode := w.readInt(4)  // errNum
	w.readInt(2)             // arrayElemWithError
	w.readInt(2)             // arrayElemErrno
	w.readInt(2)             // cursorID
	w.readInt(2)             // errorPos
	w.skipBytes(2)           // sqlType, oerFatal
	w.readInt(2)             // flags
	w.readInt(2)             // userCursorOPT
	w.skipBytes(2)           // upiParam, warningFlag
	w.readInt(4)             // rba
	w.readInt(2)             // partitionID
	w.skipBytes(1)           // tableID
	w.readInt(4)             // blockNumber
	w.readInt(2)             // slotNumber
	w.readInt(4)             // osError
	w.skipBytes(2)           // stmtNumber, callNumber
	w.readInt(2)             // padding
	w.readInt(4)             // successIterations
	oerrdd := w.readInt(4)   // logical rowid length
	codes := w.readInt(2)    // batch error codes
	offsets := w.readInt(4)  // batch error row offsets
	messages := w.readInt(2) // batch error messages

	// dbbat only learns from a plain OER. One carrying a logical rowid or batch
	// errors has variable-length blocks here that would have to be walked to
	// reach the tail, and it teaches nothing the next plain one will not.
	if !w.ok || oerrdd != 0 || codes != 0 || offsets != 0 || messages != 0 {
		return 0, 0, false
	}

	wideErr := w.readInt(4)
	w.readInt(8) // wide row count

	if !w.ok || wideErr != errCode {
		return 0, 0, false
	}

	return w.pos, errCode, true
}

// oerWalker is a cursor over a summary object that latches a decode failure,
// so the field sequence above reads as the layout it is rather than as error
// handling.
type oerWalker struct {
	payload []byte
	pos     int
	ok      bool
}

// readInt consumes one TTC compressed integer whose value is at most maxSize
// bytes wide. A wider encoding means the walk has lost the layout.
func (w *oerWalker) readInt(maxSize int) int {
	if !w.ok || w.pos > len(w.payload) {
		w.ok = false

		return 0
	}

	val, n := readCompressedInt(w.payload[w.pos:])
	if n == 0 || n-1 > maxSize {
		w.ok = false

		return 0
	}

	w.pos += n

	return val
}

// skipBytes consumes raw single-byte fields.
func (w *oerWalker) skipBytes(count int) {
	if !w.ok || w.pos+count > len(w.payload) {
		w.ok = false

		return
	}

	w.pos += count
}

// observeOERCapabilities reads the OER-shaping capabilities off a Set Protocol
// (OSETPRO) reply's ServerCompileTimeCaps and records them on the shape.
// Returns whether the packet was one and the caps could be read.
//
// raw is a whole TNS packet. The capability array is located the way go-ora
// locates it — by parsing the reply's preamble — rather than by scanning for a
// byte pattern, because the two flags read here sit deep enough in the array
// that an off-by-four anchor would silently read a neighbouring capability.
func observeOERCapabilities(shape *oerShape, raw []byte) bool {
	caps, ok := serverCompileTimeCaps(raw)
	if !ok {
		return false
	}

	if len(caps) > oerCapECIDIndex {
		shape.hasEndOfCallStatus = caps[oerCapEndOfCallIndex]&1 != 0
		shape.hasECIDSequence = caps[oerCapECIDIndex]&1 != 0
	}

	if len(caps) > oerCapFieldVersionIndex && caps[oerCapFieldVersionIndex] > 0 {
		shape.ttcVersion = min(shape.ttcVersion, int(caps[oerCapFieldVersionIndex]))
	}

	return true
}

// observeClientTTCVersion lowers the shape's TTC version to the client's own
// CompileTimeCaps[7], read off a Set Data Types (ODTYPES) request. The
// negotiated version is the minimum of both ends'.
//
// ttcBody is a TTC message body — it starts at the function-code byte. A
// client that pipelines its login wraps ODTYPES inside a fast-auth bundle and
// is not read here; the server's own version then stands, which is the
// conservative direction — it can only be higher than what was negotiated, and
// the only boundary that matters is >= 7.
func observeClientTTCVersion(shape *oerShape, ttcBody []byte) bool {
	if len(ttcBody) < 1 || TTCFunctionCode(ttcBody[0]) != TTCFuncSetDataTypes {
		return false
	}

	// [charset:2][charset:2][serverFlags:1][capsLen:1][caps...]
	body := ttcBody[1:]

	const capsLenOffset = 5

	if len(body) <= capsLenOffset {
		return false
	}

	capsLen := int(body[capsLenOffset])
	if capsLen <= oerCapFieldVersionIndex || len(body) < capsLenOffset+1+capsLen {
		return false
	}

	caps := body[capsLenOffset+1 : capsLenOffset+1+capsLen]
	if caps[oerCapFieldVersionIndex] == 0 {
		return false
	}

	shape.ttcVersion = min(shape.ttcVersion, int(caps[oerCapFieldVersionIndex]))

	return true
}

// Capability-array indices, named after what they gate in the summary object.
// These are indices into the array as Oracle frames it — the one that *starts*
// with the 06 01 01 01 prefix, not four bytes past it.
const (
	oerCapFieldVersionIndex = 7  // TTC field version (min of both ends wins)
	oerCapEndOfCallIndex    = 15 // bit 0: the summary opens with a call status
	oerCapECIDIndex         = 16 // bit 0: the summary carries an ECID sequence
)

// serverCompileTimeCaps extracts ServerCompileTimeCaps from a Set Protocol
// (OSETPRO) reply packet, parsing the preamble that precedes it:
//
//	[func 0x01][version:1][pad:1][server string, NUL-terminated]
//	[charset:2 LE][serverFlags:1][charsetElems:2 LE][charsetElems*5]
//	[len:2 BE][numArray][capsLen:1][caps...]
func serverCompileTimeCaps(raw []byte) ([]byte, bool) {
	if len(raw) < tnsHeaderSize+ttcDataFlagsSize+1 ||
		TNSPacketType(raw[4]) != TNSPacketTypeData {
		return nil, false
	}

	body := raw[tnsHeaderSize:]
	if TTCFunctionCode(body[ttcDataFlagsSize]) != TTCFuncSetProtocol {
		return nil, false
	}

	p := body[ttcDataFlagsSize+1:]

	pos := 2 // protocol version + pad

	nul := bytes.IndexByte(p[min(pos, len(p)):], 0x00)
	if nul < 0 {
		return nil, false
	}

	pos += nul + 1
	pos += 2 // server charset
	pos++    // server flags

	if pos+2 > len(p) {
		return nil, false
	}

	charsetElems := int(p[pos]) | int(p[pos+1])<<8
	pos += 2 + charsetElems*5

	if pos+2 > len(p) {
		return nil, false
	}

	numArrayLen := int(p[pos])<<8 | int(p[pos+1])
	pos += 2 + numArrayLen

	if pos >= len(p) {
		return nil, false
	}

	capsLen := int(p[pos])
	pos++

	if capsLen == 0 || pos+capsLen > len(p) {
		return nil, false
	}

	return p[pos : pos+capsLen], true
}
