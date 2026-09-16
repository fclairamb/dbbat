package oracle

import (
	"bytes"
	"encoding/binary"
)

// Rewriting a statement on its way upstream.
//
// Every other file in this package reads. This one writes, and it is the first
// thing in the data phase that does — `reassembly.go` states the invariant it
// breaks: *"the reassembled buffer is for reading only, and dbbat never
// synthesizes wire bytes toward the upstream."* That invariant held because
// nothing needed to change a statement. The per-user query tag
// (`shared.NewUserQueryTagger`, `DBB_QUERY_TAGGING_ORACLE=user`) does.
//
// The whole design is one idea: **locate exactly, or refuse**. The decoders next
// door (`decodeExecStatementText`, `locateExecSQLText`) *search* for a text run
// of roughly the declared length, accept `sqlLen` or `sqlLen-1`, tolerate a
// one-byte shift and validate with "looks like SQL". That is right for a gate,
// which fails open to a scan when it is unsure. It is not a basis for
// overwriting a length prefix: the documented failure mode for getting a TTC
// length field wrong, already met on the AUTH leg, is `ORA-03146 invalid buffer
// length for TTC field` — a dead session, in exchange for a comment.
//
// So `locateStatementRewrite` answers only when it can name, to the byte:
//
//   - the SQL-length field's offset, its encoded width and which of the three
//     encodings it uses;
//   - the span holding the statement value, with whatever CLR framing wraps it;
//   - the statement bytes themselves.
//
// and only when **re-encoding what it read reproduces the client's own bytes
// exactly** (`stmtRewrite.verify`). That round-trip is the certainty: a model of
// the frame that cannot reproduce the frame is a model that must not be used to
// change it. It runs on every frame, not just on the first, and costs one
// comparison of a few hundred bytes.
//
// Three shapes are covered, all of them measured against the recordings in
// testdata/ (see ttc_statement_rewrite_survey_test.go):
//
//   - **thin exec** (`03 5e`, and the same op stapled behind a `11 69` close
//     list): the length is a TTC compressed int whose *encoded width grows with
//     the value*, so widening it shifts every byte behind it. go-ora and
//     python-oracledb thin then repeat the length as a CLR byte immediately
//     before the text; ojdbc and DBeaver write the run bare, with the header
//     field as its only length. Both are handled, and told apart by what is
//     actually in front of the run.
//   - **OCI wide exec** (sqlplus, SQL*Developer, Instant Client): the length is
//     a little-endian ub4 holding `sqlLen * 3` behind the `fe x8` pointer
//     sentinel, and the CLR body carries the trailing NUL the client counts.
//     The NUL rides along at the end of the run and needs no special case: the
//     rewriter prepends to the *value*, not to the text.
//   - **OALL8** (`0x0e`, legacy pre-v315): `decodeVarLen` (1 byte / `0xFE`+2BE /
//     `0xFF`+4BE) with the text immediately behind it and the bind count
//     immediately behind that. No recording carries one, so it is covered by
//     synthetic tests only — see the survey.
//
// The 252-byte CLR format change is the one that bites, because a ~45-byte tag
// is exactly what pushes a statement across it. A value at or past the limit is
// written in the `0xFE`-chunked long form, in the variant the session
// negotiated (`s.clientBigClrChunks`); a value already chunked is re-chunked
// with the client's own observed chunk size and chunk-length encoding, which is
// what the round-trip check pins.

// stmtLenKind names the three encodings a statement-carrying TTC op uses for its
// SQL-length field.
type stmtLenKind int

const (
	// stmtLenCompressed is the TTC compressed integer: a one-byte count then
	// that many big-endian bytes. Its width changes with the value.
	stmtLenCompressed stmtLenKind = iota
	// stmtLenWideUB4 is the OCI header's `sqlLen * 3` little-endian ub4. Fixed
	// width, so a rewrite never shifts anything behind it.
	stmtLenWideUB4
	// stmtLenVarLen is OALL8's decodeVarLen encoding. Its width changes with
	// the value too.
	stmtLenVarLen
)

// stmtClrKind names how the statement value is framed inside the message.
type stmtClrKind int

const (
	// stmtClrNone is a bare run: the header's length field is the only thing
	// that says how long it is. ojdbc and DBeaver write this shape.
	stmtClrNone stmtClrKind = iota
	// stmtClrShort is one length byte immediately in front of the run —
	// go-ora, python-oracledb thin and the OCI clients.
	stmtClrShort
	// stmtClrChunked is the CLR long form: 0xFE, then length-prefixed chunks,
	// then a zero-length terminator.
	stmtClrChunked
)

// clrShortMaxLen is the largest value this package will write in the CLR short
// form. Oracle's own limit is 252 (0xFC), but 0xFC..0xFF are the bytes the long
// form and the null/undefined markers use, so staying a byte under it keeps the
// prefix unambiguous to every reader. A value the client wrote as 0xFC fails the
// round-trip check and the frame is left alone, which is the fail-closed
// direction.
const clrShortMaxLen = 0xFB

// stmtRewrite is everything needed to put a different statement in a TTC
// message, and nothing else. Offsets are into the TTC body it was located in.
type stmtRewrite struct {
	// lenAt/lenWidth/lenKind locate and describe the SQL-length field.
	lenAt    int
	lenWidth int
	lenKind  stmtLenKind

	// valueAt/valueEnd span the statement value *with* its framing: the CLR
	// prefix byte for the short form, the 0xFE..0x00 run for the long one, and
	// exactly the run itself when there is no framing at all.
	valueAt  int
	valueEnd int
	clrKind  stmtClrKind

	// chunkSize and chunkBig record the client's own long-form convention, so a
	// rewritten value is re-chunked the way this client chunks rather than the
	// way this package happens to. Meaningful for stmtClrChunked only.
	chunkSize int
	chunkBig  bool

	// run is the statement value itself — the CLR body, which for an OCI client
	// includes the trailing NUL. It is *not* statement text and is never stored:
	// see text().
	run []byte
}

// text is the statement as the gate reads it: the run without the trailing NUL
// an OCI client counts in its declared length.
func (r stmtRewrite) text() string {
	if n := len(r.run); n > 0 && r.run[n-1] == 0 {
		return string(r.run[:n-1])
	}

	return string(r.run)
}

// encodeLen writes n into this field's encoding.
func (r stmtRewrite) encodeLen(n int) []byte {
	switch r.lenKind {
	case stmtLenWideUB4:
		out := make([]byte, 4)
		binary.LittleEndian.PutUint32(out, uint32(n*wideCharWidth))

		return out

	case stmtLenVarLen:
		return encodeVarLenBytes(n)

	case stmtLenCompressed:
		return ttcCompressedUint(uint64(n))

	default:
		return nil
	}
}

// encodeValue writes run back in this message's framing. bigChunks is the
// session's negotiated CLR long form, consulted only when a value that was short
// has to become long — which is exactly what a tag does to a statement sitting
// just under the limit.
func (r stmtRewrite) encodeValue(run []byte, bigChunks bool) []byte {
	switch r.clrKind {
	case stmtClrNone:
		return run

	case stmtClrChunked:
		return encodeChunkedCLR(run, r.chunkSize, r.chunkBig)

	case stmtClrShort:
		if len(run) <= clrShortMaxLen {
			out := make([]byte, 0, 1+len(run))
			out = append(out, byte(len(run)))

			return append(out, run...)
		}

		// Crossing the format boundary. The client never showed dbbat its long
		// form (this value was short), so the convention comes from the session:
		// ttcClrVariant is the same encoder the AUTH leg already writes to this
		// upstream, in the variant the upstream advertised.
		return ttcClrVariant(run, bigChunks)

	default:
		return nil
	}
}

// verify reports whether re-encoding what was located reproduces the client's
// own bytes. Everything else in this file rests on it: a frame whose model
// cannot reproduce the frame is a frame nothing may rewrite.
func (r stmtRewrite) verify(body []byte, bigChunks bool) bool {
	if r.lenAt < 0 || r.lenWidth <= 0 || r.lenAt+r.lenWidth > len(body) {
		return false
	}

	if r.valueAt < r.lenAt+r.lenWidth || r.valueEnd > len(body) || r.valueAt >= r.valueEnd {
		return false
	}

	if !bytes.Equal(r.encodeLen(len(r.run)), body[r.lenAt:r.lenAt+r.lenWidth]) {
		return false
	}

	return bytes.Equal(r.encodeValue(r.run, bigChunks), body[r.valueAt:r.valueEnd])
}

// apply returns body with run replaced by newRun: the length field re-encoded
// (which may change its width, shifting everything behind it), the value
// re-framed, and every other byte carried over untouched.
func (r stmtRewrite) apply(body []byte, newRun []byte, bigChunks bool) []byte {
	lenField := r.encodeLen(len(newRun))
	value := r.encodeValue(newRun, bigChunks)

	out := make([]byte, 0, len(body)+len(value)-(r.valueEnd-r.valueAt)+len(lenField)-r.lenWidth)
	out = append(out, body[:r.lenAt]...)
	out = append(out, lenField...)
	out = append(out, body[r.lenAt+r.lenWidth:r.valueAt]...)
	out = append(out, value...)
	out = append(out, body[r.valueEnd:]...)

	return out
}

// encodeVarLen is decodeVarLen's inverse: one byte under 0xFE, else 0xFE plus a
// big-endian uint16, else 0xFF plus a big-endian uint32.
func encodeVarLenBytes(n int) []byte {
	switch {
	case n < oall8LenShort:
		return []byte{byte(n)}
	case n <= 0xFFFF:
		out := []byte{oall8LenShort, 0, 0}
		binary.BigEndian.PutUint16(out[1:], uint16(n))

		return out
	default:
		out := []byte{oall8LenLong, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(out[1:], uint32(n))

		return out
	}
}

// encodeChunkedCLR writes the CLR long form with a given chunk payload size and
// chunk-length encoding. It is encodeBigChunkCLRSplit generalized to the
// single-byte-length variant, so a value can be re-chunked the way the client
// that sent it chunks.
func encodeChunkedCLR(data []byte, chunkSize int, bigChunks bool) []byte {
	if chunkSize <= 0 {
		return nil
	}

	if bigChunks {
		return encodeBigChunkCLRSplit(data, chunkSize)
	}

	if chunkSize > ttcClrChunkSize {
		// ttcClrChunkSize (252) is both what every client emits and the largest
		// chunk a single-byte length carries without running into the marker
		// bytes. A larger one is not something any client asks for, and
		// answering nil here is what makes the round-trip check refuse the frame
		// rather than write a length nothing can read.
		return nil
	}

	out := make([]byte, 0, len(data)+16)
	out = append(out, 0xFE)

	for i := 0; i < len(data); i += chunkSize {
		end := min(i+chunkSize, len(data))

		out = append(out, byte(end-i))
		out = append(out, data[i:end]...)
	}

	out = append(out, 0x00)

	return out
}

// --- locating ---------------------------------------------------------------

// locateStatementRewrite finds the statement in a client TTC message and
// returns the exact plan for putting a different one there, or reports that it
// cannot.
//
// bigChunks is the session's negotiated CLR long form; it only takes part in the
// round-trip check, never in the search.
func locateStatementRewrite(ttcPayload []byte, bigChunks bool) (stmtRewrite, bool) {
	if rw, ok := locateExecRewriteAt(ttcPayload, 0, bigChunks); ok {
		return rw, true
	}

	// The JDBC/DBeaver shape: a close-cursors list with the execute stapled
	// behind it, which is the same op at a non-zero offset.
	if end, ok := closeCursorsEnd(ttcPayload); ok && end < len(ttcPayload) {
		if rw, ok := locateExecRewriteAt(ttcPayload, end, bigChunks); ok {
			return rw, true
		}
	}

	// OALL8 is deliberately not rewritten. See oall8RewriteEnabled.
	if oall8RewriteEnabled {
		return locateOALL8Rewrite(ttcPayload, bigChunks)
	}

	return stmtRewrite{}, false
}

// oall8RewriteEnabled gates the legacy OALL8 rewrite, and is false.
//
// Turning it off costs no coverage at all: **no recording in testdata/ carries
// an OALL8 statement**, no client the e2e suite drives sends one, and the survey
// counts zero of them. What it removes is the least-defended write path of the
// three, which is not a description of how the code below is written but of what
// the op's own layout allows:
//
//   - it does not go through locateStatementValue, so it gets none of that
//     function's guards — no uniqueness requirement, no boundedness check at the
//     run's far end, and in particular no valuePrecededByAnotherLength, the
//     check that exists because a stale second copy of the declared length is
//     exactly the bug a real 23ai already caught once (ORA-03120, see
//     locateStatementValue);
//   - its clrKind is stmtClrNone, so verify's value half compares the run
//     against itself — true by construction, not a check. Only the length half
//     is doing work, which is half the certainty every other shape gets;
//   - the offsets come from decodeOALL8's walk, which this package's own comment
//     calls "a simplified decoding that handles the most common cases" and which
//     has never been checked against a real OALL8 capture, because there is
//     none.
//
// So the encoder below is a specification rather than a shipped path: the
// writers are unit-tested against synthetic frames (TestRewriteOALL8VarLenWidens)
// and stay that way until a recording of a real OALL8 client lands in testdata/.
// See specs/todos/2026-09-16-11-oracle-tag-oall8-rewrite.md.
const oall8RewriteEnabled = false

// locateExecRewriteAt locates the statement of an exec op starting at base
// inside ttcPayload, returning offsets relative to ttcPayload.
func locateExecRewriteAt(ttcPayload []byte, base int, bigChunks bool) (stmtRewrite, bool) {
	body := ttcPayload[base:]

	if !isPiggybackExecHeader(body) || len(body) < execHeaderMinLen {
		return stmtRewrite{}, false
	}

	field, ok := execSQLLengthField(body)
	if !ok {
		return stmtRewrite{}, false
	}

	rw, ok := locateStatementValue(body, field)
	if !ok {
		return stmtRewrite{}, false
	}

	if !rw.verify(body, bigChunks) {
		return stmtRewrite{}, false
	}

	rw.lenAt += base
	rw.valueAt += base
	rw.valueEnd += base

	return rw, true
}

// locateOALL8Rewrite locates the statement of a legacy OALL8 parse+execute.
//
// The layout leaves nothing to search for: the text sits immediately behind the
// length field with no CLR framing, and the bind count sits immediately behind
// the text — which is also why the length field's *width* matters here as much
// as its value, since widening it shifts the binds.
func locateOALL8Rewrite(ttcPayload []byte, bigChunks bool) (stmtRewrite, bool) {
	if len(ttcPayload) < oall8MinPayloadSize || TTCFunctionCode(ttcPayload[0]) != TTCFuncOALL8 {
		return stmtRewrite{}, false
	}

	// func(1) + options(4) + cursor(2), exactly as decodeOALL8 walks it.
	const lenAt = 7

	sqlLen, width, err := decodeVarLen(ttcPayload[lenAt:])
	if err != nil || sqlLen == 0 || int(sqlLen) > execMaxSQLLen {
		return stmtRewrite{}, false
	}

	runAt := lenAt + width
	if runAt+int(sqlLen) > len(ttcPayload) {
		return stmtRewrite{}, false
	}

	if !isStatementRun(ttcPayload, runAt, int(sqlLen)) {
		return stmtRewrite{}, false
	}

	rw := stmtRewrite{
		lenAt:    lenAt,
		lenWidth: width,
		lenKind:  stmtLenVarLen,
		valueAt:  runAt,
		valueEnd: runAt + int(sqlLen),
		clrKind:  stmtClrNone,
		run:      ttcPayload[runAt : runAt+int(sqlLen)],
	}

	if !rw.verify(ttcPayload, bigChunks) {
		return stmtRewrite{}, false
	}

	return rw, true
}

// locateStatementValue finds the one place in an exec body where a statement of
// the declared length can be, and refuses when there is not exactly one.
//
// Uniqueness is the point. A run that merely *could* be the statement is what
// the gate's scan settles for; here a second candidate means dbbat does not know
// which bytes the server will parse, and writing into the wrong one is the
// ORA-03146 this whole file is arranged to avoid.
func locateStatementValue(body []byte, field execSQLLenField) (stmtRewrite, bool) {
	// The CLR long form is tried first, and that ordering is load-bearing rather
	// than a preference. A client whose chunk size is larger than the statement
	// writes it as *one* chunk, so its text sits contiguously in the payload and
	// the scan below finds it — as a bare run, with the chunk's own length
	// prefix left outside the span the rewriter touches. Rewriting that leaves a
	// chunk header still declaring the old length in front of a longer
	// statement, which a real 23ai answers with `ORA-03120: two-task conversion
	// routine: integer overflow` (measured, 2026-09-16, on a 20KB go-ora
	// statement). Reading the framing first is what stops the contiguous scan
	// from ever seeing that frame.
	if rw, ok := locateChunkedStatementValue(body, field); ok {
		return rw, true
	}

	var (
		found stmtRewrite
		hits  int
	)

	// The statement always sits behind the field that declares it.
	from := field.at + field.width

	for i := from; i+field.value <= len(body); i++ {
		if !isStatementRun(body, i, field.value) {
			continue
		}

		if !statementRunBoundedAt(body, i, field.value) {
			continue
		}

		// Nothing but the framing this locator understands may sit between the
		// header field and the statement. Another copy of the declared length
		// immediately in front of the value is a second thing that would have to
		// change with it, and the round-trip check cannot see it — re-encoding
		// the *same* value reproduces the frame whether or not that copy is
		// inside the span being rewritten. This is the generalization of the
		// chunk-header case above.
		if valuePrecededByAnotherLength(body, i, field.value) {
			return stmtRewrite{}, false
		}

		hits++
		if hits > 1 {
			return stmtRewrite{}, false
		}

		found = stmtRewrite{
			lenAt:    field.at,
			lenWidth: field.width,
			lenKind:  field.kind,
			valueAt:  i,
			valueEnd: i + field.value,
			clrKind:  stmtClrNone,
			run:      body[i : i+field.value],
		}

		// go-ora, python-oracledb thin and the OCI clients repeat the length as
		// a CLR byte immediately in front of the run; ojdbc and DBeaver do not,
		// and write a zero there, so the two are told apart by what is actually
		// on the wire rather than by which client dbbat thinks it is talking to.
		if i > from && field.value <= 0xFF && body[i-1] == byte(field.value) {
			// …except at 0xFC..0xFF, where a one-byte prefix is a length this
			// encoder will not write (clrShortMaxLen) and the byte is also what
			// the long form and the null markers use. Reading such a frame as a
			// bare run would leave that prefix behind, still declaring the old
			// length, in front of a longer statement — a desynchronized message
			// rather than a wrong comment. Neither reading is reproducible, so
			// the frame is refused outright.
			if field.value > clrShortMaxLen {
				return stmtRewrite{}, false
			}

			found.clrKind = stmtClrShort
			found.valueAt = i - 1
		}
	}

	return found, hits == 1
}

// valuePrecededByAnotherLength reports whether the bytes immediately in front of
// a statement run spell the declared length again, in any of the encodings this
// package can write.
//
// One-byte encodings are excluded: that is the CLR short form's own prefix,
// which the caller identifies and rewrites deliberately. What is being caught
// here is a *second* length — a CLR chunk header, a repeated field — that the
// rewriter would leave behind still declaring the old size.
func valuePrecededByAnotherLength(body []byte, valueAt, declared int) bool {
	// `0xFE <len> <text> 0x00`: a CLR long form carrying a value *below* the
	// 252-byte short-form limit, which is the one instance of the ORA-03120 bug
	// class the two guards around this one do not see. locateChunkedStatementValue
	// bails out under clrChunkedMinLen and never looks; the encodings below do not
	// match, because a compressed int of a value under 256 is `01 <n>` and not
	// `FE <n>`. So the scan would read the 0xFE as framing it does not own and
	// the byte after it as an ordinary short prefix, and a tag pushing the value
	// past the limit would then nest a whole new `0xFE … 0x00` inside the
	// client's own. One byte of the *next* length would be enough to
	// desynchronize the message; a nested long form certainly is.
	if valueAt >= 2 && body[valueAt-2] == 0xFE && body[valueAt-1] == byte(declared) {
		return true
	}

	ub4 := make([]byte, 4)
	binary.LittleEndian.PutUint32(ub4, uint32(declared*wideCharWidth))

	for _, enc := range [][]byte{
		ttcCompressedUint(uint64(declared)),
		encodeVarLenBytes(declared),
		ub4,
	} {
		if len(enc) < 2 || valueAt-len(enc) < 0 {
			continue
		}

		if bytes.Equal(body[valueAt-len(enc):valueAt], enc) {
			return true
		}
	}

	return false
}

// locateChunkedStatementValue finds a statement the client wrote in the CLR long
// form, where the text does not sit contiguously and the contiguous scan above
// structurally cannot see it.
//
// The chunk convention is read off the wire rather than assumed — every chunk
// but the last the same size, lengths as compressed integers or as single bytes
// — and then checked by re-encoding, which is what lets a rewritten value be
// re-chunked the way this client chunks.
func locateChunkedStatementValue(body []byte, field execSQLLenField) (stmtRewrite, bool) {
	if field.value < clrChunkedMinLen {
		return stmtRewrite{}, false
	}

	var (
		found stmtRewrite
		hits  int
	)

	from := field.at + field.width

	for i := from; i < len(body); i++ {
		if body[i] != 0xFE {
			continue
		}

		for _, big := range [...]bool{true, false} {
			rw, ok := chunkedValueAt(body, i, field, big)
			if !ok {
				continue
			}

			hits++
			if hits > 1 {
				return stmtRewrite{}, false
			}

			found = rw
		}
	}

	return found, hits == 1
}

// chunkedValueAt walks one candidate CLR long form and reports the rewrite plan
// it yields, when the chunks concatenate to exactly the declared length, every
// chunk but the last is the same size, and the run they spell is statement text.
func chunkedValueAt(body []byte, marker int, field execSQLLenField, big bool) (stmtRewrite, bool) {
	var (
		run       []byte
		chunkSize int
	)

	pos := marker + 1

	for {
		var chunkLen, n int

		if big {
			chunkLen, n = readCompressedInt(body[pos:])
			if n == 0 {
				return stmtRewrite{}, false
			}
		} else {
			if pos >= len(body) {
				return stmtRewrite{}, false
			}

			chunkLen, n = int(body[pos]), 1
		}

		pos += n

		if chunkLen == 0 {
			break
		}

		if pos+chunkLen > len(body) || len(run)+chunkLen > field.value {
			return stmtRewrite{}, false
		}

		// Every chunk but the last is the client's chunk size, and the last is
		// what was left. A run that does not fit that shape is not something
		// this package knows how to re-chunk.
		switch {
		case chunkSize == 0:
			chunkSize = chunkLen
		case chunkLen > chunkSize:
			return stmtRewrite{}, false
		case chunkLen < chunkSize && len(run)+chunkLen != field.value:
			return stmtRewrite{}, false
		}

		if run == nil {
			run = make([]byte, 0, field.value)
		}

		run = append(run, body[pos:pos+chunkLen]...)
		pos += chunkLen
	}

	if len(run) != field.value || !isStatementRun(run, 0, len(run)) {
		return stmtRewrite{}, false
	}

	return stmtRewrite{
		lenAt:     field.at,
		lenWidth:  field.width,
		lenKind:   field.kind,
		valueAt:   marker,
		valueEnd:  pos,
		clrKind:   stmtClrChunked,
		chunkSize: chunkSize,
		chunkBig:  big,
		run:       run,
	}, true
}

// isStatementRun reports whether body[at:at+n] is a statement value: printable
// statement text throughout, opening with a SQL verb, optionally closed by the
// single NUL an OCI client counts in the length it declares.
//
// It is the gate's own test (sanitizeSQLRun + startsWithSQLVerb, the pair that
// stops a run of bind values or a client-identifier string from answering), with
// the trailing NUL allowance the exec decoders express as "accept sqlLen or
// sqlLen-1".
func isStatementRun(body []byte, at, n int) bool {
	if at < 0 || n <= 0 || at+n > len(body) {
		return false
	}

	run := body[at : at+n]
	if run[n-1] == 0 {
		run = run[:n-1]
	}

	if len(run) == 0 {
		return false
	}

	text, ok := sanitizeSQLRun(string(run))

	return ok && startsWithSQLVerb(text)
}

// statementRunBoundedAt reports whether a run of n bytes at i is bounded at both
// ends — it neither continues a text run that started earlier nor stops in the
// middle of one that keeps going.
//
// The left end allows exactly one thing besides a non-text byte: the CLR length
// prefix, which is printable whenever the statement is 32..126 bytes long and
// would otherwise slide the match one byte left. That is the same allowance
// execTextRunStartsHere makes, and for the same reason.
func statementRunBoundedAt(body []byte, i, n int) bool {
	if i > 0 && isPrintableSQLByte(body[i-1]) && body[i-1] != byte(n) {
		return false
	}

	return i+n >= len(body) || !isPrintableSQLByte(body[i+n])
}
