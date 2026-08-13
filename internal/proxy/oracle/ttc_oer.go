package oracle

import "strings"

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

// oerEndOfCallBit is set in CallStatus on every OER a go-ora session carries
// (success and error, DDL and DML), and on none of a python-oracledb thin
// session's. Byte runs inside a row stream that happen to start with 0x04 don't
// carry it either, which is what makes it worth keeping as the discriminator
// exactly where row bytes are what a false positive would be made of — the
// standalone func=0x04 marker and anything mid-fetch. Outside those, see
// findPlausibleOERInResponse.
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
func decodeOERAt(payload []byte, offset int) *oerInfo {
	info, rest := decodeOERFieldsAt(payload, offset)
	if info == nil || info.CallStatus&oerEndOfCallBit == 0 {
		return nil
	}

	if info.ErrorCode != 0 {
		info.ErrorMessage = extractORAMessage(payload[rest:])
	}

	return info
}

// decodeOERFieldsAt decodes the leading integer fields of an OER whose 0x04
// marker sits at payload[offset], without judging whether the result is a real
// OER. It returns the fields and the offset just past them, or nil.
//
// Split out of decodeOERAt because the end-of-call bit is not universal: every
// OER go-ora's server leg carries it, while python-oracledb's connections get
// OERs with CallStatus 1–2 (see testdata/python_thin_cursor_reexec.pcapng).
// Neither cursor-id learning nor the completion of a call that has already left
// its row stream can afford to demand the bit; the paths where row bytes could
// impersonate an OER still do, through decodeOERAt.
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
func findPlausibleOERInResponse(payload []byte) *oerInfo {
	for i := 1; i < len(payload); i++ {
		if payload[i] != 0x04 {
			continue
		}

		info, _ := decodeOERFieldsAt(payload, i)
		if info == nil {
			continue
		}

		if info.ErrorCode != 0 && info.ErrorCode != oraNoDataFound {
			continue
		}

		if info.SeqNumber > oerMaxSeqNumber || info.CursorID <= 0 || info.CursorID > cursorReexecMaxID {
			continue
		}

		return info
	}

	return nil
}

// findCursorIDInResponse returns the cursor id the server assigned to the
// statement just executed.
//
// dbbat needs this because the modern execute paths (the v315+ piggyback exec
// and the JDBC thin exec) send the statement text with *no* cursor id — the
// server picks one and reports it back, and the client then re-runs the
// statement by that id alone. Without reading it here, a re-execution names a
// cursor dbbat has no statement for.
func findCursorIDInResponse(payload []byte) (uint16, bool) {
	info := findPlausibleOERInResponse(payload)
	if info == nil {
		return 0, false
	}

	return uint16(info.CursorID), true
}

// findOERInResponse scans a Response (func=0x08) payload for the embedded OER
// message that follows the return-parameter block. payload starts at the
// function code byte. Returns nil when no valid OER is found.
func findOERInResponse(payload []byte) *oerInfo {
	for i := 1; i < len(payload); i++ {
		if payload[i] != 0x04 {
			continue
		}

		if info := decodeOERAt(payload, i); info != nil {
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
