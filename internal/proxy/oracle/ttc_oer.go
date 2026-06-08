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
	ErrorMessage string
}

// oerEndOfCallBit is set in CallStatus on every real OER message observed in
// captures (success and error, DDL and DML). Byte runs inside the preceding
// return-parameter block that happen to start with 0x04 don't carry it, which
// makes it the discriminator for the scanning locator below.
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
	if offset >= len(payload) || payload[offset] != 0x04 {
		return nil
	}

	pos := offset + 1

	var fields [len(oerFieldMaxSizes)]int

	for i, maxSize := range oerFieldMaxSizes {
		val, n := readCompressedInt(payload[pos:])
		if n == 0 || n-1 > maxSize {
			return nil
		}

		fields[i] = val
		pos += n
	}

	info := &oerInfo{
		CallStatus:   fields[0],
		SeqNumber:    fields[1],
		CurRowNumber: fields[2],
		ErrorCode:    fields[3],
	}

	if info.CallStatus&oerEndOfCallBit == 0 {
		return nil
	}

	if info.ErrorCode != 0 {
		info.ErrorMessage = extractORAMessage(payload[pos:])
	}

	return info
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
