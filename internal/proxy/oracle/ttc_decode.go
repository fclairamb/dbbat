package oracle

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// columnDef describes a column in a TTC response.
type columnDef struct {
	Name      string
	TypeCode  uint8
	Size      uint32
	Precision uint8
	Scale     uint8
	Nullable  bool
}

// columnTypeCodes returns the per-column TTC type codes for type-aware value
// decoding. It returns nil when no type is known (all codes zero), so callers
// fall back to the heuristic decoder.
func columnTypeCodes(columns []columnDef) []int {
	types := make([]int, len(columns))

	known := false
	for i, c := range columns {
		types[i] = int(c.TypeCode)
		if c.TypeCode != 0 {
			known = true
		}
	}

	if !known {
		return nil
	}

	return types
}

// TTCResponse contains decoded fields from a TTC Response message.
type TTCResponse struct {
	ReturnCode   uint16
	RowCount     uint32
	Columns      []columnDef
	Rows         [][]interface{}
	MoreData     bool
	IsError      bool
	ErrorCode    int
	ErrorMessage string
}

// decodeTTCResponse decodes a TTC response payload into structured data.
//
// Response layout (simplified):
//
//	Offset  Field
//	0       Function code (0x08)
//	1       Sequence number
//	2-5     Error code (uint32 BE)
//	6-7     Cursor ID (uint16 BE)
//	8-11    Row count (uint32 BE)
//	12-13   Error flag (uint16 BE)
//	14-15   Error message length (uint16 BE) [if error flag set]
//	16+     Error message [if error flag set]
//	-- OR (if no error) --
//	14-15   Column count (uint16 BE) [first response only]
//	16+     Column definitions [if column count > 0]
//	...     Row data
//	...     More-data flag (1 byte)
func decodeTTCResponse(payload []byte) (*TTCResponse, error) {
	if len(payload) < 14 {
		return nil, fmt.Errorf("%w: response needs at least 14 bytes, got %d", ErrOALL8TooShort, len(payload))
	}

	resp := &TTCResponse{}

	// Error code at offset 2
	errCode := binary.BigEndian.Uint32(payload[2:6])
	resp.ReturnCode = uint16(errCode)

	// Row count at offset 8
	resp.RowCount = binary.BigEndian.Uint32(payload[8:12])

	// Error flag at offset 12
	errFlag := binary.BigEndian.Uint16(payload[12:14])

	if errCode != 0 && errFlag != 0 {
		msg, ok := legacyResponseErrorMessage(payload, errCode)
		if !ok {
			// The fixed-offset layout produced an "error" that is not an Oracle
			// diagnostic, which means these bytes are not a legacy Response at
			// all (row-stream content, or a v315+ response). Reject the whole
			// payload rather than persisting raw bytes as Query.Error.
			return nil, fmt.Errorf("%w: code=%d flag=%d", ErrNotLegacyResponse, errCode, errFlag)
		}

		resp.IsError = true
		resp.ErrorCode = int(errCode)
		resp.ErrorMessage = msg

		return resp, nil
	}

	// Parse column definitions if present
	offset := 14
	if offset+2 <= len(payload) {
		colCount := binary.BigEndian.Uint16(payload[offset : offset+2])
		offset += 2

		if colCount > 0 {
			resp.Columns = make([]columnDef, 0, colCount)

			for i := 0; i < int(colCount) && offset < len(payload); i++ {
				col, bytesRead, err := decodeColumnDef(payload[offset:])
				if err != nil {
					break
				}

				resp.Columns = append(resp.Columns, col)
				offset += bytesRead
			}
		}
	}

	// Parse row data if columns are present
	if offset < len(payload) && len(resp.Columns) > 0 {
		for offset < len(payload)-1 { // Reserve last byte for more-data flag
			row, bytesRead, err := decodeRow(payload[offset:], resp.Columns)
			if err != nil || bytesRead == 0 {
				break
			}

			resp.Rows = append(resp.Rows, row)
			offset += bytesRead
		}
	}

	// More-data flag is the last byte
	if offset < len(payload) {
		resp.MoreData = payload[len(payload)-1] != 0
	}

	return resp, nil
}

// maxPlausibleORACode bounds a legacy Response error code. Oracle diagnostics
// are at most five digits (ORA-00000..ORA-65535, PLS-/TNS- likewise), so a code
// at or above this is arbitrary bytes read through the fixed-offset layout.
const maxPlausibleORACode = 100000

// oracleDiagnosticPrefixes are the prefixes every Oracle server diagnostic
// message carries.
var oracleDiagnosticPrefixes = []string{"ORA-", "PLS-", "TNS-"}

// legacyResponseErrorMessage extracts the error text of a legacy TTC Response
// and proves it is a real Oracle diagnostic before the caller treats the
// payload as a failure.
//
// The legacy layout is fixed-offset and misreads anything that is not an
// actual legacy Response — most damagingly the compressed row stream, where
// payload[2:6] and payload[12:14] are row bytes and payload[14:16] is read as a
// message length. Requiring a plausible code AND an ORA-/PLS-/TNS- prefixed,
// printable message is what stops row data from being reported as an error.
//
// No message is synthesized from the code alone: a synthesized "ORA-NNNNN" from
// misread bytes is exactly the fabrication this gate exists to prevent, and
// genuine server errors reach dbbat through the OER path (findOERInResponse),
// not this one.
//
// The trade that removes: a real legacy Response whose message is absent or
// truncated past the end of the payload is now rejected wholesale, so it does
// not complete the query either — the query stays pending until the next call
// boundary (or cleanup) closes it. No capture fixture exhibits that shape, and
// every 0x08 Response across all 16 of them used to decode to a fabricated
// "ORA-<huge number>", so mis-firing here was the far more likely failure.
func legacyResponseErrorMessage(payload []byte, errCode uint32) (string, bool) {
	if errCode >= maxPlausibleORACode {
		return "", false
	}

	if len(payload) < 16 {
		return "", false
	}

	msgLen := int(binary.BigEndian.Uint16(payload[14:16]))
	if msgLen == 0 || 16+msgLen > len(payload) {
		return "", false
	}

	msg := strings.TrimSpace(string(payload[16 : 16+msgLen]))
	if !looksLikeOracleDiagnostic(msg) {
		return "", false
	}

	return msg, true
}

// looksLikeOracleDiagnostic reports whether msg reads as an Oracle diagnostic:
// an ORA-/PLS-/TNS- prefix and no binary content.
func looksLikeOracleDiagnostic(msg string) bool {
	if isBinaryData([]byte(msg)) {
		return false
	}

	for _, prefix := range oracleDiagnosticPrefixes {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}

	return false
}

// decodeColumnDef decodes a single column definition from a TTC response.
// Returns the column definition and the number of bytes consumed.
func decodeColumnDef(data []byte) (columnDef, int, error) {
	if len(data) < 1 {
		return columnDef{}, 0, ErrColumnDefTooShort
	}

	offset := 0

	// Name length + name
	nameLen, bytesRead, err := decodeVarLen(data[offset:])
	if err != nil {
		return columnDef{}, 0, err
	}

	offset += bytesRead

	if offset+int(nameLen) > len(data) {
		return columnDef{}, 0, ErrColumnNameTruncated
	}

	name := string(data[offset : offset+int(nameLen)])
	offset += int(nameLen)

	// Type code (1 byte)
	if offset >= len(data) {
		return columnDef{}, 0, ErrNoTypeCode
	}

	typeCode := data[offset]
	offset++

	// Max size (4 bytes)
	var size uint32
	if offset+4 <= len(data) {
		size = binary.BigEndian.Uint32(data[offset : offset+4])
		offset += 4
	}

	// Precision (1 byte)
	var precision uint8
	if offset < len(data) {
		precision = data[offset]
		offset++
	}

	// Scale (1 byte)
	var scale uint8
	if offset < len(data) {
		scale = data[offset]
		offset++
	}

	// Nullable (1 byte)
	var nullable bool
	if offset < len(data) {
		nullable = data[offset] != 0
		offset++
	}

	return columnDef{
		Name:      name,
		TypeCode:  typeCode,
		Size:      size,
		Precision: precision,
		Scale:     scale,
		Nullable:  nullable,
	}, offset, nil
}

// decodeRow decodes a single row of data from a TTC response.
// Each column value is encoded as: length (varlen) + value bytes.
func decodeRow(data []byte, columns []columnDef) ([]interface{}, int, error) {
	if len(data) == 0 {
		return nil, 0, ErrEmptyRowData
	}

	row := make([]interface{}, len(columns))
	offset := 0

	for i := range columns {
		if offset >= len(data) {
			break
		}

		valLen, bytesRead, err := decodeVarLen(data[offset:])
		if err != nil {
			return nil, 0, err
		}

		offset += bytesRead

		if valLen == 0 {
			row[i] = nil
			continue
		}

		if offset+int(valLen) > len(data) {
			return nil, 0, ErrRowValueTruncated
		}

		valBytes := data[offset : offset+int(valLen)]
		offset += int(valLen)

		decoded, err := decodeOracleValue(columns[i].TypeCode, valBytes)
		if err != nil {
			// On decode error, store raw as string
			decoded = string(valBytes)
		}

		row[i] = decoded
	}

	return row, offset, nil
}

// Decoding errors.
var (
	ErrEmptySQL         = errors.New("OALL8 message contains empty SQL")
	ErrOALL8TooShort    = errors.New("OALL8 payload too short")
	ErrSQLLengthInvalid = errors.New("OALL8 SQL length exceeds payload")
	// ErrNotLegacyResponse reports that a payload does not follow the legacy
	// fixed-offset Response layout — its "error" fields decode to something
	// that is not an Oracle diagnostic. Callers ignore such payloads instead
	// of acting on the misread fields.
	ErrNotLegacyResponse = errors.New("payload is not a legacy TTC Response")
	// ErrOALL8NoSQL reports a *well-formed* OALL8 that carries no SQL text:
	// the client is re-executing a cursor it already parsed. This is
	// deliberately NOT a decode failure — a frame dbbat cannot parse is
	// forwarded ungated (see the Oracle caveat in docs/approvals.md), whereas
	// this one decoded fine and names a cursor whose SQL the session already
	// knows, so it can and must be re-gated against that SQL.
	//
	// Match it with errors.Is; use errors.As on *OALL8NoSQLError to recover
	// the cursor id.
	ErrOALL8NoSQL = errors.New("OALL8 carries no SQL text (cursor re-execution)")
)

// OALL8NoSQLError is ErrOALL8NoSQL for one specific cursor. It carries the
// cursor id that was decoded successfully before the SQL length turned out to
// be zero — throwing that id away is what used to make a re-execution
// indistinguishable from a broken frame.
type OALL8NoSQLError struct {
	CursorID uint16
}

func (e *OALL8NoSQLError) Error() string {
	return fmt.Sprintf("%s: cursor %d", ErrOALL8NoSQL.Error(), e.CursorID)
}

// Unwrap makes errors.Is(err, ErrOALL8NoSQL) true.
func (e *OALL8NoSQLError) Unwrap() error { return ErrOALL8NoSQL }

// ErrPiggybackExecNoSQL is ErrOALL8NoSQL for the op modern clients actually
// send: a *well-formed* `03 5e` execute whose header declares a statement
// length of zero. Same meaning — the client is re-executing a cursor it already
// parsed — and the same reason for not being a decode failure: a frame dbbat
// cannot parse is forwarded ungated, and this one parsed fine.
//
// Match it with errors.Is; use errors.As on *PiggybackExecNoSQLError to recover
// the cursor id.
var ErrPiggybackExecNoSQL = errors.New("piggyback exec carries no SQL text (cursor re-execution)")

// PiggybackExecNoSQLError is ErrPiggybackExecNoSQL for one specific cursor.
// It is OALL8NoSQLError's counterpart on the v315+ execute op — see
// execNoStatementCursor for the recording that turned this frame up, and for
// what it was doing before it had a name (going upstream ungated).
type PiggybackExecNoSQLError struct {
	CursorID uint16
}

func (e *PiggybackExecNoSQLError) Error() string {
	return fmt.Sprintf("%s: cursor %d", ErrPiggybackExecNoSQL.Error(), e.CursorID)
}

// Unwrap makes errors.Is(err, ErrPiggybackExecNoSQL) true.
func (e *PiggybackExecNoSQLError) Unwrap() error { return ErrPiggybackExecNoSQL }

// ErrNotCursorReexec reports that a payload is not a decodable piggyback
// cursor re-execution (wrong sub-op, truncated, or a cursor id of zero — which
// would mean "allocate a new cursor", not "re-run that one").
var ErrNotCursorReexec = errors.New("payload is not a piggyback cursor re-execution")

// cursorReexecMaxID bounds a plausible cursor id. Oracle allots them from a
// small per-session pool (open_cursors); anything past 16 bits is a sign the
// compressed-int walk landed on the wrong bytes.
const cursorReexecMaxID = 0xFFFF

// cursorReexecTrailingFields is how many compressed ints follow the cursor id
// in a re-execution: rows to fetch, execute options, execute flags.
const cursorReexecTrailingFields = 3

// decodeCursorReexec extracts the cursor id from a piggyback re-execution —
// func 0x03, sub-op 0x4e (SELECT) or 0x04 (everything else). This is what a
// modern thin client puts on the wire to re-run a statement it already parsed;
// the statement text is never resent.
//
// Layout, verified byte-for-byte against testdata/go_ora_cursor_reexec.pcapng,
// testdata/go_ora_dml_cursor_reexec.pcapng and
// testdata/python_thin_cursor_reexec.pcapng:
//
//	[0]    0x03 (piggyback)
//	[1]    0x4e or 0x04 (sub-op)
//	[2]    TTC sequence number
//	[3]    0x00 — present only from TTC version 18 (v315+) on
//	[4..]  cursorID, rowsToFetch, execOptions, execFlags — TTC compressed ints
//
// The trailing zero of the function header is what distinguishes the two
// framings: pre-v315 clients emit a 3-byte header, so the fields start one byte
// earlier. Only the cursor id is read — the rest of the frame says how to run
// the statement, not which statement it is.
func decodeCursorReexec(ttcPayload []byte) (uint16, error) {
	if len(ttcPayload) < 5 || ttcPayload[0] != byte(TTCFuncPiggyback) || !IsPiggybackCursorReexec(ttcPayload) {
		return 0, ErrNotCursorReexec
	}

	// TTC >= 18 pads the function header with a zero byte; older ones do not.
	pos := 3
	if ttcPayload[3] == 0 {
		pos = 4
	}

	cursorID, n := readCompressedInt(ttcPayload[pos:])
	if n == 0 || cursorID <= 0 || cursorID > cursorReexecMaxID {
		return 0, fmt.Errorf("%w: cursor id decoded as %d", ErrNotCursorReexec, cursorID)
	}

	// The other three fields are not read, only walked: a re-execution is
	// exactly these four integers and nothing else, and requiring them to
	// consume the frame to the byte is what keeps some *other* piggyback
	// sub-op from being mistaken for one and gated against a cursor it has
	// nothing to do with.
	pos += n

	for range cursorReexecTrailingFields {
		_, n := readCompressedInt(ttcPayload[pos:])
		if n == 0 {
			return 0, fmt.Errorf("%w: truncated execution fields", ErrNotCursorReexec)
		}

		pos += n
	}

	if pos != len(ttcPayload) {
		return 0, fmt.Errorf("%w: %d trailing bytes", ErrNotCursorReexec, len(ttcPayload)-pos)
	}

	return uint16(cursorID), nil
}

// ErrNotCloseCursors reports that a payload is not a decodable close-cursors
// piggyback — wrong message type or function, missing pointer flag, an
// implausible count, or truncated before the list ends. Callers delete nothing
// when they see it: a half-read list would evict tracker entries the client
// never closed, and an evicted entry turns a correctly-gated re-execution into
// a refusal.
var ErrNotCloseCursors = errors.New("payload is not a close-cursors piggyback")

const (
	// closeCursorsPointer is the one-byte pointer flag Oracle writes between
	// the function header and the list. Requiring it is what keeps a
	// wide-encoded (OCI) frame — whose next field is an 8-byte sentinel — from
	// being walked as compressed ints.
	closeCursorsPointer byte = 0x01

	// closeCursorsMaxCount bounds a plausible batch. Cursors come from a
	// per-session pool (open_cursors, a few hundred by default); a count past
	// this means the walk landed on the wrong bytes.
	closeCursorsMaxCount = 4096
)

// closeCursorsWideSentinel is the 8-byte pointer placeholder the OCI thick
// client (sqlplus, SQL*Developer via OCI, Instant Client) writes instead of
// the compressed-int pointer flag — the same sentinel the AUTH path already
// knows (see payloadUsesWideKVEncoding, findUserIDLenPos).
var closeCursorsWideSentinel = []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// closeCursorsWideHeaderLen is the size, in bytes, of the two-byte pad OCI
// writes between the TTC sequence number and the pointer sentinel — see
// isCloseCursorsWideHeader.
const closeCursorsWideHeaderLen = 2

// isCloseCursorsWideHeader reports whether ttcPayload's close-cursors header
// is OCI's wide encoding: [seq] [0x01] [seq+1] [8-byte pointer sentinel].
//
// The two bytes between the sequence number and the sentinel were not
// guessed at: pinned byte-for-byte against every close-cursors frame in
// testdata/sqlplus_cursor_reexec.pcapng (client frames 9, 12, 14, 16) plus
// the piggyback execute-with-SQL header stapled behind three of them (func
// 0x03 sub 0x5e), which carries the identical two-byte pad ahead of its own
// sentinel. In every one of those seven headers the first byte is a constant
// 0x01 and the second equals that header's OWN sequence number plus one —
// i.e. the sequence number the NEXT TTC message on the wire will carry. That
// makes it the wide framing's own header padding, not something specific to
// the close-cursors op, so decodeCloseCursors validates rather than skips it:
// a payload whose two bytes don't fit this shape is read as the thin
// (compressed-int) encoding instead, never guessed at as wide.
func isCloseCursorsWideHeader(ttcPayload []byte) bool {
	const sentinelStart = 3 + closeCursorsWideHeaderLen

	if len(ttcPayload) < sentinelStart+len(closeCursorsWideSentinel) {
		return false
	}

	if ttcPayload[3] != closeCursorsPointer {
		return false
	}

	if ttcPayload[4] != ttcPayload[2]+1 {
		return false
	}

	return bytes.Equal(ttcPayload[sentinelStart:sentinelStart+len(closeCursorsWideSentinel)], closeCursorsWideSentinel)
}

// closeCursorsWide8HeaderLen is the size of the op header the *other* OCI
// flavor writes — see isCloseCursorsWide8Header. It replaces the 5-byte header
// above and everything after it shifts by the difference, which is the whole
// reason this needs its own walk rather than an offset tweak.
const closeCursorsWide8HeaderLen = 17

// closeCursorsWide8SeqOffset is where that header carries its "next sequence"
// field, as a little-endian uint64.
const closeCursorsWide8SeqOffset = 9

// isCloseCursorsWide8Header reports whether ttcPayload uses the 64-bit variant
// of the OCI header: `[msg][func][seq][0x00][0x00][ub4 …][sb8 seq+1]` followed
// by the same 8-byte pointer sentinel.
//
// It exists because "the OCI encoding" turned out to be two encodings, and the
// difference hung a client. The Instant Client 23.3 the wide support was
// captured from writes the 5-byte header above, with 4-byte integers after it;
// the client bundled in gvenzl/oracle-free:23-slim (23.26) writes this one,
// with 8-byte integers. Same protocol version, same upstream, different widths
// — so a decoder pinned to one of them reads the other's close list as garbage
// and, far worse, never finds the call stapled behind it:
//
//	11 69 0d 00 00 7b 05 00 00 0e 00 00 00 00 00 00 00  ← header, seq 13
//	fe(x8)                                              ← pointer sentinel
//	01 00 00 00 00 00 00 00  02 00 00 00                ← one cursor: id 2
//	03 5e 0e …                                          ← the call, sequence 14
//
// Refusing that INSERT with the sequence dbbat could see (13, or whatever the
// previous call was) rather than 14 is what left sqlplus waiting forever.
//
// The guard is the same one the 4-byte variant uses, and for the same reason:
// the second field must be this header's own sequence number plus one — the
// sequence the next TTC message will carry — so a payload that does not fit
// the shape is read as one of the other encodings instead of guessed at.
func isCloseCursorsWide8Header(ttcPayload []byte) bool {
	return usesWide64OpHeader(ttcPayload)
}

// usesWide64OpHeader reports whether a TTC message opens with the 64-bit OCI op
// header, whatever op it is: `[msg][func][seq][0x00][0x00][ub4 …][sb8 seq+1]`
// followed by the 8-byte pointer sentinel.
//
// It is deliberately not specific to close-cursors. The same header opens this
// client's AUTH Phase 1 (`03 76 02 00 00 00000000 0300000000000000 fe…`), which
// is what lets a session know it is talking to a 64-bit OCI client *before* the
// upstream has sent an OER to learn the shape from — see nextOERFrame. The
// Instant Client's 32-bit header (`[seq][0x01][seq+1]`) never satisfies it: its
// byte 3 is the 0x01 pointer flag, not zero.
//
// The "next sequence" check is what makes it a shape test rather than a guess.
// The second field is always this header's own sequence number plus one — the
// sequence the next TTC message on the wire will carry — measured across every
// recorded frame from this client, AUTH and proxy mode alike.
func usesWide64OpHeader(ttcPayload []byte) bool {
	const sentinelStart = closeCursorsWide8HeaderLen

	if len(ttcPayload) < sentinelStart+len(closeCursorsWideSentinel) {
		return false
	}

	if ttcPayload[3] != 0x00 || ttcPayload[4] != 0x00 {
		return false
	}

	next := binary.LittleEndian.Uint64(ttcPayload[closeCursorsWide8SeqOffset : closeCursorsWide8SeqOffset+8])
	if next != uint64(ttcPayload[2])+1 {
		return false
	}

	return bytes.Equal(ttcPayload[sentinelStart:sentinelStart+len(closeCursorsWideSentinel)], closeCursorsWideSentinel)
}

// decodeCloseCursorsWide8 walks the 64-bit OCI close list: an 8-byte count,
// then one 4-byte id per cursor. The id width is *not* symmetric with the
// count, and that is measured rather than assumed — the stapled op that follows
// lands exactly at count*4 bytes past the count in every recorded frame, which
// is the only reading under which dbbat can find the call behind the list.
//
// Same guards as the other two walks: a bounded count, every id inside 16 bits,
// enough bytes for the whole list. A payload that does not fit is rejected and
// nothing is deleted.
func decodeCloseCursorsWide8(ttcPayload []byte) ([]uint16, int, error) {
	pos := closeCursorsWide8HeaderLen + len(closeCursorsWideSentinel)

	if pos+8 > len(ttcPayload) {
		return nil, 0, fmt.Errorf("%w: truncated wide count", ErrNotCloseCursors)
	}

	count := binary.LittleEndian.Uint64(ttcPayload[pos : pos+8])
	if count == 0 || count > closeCursorsMaxCount {
		return nil, 0, fmt.Errorf("%w: wide cursor count decoded as %d", ErrNotCloseCursors, count)
	}

	pos += 8

	if pos+4*int(count) > len(ttcPayload) {
		return nil, 0, fmt.Errorf("%w: truncated after wide count of %d", ErrNotCloseCursors, count)
	}

	cursorIDs := make([]uint16, 0, count)

	for range count {
		id := binary.LittleEndian.Uint32(ttcPayload[pos : pos+4])
		if id == 0 || id > cursorReexecMaxID {
			return nil, 0, fmt.Errorf("%w: wide cursor id decoded as %d", ErrNotCloseCursors, id)
		}

		cursorIDs = append(cursorIDs, uint16(id))
		pos += 4
	}

	return cursorIDs, pos, nil
}

// decodeCloseCursorsWide extracts the cursor ids out of an OCI wide-encoded
// close-cursors piggyback, once isCloseCursorsWideHeader has confirmed the
// header shape. The count and every id are little-endian uint32 fields
// (unlike the thin encoding's compressed ints), but the guards mirror
// decodeCloseCursors exactly: a bounded count, every id inside 16 bits, and
// enough bytes for the whole list — so a payload that doesn't fit is
// rejected with ErrNotCloseCursors and deletes nothing, same as the thin
// path.
func decodeCloseCursorsWide(ttcPayload []byte) ([]uint16, int, error) {
	pos := 3 + closeCursorsWideHeaderLen + len(closeCursorsWideSentinel)

	if pos+4 > len(ttcPayload) {
		return nil, 0, fmt.Errorf("%w: truncated wide count", ErrNotCloseCursors)
	}

	count := binary.LittleEndian.Uint32(ttcPayload[pos : pos+4])
	if count > closeCursorsMaxCount {
		return nil, 0, fmt.Errorf("%w: wide cursor count decoded as %d", ErrNotCloseCursors, count)
	}

	pos += 4

	if pos+4*int(count) > len(ttcPayload) {
		return nil, 0, fmt.Errorf("%w: truncated after wide count of %d", ErrNotCloseCursors, count)
	}

	cursorIDs := make([]uint16, 0, count)

	for range count {
		id := binary.LittleEndian.Uint32(ttcPayload[pos : pos+4])
		if id == 0 || id > cursorReexecMaxID {
			return nil, 0, fmt.Errorf("%w: wide cursor id decoded as %d", ErrNotCloseCursors, id)
		}

		cursorIDs = append(cursorIDs, uint16(id))
		pos += 4
	}

	return cursorIDs, pos, nil
}

// decodeCloseCursors extracts every cursor id from Oracle's close-cursors
// piggyback — message type 0x11 (TNS_MSG_TYPE_PIGGYBACK), function 0x69
// (TNS_FUNC_CLOSE_CURSORS).
//
// This is how a client tells the server it is done with cursors, and it is a
// *list*: dbbat used to read a single id out of the func-0x03 logoff frame
// instead, so batched closes left the tracker holding entries for cursors that
// no longer exist — and Oracle recycles ids, so a later re-execution naming a
// recycled id resolved to whatever statement used to hold it.
//
// Layout, verified byte-for-byte against the recordings in testdata/:
//
//	[0]    0x11            message type: piggyback
//	[1]    0x69            function: close cursors
//	[2]    seq             TTC sequence number
//	[3]    0x00            token byte — 23ai-era clients only
//	[..]   0x01            pointer flag
//	[..]   count           TTC compressed int
//	[..]   count x id      TTC compressed ints
//	[..]   (optional)      the next TTC message in the same packet
//
// The trailing zero of the function header is what distinguishes the two
// framings, exactly as in decodeCursorReexec; the pointer flag is always 0x01,
// so a zero at [3] can only be the token.
//
// Unlike decodeCursorReexec this does **not** require the fields to consume the
// frame: clients staple the statement they are about to run behind the close
// list in the same packet (`… 03 5e <execute>`), which is the frame dbbat also
// knows as the JDBC/DBeaver execute. What it does require is that the list
// itself be complete and plausible — see ErrNotCloseCursors.
//
// The OCI thick client (sqlplus, SQL*Developer via OCI, Instant Client) sends
// the same op in the wide encoding — an 8-byte pointer sentinel and
// little-endian 32-bit fields — which decodeCloseCursorsWide reads once
// isCloseCursorsWideHeader confirms the header shape. It never re-executes by
// cursor id (it resends the statement text every time, see docs/oracle.md),
// so this is defense in depth rather than something load-bearing: a tracker
// entry it leaves behind cannot mis-resolve anything on its own.
func decodeCloseCursors(ttcPayload []byte) ([]uint16, error) {
	ids, _, err := decodeCloseCursorsAt(ttcPayload)

	return ids, err
}

// closeCursorsEnd returns the offset just past a close-cursors list — where a
// stapled TTC op begins, if the client put one there. It is how
// clientCallNumber reaches the execute JDBC staples behind its closes; false
// when the payload is not a close-cursors piggyback or its list does not
// decode.
func closeCursorsEnd(ttcPayload []byte) (int, bool) {
	_, end, err := decodeCloseCursorsAt(ttcPayload)

	return end, err == nil
}

// decodeCloseCursorsAt is decodeCloseCursors plus the offset the close list
// ends at.
func decodeCloseCursorsAt(ttcPayload []byte) ([]uint16, int, error) {
	if !IsCloseCursorsPiggyback(ttcPayload) {
		return nil, 0, ErrNotCloseCursors
	}

	if isCloseCursorsWideHeader(ttcPayload) {
		return decodeCloseCursorsWide(ttcPayload)
	}

	if isCloseCursorsWide8Header(ttcPayload) {
		return decodeCloseCursorsWide8(ttcPayload)
	}

	// TTC >= 18 pads the function header with a zero byte; older ones do not.
	pos := 3
	if len(ttcPayload) > 3 && ttcPayload[3] == 0 {
		pos = 4
	}

	if pos >= len(ttcPayload) || ttcPayload[pos] != closeCursorsPointer {
		return nil, 0, fmt.Errorf("%w: no pointer flag at offset %d", ErrNotCloseCursors, pos)
	}

	pos++

	count, n := readCompressedInt(ttcPayload[pos:])
	if n == 0 || count < 0 || count > closeCursorsMaxCount {
		return nil, 0, fmt.Errorf("%w: cursor count decoded as %d", ErrNotCloseCursors, count)
	}

	pos += n

	cursorIDs := make([]uint16, 0, count)

	for range count {
		cursorID, n := readCompressedInt(ttcPayload[pos:])
		if n == 0 {
			return nil, 0, fmt.Errorf("%w: truncated after %d of %d ids", ErrNotCloseCursors, len(cursorIDs), count)
		}

		if cursorID <= 0 || cursorID > cursorReexecMaxID {
			return nil, 0, fmt.Errorf("%w: cursor id decoded as %d", ErrNotCloseCursors, cursorID)
		}

		cursorIDs = append(cursorIDs, uint16(cursorID))
		pos += n
	}

	return cursorIDs, pos, nil
}

// OALL8Result contains the decoded fields from an OALL8 (parse+execute) message.
type OALL8Result struct {
	SQL        string
	CursorID   uint16
	BindValues []string
	// Truncated reports that SQL is a *prefix* of the statement the client
	// sent: the run dbbat extracted was cut by the end of the frame rather
	// than by the TTC framing behind the statement. It is never merely
	// cosmetic — everything past the cut (a blocked pattern, an approval
	// pattern, the dynamic-SQL scan) is text the gate never saw — so a caller
	// must not put it through ValidateOracleQuery as if it were the statement.
	// See the session's gatePartialStatement.
	Truncated bool
}

// IsPLSQL returns true if the SQL text is a PL/SQL block.
func (r *OALL8Result) IsPLSQL() bool {
	normalized := strings.ToUpper(strings.TrimSpace(r.SQL))
	return strings.HasPrefix(normalized, "BEGIN") || strings.HasPrefix(normalized, "DECLARE")
}

// OALL8 binary layout (simplified):
//
//	Offset  Size     Field
//	0       1        Function code (0x0E) — already consumed by caller
//	1       4        Options (uint32 BE)
//	5       2        Cursor ID (uint16 BE)
//	7       1        SQL length encoding:
//	                   - If < 0xFE: SQL length is this byte
//	                   - If == 0xFE: next 2 bytes (uint16 BE) are the SQL length
//	                   - If == 0xFF: next 4 bytes (uint32 BE) are the SQL length
//	?       N        SQL text (UTF-8)
//	?       2        Bind count (uint16 BE)
//	?       ...      Bind definitions (skipped)
//	?       ...      Bind values
//
// Note: This is a simplified decoding that handles the most common cases.
// Real Oracle TTC encoding uses variable-length integers extensively.

const (
	oall8MinPayloadSize = 8 // func(1) + options(4) + cursor(2) + sql_len(1)
	oall8LenShort       = 0xFE
	oall8LenLong        = 0xFF
)

// decodeOALL8 decodes an OALL8 TTC payload (starting from the function code byte).
func decodeOALL8(ttcPayload []byte) (*OALL8Result, error) {
	if len(ttcPayload) < oall8MinPayloadSize {
		return nil, fmt.Errorf("%w: got %d bytes, need at least %d", ErrOALL8TooShort, len(ttcPayload), oall8MinPayloadSize)
	}

	// Skip function code (1 byte) + options (4 bytes)
	offset := 5

	// Cursor ID (2 bytes, big-endian)
	cursorID := binary.BigEndian.Uint16(ttcPayload[offset : offset+2])
	offset += 2

	// SQL length (variable encoding)
	sqlLen, bytesRead, err := decodeVarLen(ttcPayload[offset:])
	if err != nil {
		return nil, fmt.Errorf("failed to decode SQL length: %w", err)
	}

	offset += bytesRead

	// A zero SQL length is not a malformed frame: everything up to here parsed,
	// and the cursor id is good. Report it as its own condition, carrying that
	// id, so the caller can re-gate the re-execution instead of treating it as
	// an undecodable packet and waving it through.
	if sqlLen == 0 {
		return nil, &OALL8NoSQLError{CursorID: cursorID}
	}

	// SQL text
	if offset+int(sqlLen) > len(ttcPayload) {
		// The declared statement runs past this frame. Reassembly (reassembly.go)
		// is what normally makes that impossible; a frame that reaches here
		// anyway — a length past the reassembly bound, a frame dbbat was handed
		// without its continuations — holds a prefix of a statement. Hand the
		// prefix back *marked as one* rather than reporting a decode failure,
		// because a decode failure is forwarded ungated and the bytes past the
		// cut are exactly where a blocked pattern would sit.
		if prefix, ok := sanitizeSQLRun(string(ttcPayload[offset:])); ok && startsWithSQLVerb(prefix) {
			return &OALL8Result{SQL: prefix, CursorID: cursorID, Truncated: true}, nil
		}

		return nil, fmt.Errorf("%w: sql_len=%d, remaining=%d", ErrSQLLengthInvalid, sqlLen, len(ttcPayload)-offset)
	}

	sqlText := string(ttcPayload[offset : offset+int(sqlLen)])
	offset += int(sqlLen)

	// Bind count (2 bytes, big-endian) — optional, may not be present
	var bindValues []string

	if offset+2 <= len(ttcPayload) {
		bindCount := binary.BigEndian.Uint16(ttcPayload[offset : offset+2])
		offset += 2

		if bindCount > 0 {
			bindValues = decodeBindValues(ttcPayload[offset:], int(bindCount))
		}
	}

	return &OALL8Result{
		SQL:        sqlText,
		CursorID:   cursorID,
		BindValues: bindValues,
	}, nil
}

// decodePiggybackExecSQL extracts SQL text from a v315+ piggyback execute message.
//
// The TTC payload layout for func=0x03, sub=0x5e:
//
//	Offset  Field
//	[0]     0x03 (function code)
//	[1]     0x5e (sub-operation: execute with SQL)
//	[2-49]  cursor options, flags, parameters (fixed size for common cases)
//	[50]    SQL length (varlen encoding: 1 byte if < 0xFE, etc.)
//	[51+]   SQL text (UTF-8)
//
// This function scans for the SQL text by looking for a length-prefixed readable
// string in the expected region. This is more robust than assuming a fixed offset,
// since the exact layout may vary by Oracle version.
func decodePiggybackExecSQL(ttcPayload []byte, wide64 bool) (*OALL8Result, error) {
	if len(ttcPayload) < 52 {
		return nil, fmt.Errorf("%w: piggyback exec needs at least 52 bytes, got %d", ErrOALL8TooShort, len(ttcPayload))
	}

	// The header carries the statement's length, so read that first and take
	// the run it names. Everything below is the pre-2026-08 heuristic, kept for
	// a header shape no recording produces — see decodeExecStatement.
	stmt, located := decodeExecStatementText(ttcPayload, wide64)

	// A header that walks cleanly and declares **no** statement is not a frame
	// this decode failed on: it is a re-execution of a cursor already parsed,
	// and it gets reported as such so the caller can gate it against that
	// cursor's SQL instead of waving it through. See execNoStatementCursor.
	if !located {
		if cursorID, reexec := execNoStatementCursor(ttcPayload, wide64); reexec {
			return nil, &PiggybackExecNoSQLError{CursorID: cursorID}
		}
	}

	// Strategy: scan the payload for SQL text. Different Oracle client drivers
	// (oracledb thin, JDBC thin) place the SQL at slightly different offsets
	// (50-54 typically). We scan a range and validate the extracted text.
	for offset := 40; stmt.Text == "" && offset < 70 && offset < len(ttcPayload)-1; offset++ {
		if found, scanErr := extractSQLAtOffsetText(ttcPayload, offset); scanErr == nil && found.Text != "" {
			stmt = found
		}
	}

	// Last resort: find SQL keywords directly in the payload. It returns a
	// verbatim slice of the payload, so the text is its own anchor.
	var truncated bool

	if stmt.Text == "" {
		if found, cut := findSQLInPayload(ttcPayload); found != "" {
			stmt = execStatement{Text: found, Raw: found}
			truncated = cut
		}
	}

	if stmt.Text == "" {
		return nil, fmt.Errorf("%w: could not find SQL text in piggyback exec payload", ErrEmptySQL)
	}

	return &OALL8Result{
		SQL:        stmt.Text,
		BindValues: extractPiggybackBinds(ttcPayload, stmt),
		Truncated:  truncated,
	}, nil
}

// extractPiggybackBinds recovers the bind values from a piggyback exec payload.
// The values are length-prefixed at the tail of the message and their count
// equals the number of distinct bind placeholders in the statement, so they are
// located as the suffix that parses as exactly that many length-prefixed values
// consuming the rest of the payload. Returns nil when they can't be located —
// binds are then simply not captured rather than guessed wrong.
//
// The floor is anchored on stmt.Raw and not on stmt.Text, and the distinction is
// load-bearing rather than tidiness. Text may have had undecodable bytes
// repaired to U+FFFD for storage (sanitizeSQLRun), and a U+FFFD is three bytes
// where the wire had one — so searching for it would fail to match, the floor
// would collapse to 0, and the tail scan would be free to walk back into the
// statement and read "bind values" out of its own text. That is not the "no
// binds captured" this function promises; it is the guessed-wrong outcome the
// promise exists to rule out.
func extractPiggybackBinds(payload []byte, stmt execStatement) []string {
	count := countBindPlaceholders(stmt.Text)
	if count == 0 {
		return nil
	}

	// Don't scan into the SQL text itself.
	lo := 0
	if idx := findBytes(payload, []byte(stmt.Raw)); idx >= 0 {
		lo = idx + len(stmt.Raw)
	}

	// A chunked statement (CLR long form) does not sit contiguously in the
	// payload, so the search above cannot find it and the floor would collapse
	// to 0 — the guessed-wrong outcome the anchor exists to rule out. The
	// locate knows where the statement's wire bytes end and says so.
	if stmt.End > lo {
		lo = stmt.End
	}

	// Scan from the tail so the tightest (real) value run is found before any
	// bind-definition bytes that might also parse as length-prefixed values.
	for start := len(payload) - 1; start >= lo; start-- {
		vals, ok := readLenPrefixedValues(payload[start:], count)
		if !ok {
			continue
		}

		out := make([]string, len(vals))
		for i, v := range vals {
			out[i] = decodeBindValue(v)
		}

		return out
	}

	return nil
}

// readLenPrefixedValues reads exactly count single-byte-length-prefixed values
// from data, succeeding only if they consume all of data (a strong validity
// check that disambiguates the real bind run from coincidental byte patterns).
func readLenPrefixedValues(data []byte, count int) ([][]byte, bool) {
	vals := make([][]byte, 0, count)
	offset := 0

	for range count {
		if offset >= len(data) {
			return nil, false
		}

		n := int(data[offset])
		offset++

		if n == 0 {
			vals = append(vals, nil)

			continue
		}

		if n > 2000 || offset+n > len(data) {
			return nil, false
		}

		vals = append(vals, data[offset:offset+n])
		offset += n
	}

	if offset != len(data) {
		return nil, false
	}

	return vals, true
}

// countBindPlaceholders counts the distinct bind placeholders (:name or :1) in
// sql, which equals the number of bind values the client sends.
func countBindPlaceholders(sql string) int {
	seen := make(map[string]struct{})

	for i := 0; i < len(sql); i++ {
		if sql[i] != ':' {
			continue
		}

		j := i + 1
		for j < len(sql) && isBindNameByte(sql[j]) {
			j++
		}

		if j > i+1 {
			seen[sql[i:j]] = struct{}{}
			i = j - 1
		}
	}

	return len(seen)
}

func isBindNameByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// decodeExecSQL extracts SQL text from an execute-with-SQL message (func=0x11).
//
// Different Oracle client drivers use func=0x11 with different sub-operations:
//   - DBeaver/JDBC thin: sub=0x69, SQL at TTC offset 57-63
//   - Python oracledb thin: sub=0x98, SQL at TTC offset 63-67
//
// The SQL is preceded by a run of zero bytes and its length is encoded with
// the standard varlen encoding.
func decodeExecSQL(ttcPayload []byte, wide64 bool) (*OALL8Result, error) {
	if len(ttcPayload) < 30 {
		return nil, fmt.Errorf("%w: exec needs at least 30 bytes, got %d", ErrOALL8TooShort, len(ttcPayload))
	}

	// The `11 69` "JDBC exec" is a close-cursors piggyback with the real
	// execute stapled behind it (docs/oracle.md, "Closing cursors"), so walk
	// the close list to that op and decode it properly. The old 50-75 window
	// scanned *past* the list into the stapled SQL and routinely landed inside
	// the statement text — see decodeExecStatement.
	if sql, ok := decodeExecStatement(ttcPayload, wide64); ok {
		return &OALL8Result{SQL: sql}, nil
	}

	if end, ok := closeCursorsEnd(ttcPayload); ok {
		if sql, ok := decodeExecStatement(ttcPayload[end:], wide64); ok {
			return &OALL8Result{SQL: sql}, nil
		}
	}

	// Same reading as decodePiggybackExecSQL's, for the same reason: an execute
	// stapled behind a close list that declares no statement is a re-execution,
	// not an undecodable frame, and the op a client picks must not change
	// whether the gate sees it.
	if cursorID, reexec := execNoStatementCursor(ttcPayload, wide64); reexec {
		return nil, &PiggybackExecNoSQLError{CursorID: cursorID}
	}

	// Scan for SQL text at known offsets across client drivers.
	for offset := 50; offset <= 75 && offset < len(ttcPayload)-1; offset++ {
		sql, err := extractSQLAtOffset(ttcPayload, offset)
		if err == nil && sql != "" {
			return &OALL8Result{SQL: sql}, nil
		}
	}

	// Fallback: find SQL keywords directly
	sql, truncated := findSQLInPayload(ttcPayload)
	if sql != "" {
		return &OALL8Result{SQL: sql, Truncated: truncated}, nil
	}

	return nil, fmt.Errorf("%w: could not find SQL text in JDBC exec payload", ErrEmptySQL)
}

// findSQLKeywords is the keyword set the last-resort scan looks for. It is the
// set of verbs the controls in internal/proxy/shared/validation.go refuse
// (writeKeywords, ddlKeywords) plus the read and block verbs — because a verb
// the gate would refuse but the scan cannot see is a statement that reaches the
// upstream unexamined. TRUNCATE, GRANT and REVOKE were the three missing ones.
//
// Widening a keyword scan is normally how a binary frame comes to be read as a
// statement, which on the unnameable path costs a session. It does not here,
// because it lands together with the word-boundary requirement below: matching
// `GRANT` inside `GRANTED_ROLE` was measured happening on a real DBeaver frame,
// and the boundary rule removes strictly more false positives than these three
// verbs add.
var findSQLKeywords = [][]byte{
	[]byte("SELECT"), []byte("INSERT"), []byte("UPDATE"), []byte("DELETE"),
	[]byte("CREATE"), []byte("DROP"), []byte("ALTER"), []byte("BEGIN"),
	[]byte("DECLARE"), []byte("WITH"), []byte("MERGE"), []byte("CALL"),
	[]byte("TRUNCATE"), []byte("GRANT"), []byte("REVOKE"),
}

// findSQLInPayload scans the raw payload for SQL text by looking for SQL keywords.
// Used as a last resort when the header-anchored decode (decodeExecStatement)
// and the length-prefix window both fail. The keyword match is case-insensitive
// because clients send the statement verbatim and SQLcl lowercases its SQL, and
// it must land on a word boundary so an identifier that merely starts with a
// verb is not read as one.
//
// The second return says the run was cut by the end of the payload rather than
// by the TTC framing behind the statement — i.e. dbbat is holding a prefix. That
// is the honest half of this scan: it has no length to check the run against, so
// "the bytes ran out" is the only signal available, and reporting it is what
// stops a fragment being gated as if it were the statement.
//
// The run itself accepts the same bytes the header-anchored decode accepts
// (isPrintableSQLByte, non-ASCII included) instead of stopping at the first byte
// past 0x7E. Stopping there truncated every statement with an accent in it —
// the 2026-08-31 incident cut a 9KB MERGE at the `è` of 'Surface Pièce' — while
// sanitizeSQLRun, which the anchored path uses, is deliberately charitable about
// a session charset dbbat does not know.
func findSQLInPayload(payload []byte) (string, bool) {
	idx := indexOfAnyKeywordCI(payload, findSQLKeywords)
	if idx < 0 {
		return "", false
	}

	// Found a keyword — extract until we hit a byte statement text cannot
	// contain (the TTC framing that follows it) or the end of the payload.
	end := idx
	for end < len(payload) && isPrintableSQLByte(payload[end]) {
		end++
	}

	if end <= idx+2 {
		return "", false
	}

	// The keyword may be a word in a comment rather than the statement's verb.
	// When it is, re-anchor on the comment's opener so the text handed on is
	// the run the server would read, and never one that begins mid-comment.
	if start := commentOpenerBefore(payload, idx); start >= 0 {
		if text, ok := sanitizeSQLRun(string(payload[start:end])); ok {
			trimmed := strings.TrimSpace(text)
			// Only when the re-anchored run actually carries a statement.
			// A run that is comment all the way down falls back to the reading
			// below, which is refused: the scan vouches for nothing here, and
			// widening it into a fail-open would make a keyword in a comment
			// the way to get a frame past the gate.
			if skipLeadingSQLComments(trimmed) != "" {
				return trimmed, end == len(payload)
			}
		}
	}

	text, ok := sanitizeSQLRun(string(payload[idx:end]))
	if !ok {
		return "", false
	}

	return strings.TrimSpace(text), end == len(payload)
}

// commentOpenerBefore reports where the SQL comment enclosing idx opens, or -1
// when idx is not inside one.
//
// It is the correction for the last-resort scan's one structural blind spot:
// the scan matches a *word*, and `-- MERGE s'execute` is a French comment, not
// a MERGE. Read from the keyword the `--` is gone, so the apostrophe two words
// later opens a quoted run that never closes and the statement is refused as
// unreadable — which is what a production Abyla session hit on 2026-09-01, on
// a comment line that had nothing to do with the statement below it.
//
// The search is bounded to the printable run the keyword sits in — the same run
// the scan would extract — because a `--` on the far side of TTC framing bytes
// is not a comment over this text. Within that run the rules are the server's:
// a `--` runs to the end of its line, a slash-star to its closing star-slash.
// The opener also has to be free-standing (run start, or behind a byte that
// cannot be part of an identifier), so `A--B` in an expression is not read as
// one.
//
// Deliberately *not* a backward extension to the start of the printable run:
// the 2026-08 extraction survey measured that swallowing whatever precedes the
// text pulls in length-prefix bytes that are themselves printable (a space is
// 32, `T` is 84), which is the misread the header-anchored decode exists to
// remove. Anchoring on a comment opener moves the start to a byte that is
// statement text by construction.
func commentOpenerBefore(payload []byte, idx int) int {
	runStart := idx
	for runStart > 0 && isPrintableSQLByte(payload[runStart-1]) {
		runStart--
	}

	for i := runStart; i+1 < idx; i++ {
		if !freeStandingCommentOpener(payload, runStart, i) {
			continue
		}

		switch {
		case payload[i] == '-' && payload[i+1] == '-':
			// A line comment ends at its newline, so it encloses idx only when
			// no newline separates them.
			if bytes.IndexByte(payload[i:idx], '\n') < 0 {
				return i
			}

			i++

		case payload[i] == '/' && payload[i+1] == '*':
			// A block comment encloses idx only when it is still open there.
			closer := bytes.Index(payload[i+2:idx], []byte("*/"))
			if closer < 0 {
				return i
			}

			i += 2 + closer + 1
		}
	}

	return -1
}

// freeStandingCommentOpener reports whether the two bytes at i open a comment
// rather than continue an identifier or an operator — `x--y` and `a/*` behind a
// word byte are not comment openers dbbat should anchor on.
func freeStandingCommentOpener(payload []byte, runStart, i int) bool {
	return i == runStart || !isSQLWordByte(payload[i-1])
}

// indexOfAnyKeywordCI returns the offset of the earliest case-insensitive match
// of any keyword in payload that ends at a word boundary, or -1. Used to locate
// the SQL statement inside an exec message whose framing varies by client.
//
// The boundary requirement is not cosmetic. Without it `GRANT` matched the
// `GRANTED_ROLE` column in DBeaver's own privilege probe and `DELETE` matched
// `DELETE_RULE`, so the gate enforced against — and /queries recorded — a
// fragment starting in the middle of a column name.
func indexOfAnyKeywordCI(payload []byte, keywords [][]byte) int {
	for i := range payload {
		for _, kw := range keywords {
			if i+len(kw) > len(payload) {
				continue
			}

			if !equalFoldASCIIBytes(payload[i:i+len(kw)], kw) {
				continue
			}

			if i+len(kw) < len(payload) && isSQLWordByte(payload[i+len(kw)]) {
				continue
			}

			return i
		}
	}

	return -1
}

// isSQLWordByte reports whether c can appear inside an Oracle identifier, which
// is what makes a keyword match a word rather than a prefix of one.
func isSQLWordByte(c byte) bool {
	return c == '_' || c == '$' || c == '#' ||
		(c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// equalFoldASCIIBytes reports whether a and b are equal ignoring ASCII letter case.
func equalFoldASCIIBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}

		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}

		if ca != cb {
			return false
		}
	}

	return true
}

// extractSQLAtOffset tries to read a length-prefixed SQL string at the given
// offset, returning the text to store.
func extractSQLAtOffset(data []byte, offset int) (string, error) {
	stmt, err := extractSQLAtOffsetText(data, offset)

	return stmt.Text, err
}

// extractSQLAtOffsetText is extractSQLAtOffset keeping the verbatim run too, so
// bind capture can still find the statement back in the payload by byte
// comparison after repair — see extractPiggybackBinds.
func extractSQLAtOffsetText(data []byte, offset int) (execStatement, error) {
	if offset >= len(data) {
		return execStatement{}, ErrOALL8TooShort
	}

	sqlLen, bytesRead, err := decodeVarLen(data[offset:])
	if err != nil || sqlLen == 0 || sqlLen > 32768 {
		return execStatement{}, ErrEmptySQL
	}

	sqlStart := offset + bytesRead
	sqlEnd := sqlStart + int(sqlLen)

	// The declared run does not fit what is in hand — the offset-window
	// equivalent of the fragmentation reassembly.go exists for. Unlike
	// findSQLInPayload this path has a length to check against, so it hands back
	// *nothing* rather than a prefix: there is no truncated reading for a caller
	// to mark, and the window scan simply moves on.
	if sqlEnd > len(data) {
		return execStatement{}, ErrSQLLengthInvalid
	}

	raw := string(data[sqlStart:sqlEnd])

	// Statement text is text. Without this a declared run that opens with a
	// verb and then turns into TTC framing bytes — `SET CURRENT_SCHEMA=TESTADM`
	// followed by four 0x01s was the measured case — passed as a statement.
	// sanitizeSQLRun is charitable about the session charset and strict about
	// binary; it also returns the text repaired for storage.
	sqlText, ok := sanitizeSQLRun(raw)
	if !ok {
		return execStatement{}, ErrEmptySQL
	}

	// Validate that it looks like SQL (opens with a statement verb)
	if !looksLikeSQL(sqlText) {
		return execStatement{}, ErrEmptySQL
	}

	return execStatement{Text: sqlText, Raw: raw}, nil
}

// QueryResultV2 contains parsed data from a v315+ TTC QueryResult (func=0x10).
type QueryResultV2 struct {
	Columns []string
	// ColumnTypes holds the TTC type code per column when it is known from the
	// describe records (nil otherwise — values are then decoded heuristically).
	ColumnTypes []int
	Rows        [][]string
	NoData      bool // true if ORA-01403 (normal end-of-data)
}

// decodeQueryResultV2 extracts column names and row values from a v315+
// QueryResult (func=0x10) payload. Uses a scanning approach since the
// exact binary format has many variable-length fields.
//
// Strategy:
//  1. Scan for column names: length-prefixed uppercase ASCII strings
//     in the first half of the payload (column definition area)
//  2. Scan for row values: length-prefixed data after the column area
//  3. Detect ORA-01403 as end-of-data (not an error)
func decodeQueryResultV2(ttcPayload []byte, shape oerShape, lob lobRowShape) *QueryResultV2 {
	if len(ttcPayload) < 20 {
		return nil
	}

	result := &QueryResultV2{}

	// Check for ORA-01403 (no data found) — this is a normal end-of-data marker
	if idx := findBytes(ttcPayload, []byte("ORA-01403")); idx >= 0 {
		result.NoData = true
	}

	// Phase 1: Column names. Prefer the describe column-definition records, which
	// give the real names (including single-char and unnamed-expression columns
	// the heuristic scanner misses) and the authoritative count. Fall back to
	// scanning + padding when the records don't parse (e.g. an unexpected server
	// layout) so behavior never regresses.
	//
	// `shape` is the session's learned encoding, so an OCI session reads its
	// real records here instead of the scanner's guesses — whichever of the two
	// OCI dialects it speaks.
	// Turning that on is not cosmetic and was measured rather than reasoned
	// about: a session whose describes parse learns its columns, which puts it in
	// a **row stream** over packets it used to walk straight past — and in the
	// corpus seven of those packets lead with the 0x04 of the object that *ends
	// an OCI fetch*, ORA-01403 at byte 0 of its own packet. The flat mid-stream
	// refusal that used to guard against them was drawn when no OCI session ever
	// had a row stream open, so it measured an empty set; what replaces it is the
	// end-of-data discriminator in session.statusOERMayEndTheCall, which the
	// corpus separates cleanly from the 149 running-count objects that really do
	// travel inside the stream.
	if descs := parseColumnDescribes(ttcPayload, shape); descs != nil {
		result.Columns = describeColumnNames(descs)
		result.ColumnTypes = describeColumnTypes(descs)
	} else {
		result.Columns = scanAndPadColumnNames(ttcPayload)
	}

	if len(result.Columns) == 0 {
		return result
	}

	// Phase 2: Find row values
	// Row values appear after the column definitions. We look for a marker
	// pattern that separates column defs from row data.
	// The row data area starts roughly after the column definitions.
	result.Rows = scanRowValues(ttcPayload, len(result.Columns), result.ColumnTypes, shape, lob)

	return result
}

// describeColumnTypes extracts the TTC type code per column from parsed describe
// records, for type-aware value decoding.
func describeColumnTypes(descs []columnDesc) []int {
	types := make([]int, len(descs))
	for i, d := range descs {
		types[i] = d.Type
	}

	return types
}

// describeColumnNames maps parsed describe records to column-name labels,
// substituting COLn for the unnamed-expression columns that carry no name.
func describeColumnNames(descs []columnDesc) []string {
	names := make([]string, len(descs))
	for i, d := range descs {
		if d.Name != "" {
			names[i] = d.Name
		} else {
			names[i] = fmt.Sprintf("COL%d", i+1)
		}
	}

	return names
}

// scanAndPadColumnNames is the fallback column-name source when the describe
// records don't parse: scan the column-definition area for names, then pad up to
// the describe-header count with synthetic COLn names so the row stream is still
// framed with the correct column count.
func scanAndPadColumnNames(ttcPayload []byte) []string {
	// Column names appear in the area BEFORE the 0x06 0x22 row data marker.
	columnArea := ttcPayload
	if markerIdx := findBytes(ttcPayload, []byte{0x06, 0x22}); markerIdx > 0 {
		columnArea = ttcPayload[:markerIdx]
	}

	names := scanColumnNames(columnArea)

	if n, ok := describeColumnCount(ttcPayload); ok && n > len(names) {
		for i := len(names); i < n; i++ {
			names = append(names, fmt.Sprintf("COL%d", i+1))
		}
	}

	return names
}

// describeColumnCount reads the authoritative column count from a v315 describe
// message (TTC func 0x10), whose header is:
//
//	[0x10] [size] [size bytes] [maxRowSize: compressed int] [colCount: compressed int]
//
// Returns false if the payload is not a describe header or the count is out of a
// sane range, in which case callers fall back to the scanned column names.
func describeColumnCount(ttcPayload []byte) (int, bool) {
	count, _, ok := describeColumnLayout(ttcPayload)
	if !ok || count <= 0 || count > 1000 {
		return 0, false
	}

	return count, true
}

// scanColumnNames finds length-prefixed column names in the payload.
// Column names in Oracle are uppercase ASCII identifiers.
func scanColumnNames(data []byte) []string {
	var columns []string
	i := 30 // Skip the header area

	for i < len(data)-1 {
		nameLen := int(data[i])
		if nameLen < 1 || nameLen > 128 || i+1+nameLen > len(data) {
			i++
			continue
		}

		candidate := data[i+1 : i+1+nameLen]
		if isOracleColumnName(candidate) {
			columns = append(columns, string(candidate))
			i += 1 + nameLen
			// Skip past column metadata (type info, etc.) — at least a few bytes
			i += skipColumnMetadata(data[i:])
		} else {
			i++
		}
	}

	return columns
}

// isOracleColumnName checks if bytes look like an Oracle column name.
// Column names are uppercase ASCII with letters, digits, underscores, $, #.
// Minimum 2 chars to avoid false positives from random bytes.
func isOracleColumnName(b []byte) bool {
	if len(b) < 2 || len(b) > 128 {
		return false
	}

	// First char must be a letter
	if !isUpperLetter(b[0]) {
		return false
	}

	for _, c := range b {
		if isUpperLetter(c) || (c >= '0' && c <= '9') || c == '_' || c == '$' || c == '#' {
			continue
		}

		return false
	}

	return true
}

func isUpperLetter(c byte) bool {
	return c >= 'A' && c <= 'Z'
}

// skipColumnMetadata skips past the metadata bytes following a column name.
// Returns the number of bytes to skip.
func skipColumnMetadata(data []byte) int {
	// Column metadata includes type code, size, precision, scale, nullable flag, etc.
	// These are variable-length but typically 10-30 bytes.
	// We scan forward looking for the next length-prefixed column name or the row data marker.
	for i := 0; i < min(40, len(data)); i++ {
		if i+1 < len(data) {
			nameLen := int(data[i])
			if nameLen >= 1 && nameLen <= 128 && i+1+nameLen <= len(data) {
				if isOracleColumnName(data[i+1 : i+1+nameLen]) {
					return i
				}
			}
		}
	}

	return 0
}

// scanRowValues extracts row values from a QRESULT (func=0x10) payload. The row
// area uses the same compressed encoding as continuation packets — length-
// prefixed values for each active column, 0x07 / 0x15 descriptors between rows,
// terminated by the 0x08 footer or an ORA-01403 marker — so it delegates to the
// shared parseRowStream. The first QRESULT row carries every column.
//
// The values themselves are encoding-independent — a one-byte length then that
// many raw bytes, in all three dialects — but the ROW_HEADER object that
// introduces them is not, so `shape` (the session's learned encoding, never
// anything sniffed from the payload) decides how the stream is found. See
// rowDataStart.
func scanRowValues(data []byte, numCols int, colTypes []int, shape oerShape, lob lobRowShape) [][]string {
	rowStart := rowDataStart(data, numCols, shape)
	if rowStart < 0 || numCols == 0 {
		return nil
	}

	rows := parseRowStream(data, rowStart, numCols, allColumns(numCols), nil, colTypes, shape, lob)

	out := make([][]string, len(rows))
	for i, row := range rows {
		strRow := make([]string, numCols)
		for j, v := range row {
			if s, ok := v.(string); ok {
				strRow[j] = s
			}
		}

		out[i] = strRow
	}

	return out
}

// ttcMsgRowHeader is go-ora's `case 6`: the object a fetch sends ahead of its
// values, naming how many columns each row carries. The message that follows it
// is 0x07 — ttcMsgBindOutput, which is the same block under the name the
// bind-output walk knows it by.
const ttcMsgRowHeader = 0x06

// The 64-bit OCI dialect's ROW_HEADER, measured rather than inferred — see
// wide64RowDataStartAt for what each constant is pinned by.
const (
	wide64RowHeaderFlagOffset  = 2
	wide64RowHeaderFlag        = 0x22
	wide64RowHeaderCountOffset = 4
	wide64RowHeaderLen         = 50
)

// The 4-byte OCI dialect's ROW_HEADER, measured the same way — see
// wideRowDataStartAt.
const (
	wideRowHeaderFlagOffset  = 1
	wideRowHeaderFlag        = 0x22
	wideRowHeaderCountOffset = 2
	wideRowHeaderLen         = 22
)

// compressedRowHeaderInts is how many TTC compressed integers stand between the
// compressed dialect's 0x22 flag and its ROW_DATA byte — see
// compressedRowDataStartAt. The header has no fixed length there, because those
// integers are self-sizing, so it is walked rather than measured.
const compressedRowHeaderInts = 6

// compressedRowHeaderCountUnit is the multiplier on the compressed header's
// second integer, which carries the column count's high part: the count field
// itself is a two-byte one, so a select list past 255 columns spills into it.
// Every sample in the corpus has it zero (the widest is 45 columns), so this is
// the one constant here the recordings do not exercise.
const compressedRowHeaderCountUnit = 0x100

// rowDataStart locates the first row value of a fetch, in whichever encoding the
// session speaks. The row area itself is the same in all three (parseRowStream);
// what differs is the ROW_HEADER object standing in front of it.
//
// The header is found by validating one at each candidate offset, never by
// scanning forward from it for the ROW_DATA byte. That distinction is the whole
// of specs/todos/2026-09-22-06: a forward scan reads the header's own column
// count as the `0x07` that opens the rows whenever a query has exactly seven
// columns, on both dialects that encode seven as the single byte 0x07.
func rowDataStart(data []byte, numCols int, shape oerShape) int {
	for i := 0; i+1 < len(data); i++ {
		if data[i] != ttcMsgRowHeader {
			continue
		}

		if start := rowHeaderRowDataStart(data, i, numCols, shape); start >= 0 {
			return start
		}
	}

	return -1
}

// rowHeaderRowDataStart reads the ROW_HEADER that must begin at data[at] in the
// encoding the session speaks, and returns the offset of the first row value —
// or -1 when what is there is not that dialect's header for a numCols-column
// fetch.
//
// All three readings fail closed the same way: the header's own column count
// must be the describe's, and the ROW_DATA byte must land exactly where the
// header ends.
func rowHeaderRowDataStart(data []byte, at, numCols int, shape oerShape) int {
	if numCols <= 0 || at < 0 || at >= len(data) || data[at] != ttcMsgRowHeader {
		return -1
	}

	switch {
	case shape.fixedWidth64:
		return wide64RowDataStartAt(data, at, numCols)
	case shape.fixedWidth:
		return wideRowDataStartAt(data, at, numCols)
	default:
		return compressedRowDataStartAt(data, at, numCols)
	}
}

// fetchRowDataStart locates the row values of a fetch response that arrives in
// a packet of its own — one whose TTC payload *opens* with the ROW_HEADER
// rather than embedding it behind a describe.
//
// It is rowDataStart with one extra demand: the header must sit at offset 0.
// rowDataStart tries every offset, which is right in a QueryResult (the header
// follows the column records) and wrong here, where it would let a mid-stream
// continuation packet supply a header-shaped run of bytes from inside its own
// row data. A fetch response leads with the object or it is not one.
func fetchRowDataStart(data []byte, numCols int, shape oerShape) int {
	return rowHeaderRowDataStart(data, 0, numCols, shape)
}

// wide64RowDataStartAt reads a **64-bit** OCI fetch's ROW_HEADER at data[at].
//
// It exists because the compressed reading finds nothing at all on this dialect,
// which is why a 64-bit OCI session captured rows that were empty JSON objects
// even after its describes became readable: the two-byte `06 22` marker that
// reading keys on is `06 01 22 xx` here, and no `06 22` pair occurs anywhere in
// the payload. The values behind the header are byte-for-byte the same shape as
// the 4-byte dialect's — a one-byte length then the raw bytes, which is what let
// both fixtures be compared value by value.
//
// The header is measured off testdata/oci64_describe.hex, from the two frames
// that carry one, against their counterparts in testdata/oci_describe.hex. The
// column count is the only field read, and it is the only one either recording
// varies (1 on the login probe, 8 on the rich query), so it is also the only one
// that could be pinned at all:
//
//	         | 4-byte dialect      | 64-bit dialect
//	+0       | 0x06 ROW_HEADER     | 0x06 ROW_HEADER
//	+1       | 0x22 flag           | 0x01
//	+2       | ub4 column count    | 0x22 flag
//	+3       |                     | padding, stale (0xaf / 0x59)
//	+4       |                     | ub4 column count
//	+6/+8    | ub2 = 0, ub2 = rows | ub8 = 0x10000
//	+10/+16  | 3 × ub4 = 0         | 4 × ub8, then a ub2 = 0
//	+22/+50  | 0x07 ROW_DATA       | 0x07 ROW_DATA
//
// What says the wider tail is that same field list at 64-bit widths, rather than
// a guess that happens to total 50: two of those four ub8 slots carry a non-zero
// **upper** half (0xffffa5a9…, 0xffffa382… / 0x0001a382…) over a zero lower half,
// in both frames and at the same two offsets. That is what a 64-bit struct looks
// like when the server writes a 32-bit value into it and leaves the top half
// stale — and it is also why nothing here reads those slots. Their low halves
// are zero in every sample, so the corpus says nothing about what they mean.
//
// The reading fails closed, which is the behavior the dialect had before it
// existed: the count must be the describe's own, and the ROW_DATA byte must land
// exactly where the header ends. Nothing is scanned for. Across every frame of
// every .hex fixture in the corpus that pattern matches exactly twice — the two
// 64-bit headers — and never on a 4-byte or compressed payload, so a session
// offered the wrong reading gets no rows rather than plausible-looking ones.
func wide64RowDataStartAt(data []byte, at, numCols int) int {
	if at+wide64RowHeaderLen >= len(data) {
		return -1
	}

	if data[at+wide64RowHeaderFlagOffset] != wide64RowHeaderFlag {
		return -1
	}

	if data[at+wide64RowHeaderLen] != ttcMsgBindOutput {
		return -1
	}

	count := binary.LittleEndian.Uint32(data[at+wide64RowHeaderCountOffset : at+wide64RowHeaderCountOffset+4])
	if int(count) != numCols {
		return -1
	}

	return at + wide64RowHeaderLen + 1
}

// wideRowDataStartAt reads a **4-byte** OCI fetch's ROW_HEADER at data[at].
//
// The header is a measured 22 bytes, pinned the same way the 64-bit one was and
// off the same recordings: the two frames of testdata/oci_describe.hex that
// carry a header (column counts 1 and 8) and the four in sqlplus_*.pcapng
// (counts 1, 2 and 3). The layout is the table above, read down the left-hand
// column — `0x06`, the `0x22` flag, a ub4 column count, then a ub4 and three
// more, all of which are 0 except the second half of the first (the fetch's
// array size: 1 on both describe frames, 15 on the REF-cursor recording, which
// is what says that slot is a field rather than padding). Then `0x07` at +22.
//
// It replaces a forward scan for that `0x07`, and the difference is a live bug
// rather than a tidy-up: the byte the scan started on is the low byte of the
// column count, so a seven-column fetch's own count *is* the `0x07` and the scan
// returned +3 instead of +23, handing parseRowStream the middle of the header.
// The count check is what makes the measured reading fail closed instead, the
// way the 64-bit one does.
func wideRowDataStartAt(data []byte, at, numCols int) int {
	if at+wideRowHeaderLen >= len(data) {
		return -1
	}

	if data[at+wideRowHeaderFlagOffset] != wideRowHeaderFlag {
		return -1
	}

	if data[at+wideRowHeaderLen] != ttcMsgBindOutput {
		return -1
	}

	count := binary.LittleEndian.Uint32(data[at+wideRowHeaderCountOffset : at+wideRowHeaderCountOffset+4])
	if int(count) != numCols {
		return -1
	}

	return at + wideRowHeaderLen + 1
}

// compressedRowDataStartAt reads a **compressed/thin** fetch's ROW_HEADER at
// data[at] — go-ora, python-oracledb thin, the JDBC thin driver, DBeaver.
//
// This dialect's header has no fixed length, because its integers are TTC
// compressed ones that carry their own width, so it is **walked** rather than
// measured: `0x06`, the `0x22` flag, then six compressed integers, then the
// `0x07` that opens the rows. The field list is the 4-byte dialect's, which is
// what says six is the count rather than a number that happened to fit —
// counting the ub2 pair at +6 separately, that reading has six integers behind
// its flag too.
//
// Pinned across every thin recording in testdata/ that carries a header
// (go_ora*, python_thin*, jdbc_thin*, dbeaver*, ojdbc6_legacy), at column counts
// 1, 2, 3, 4, 6, 9, 15, 35 and 45. Only two of the six fields ever vary: the
// count, and the third one, which is the client's prefetch size (10 for the JDBC
// thin driver, 25 and 1000 for go-ora, 2 and 100 for python-oracledb). The other
// four are a single `0x00` byte in every sample, so whether they are integers or
// bare bytes is not something the corpus can say — they are walked as integers
// because the 4-byte dialect spells the same tail as three ub4s, and because a
// compressed walk of a zero byte consumes exactly the one byte either way.
//
// Fails closed like the other two: the walk must land the `0x07` exactly, and
// the count it read must be the describe's. That is what keeps it off the
// `06 22` pairs that occur *inside* row data — the midfetch recordings carry
// several — which the forward scan it replaces would happily have followed.
func compressedRowDataStartAt(data []byte, at, numCols int) int {
	if at+1 >= len(data) || data[at+1] != wideRowHeaderFlag {
		return -1
	}

	pos, count, high := at+2, 0, 0

	for field := range compressedRowHeaderInts {
		val, n := readCompressedInt(data[pos:])
		if n == 0 {
			return -1
		}

		switch field {
		case 0:
			count = val
		case 1:
			high = val
		}

		pos += n
	}

	if pos >= len(data) || data[pos] != ttcMsgBindOutput {
		return -1
	}

	if count+high*compressedRowHeaderCountUnit != numCols {
		return -1
	}

	return pos + 1
}

// decodeOracleRawValue converts raw Oracle bytes to a readable string.
func decodeOracleRawValue(b []byte) string {
	// Try as readable ASCII first
	if isReadableASCII(b) {
		return string(b)
	}

	// Try as Oracle DATE (7 bytes: century, year, month, day, hour, min, sec)
	if dt, ok := decodeOracleDateToString(b); ok {
		return dt
	}

	// Try as Oracle TIMESTAMP (11 bytes) or TIMESTAMP WITH TIME ZONE (13 bytes)
	if ts, ok := decodeOracleTimestampToString(b); ok {
		return ts
	}

	// Try as Oracle NUMBER
	if num, ok := decodeOracleNumberToString(b); ok {
		return num
	}

	// Fallback: hex representation
	return hex.EncodeToString(b)
}

// decodeOracleDateToString converts Oracle DATE format (7 bytes) to ISO string.
// Format: [century] [year] [month] [day] [hour+1] [minute+1] [second+1]
// Century and year: (century-100)*100 + (year-100) = actual year
func decodeOracleDateToString(b []byte) (string, bool) {
	if len(b) != 7 {
		return "", false
	}

	century := int(b[0])
	year := int(b[1])
	month := int(b[2])
	day := int(b[3])
	hour := int(b[4]) - 1
	minute := int(b[5]) - 1
	second := int(b[6]) - 1

	// Sanity checks
	if century < 100 || century > 200 || year < 100 || year > 200 {
		return "", false
	}

	if month < 1 || month > 12 || day < 1 || day > 31 {
		return "", false
	}

	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return "", false
	}

	fullYear := (century-100)*100 + (year - 100)

	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", fullYear, month, day, hour, minute, second), true
}

// decodeOracleTimestampToString converts Oracle TIMESTAMP (11 bytes) or
// TIMESTAMP WITH TIME ZONE (13 bytes) to a readable string.
//
// Bytes 0-6 are the DATE portion (UTC wall clock), bytes 7-10 are fractional
// seconds as a big-endian nanosecond count. For the 13-byte tz form, bytes
// 11-12 carry the zone: a numeric offset (tzHour = (b[11]&0x3f)-20,
// tzMin = b[12]-60) when b[11]'s high bit is clear, or a named-region id (not
// resolvable to a numeric offset here) when it is set — region values decode to
// the UTC wall clock without an offset suffix.
func decodeOracleTimestampToString(b []byte) (string, bool) {
	if len(b) != 11 && len(b) != 13 {
		return "", false
	}

	nanos := int(binary.BigEndian.Uint32(b[7:11]))

	t, ok := parseOracleDateTimePrefix(b[:7], nanos)
	if !ok {
		return "", false
	}

	// 13-byte form with a numeric offset: render the original local wall clock
	// plus the offset suffix. Byte 11's low 6 bits are the hour; bit 0x80 (above)
	// marks a named region; bit 0x40 is the "time in zone" flag — when set the
	// 7-byte prefix is already the local wall clock, otherwise it is UTC and is
	// shifted into the zone to recover the local time.
	if len(b) == 13 && b[11]&0x80 == 0 {
		offsetSec := (int(b[11]&0x3f)-20)*3600 + (int(b[12])-60)*60
		if offsetSec < -15*3600 || offsetSec > 15*3600 {
			return "", false
		}

		zone := time.FixedZone("", offsetSec)

		local := t.In(zone)
		if b[11]&0x40 != 0 {
			local = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), zone)
		}

		return local.Format("2006-01-02 15:04:05.999999999 -07:00"), true
	}

	return t.Format("2006-01-02 15:04:05.999999999"), true
}

// parseOracleDateTimePrefix validates and decodes the 7-byte
// century/year/month/day/hour/min/sec prefix shared by Oracle DATE and
// TIMESTAMP values, returning the UTC wall clock with the supplied nanoseconds.
// ok is false when any field is out of range, which lets heuristic callers
// reject non-temporal byte runs.
func parseOracleDateTimePrefix(b []byte, nanos int) (time.Time, bool) {
	century := int(b[0])
	year := int(b[1])
	month := int(b[2])
	day := int(b[3])
	hour := int(b[4]) - 1
	minute := int(b[5]) - 1
	second := int(b[6]) - 1

	if century < 100 || century > 200 || year < 100 || year > 200 {
		return time.Time{}, false
	}

	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, false
	}

	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return time.Time{}, false
	}

	fullYear := (century-100)*100 + (year - 100)

	return time.Date(fullYear, time.Month(month), day, hour, minute, second, nanos, time.UTC), true
}

// isReadableASCII checks if all bytes are printable ASCII.
func isReadableASCII(b []byte) bool {
	if len(b) == 0 {
		return false
	}

	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}

	return true
}

// decodeOracleNumberToString decodes an Oracle NUMBER for the heuristic
// row-capture path, where no column type is available. It gates on
// isOracleNumber so genuine text (which carries no type tag on the wire) is
// never misread as a number, then formats with the shared formatOracleNumber.
//
// NOTE: a negative NUMBER whose bytes happen to all be printable ASCII is
// indistinguishable from text here; decodeOracleRawValue tries ASCII first, so
// such values are captured as strings.
func decodeOracleNumberToString(b []byte) (string, bool) {
	if len(b) == 1 && b[0] == 0x80 {
		return "0", true
	}

	if !isOracleNumber(b) {
		return "", false
	}

	return formatOracleNumber(b)
}

// formatOracleNumber reconstructs the exact decimal string of an Oracle NUMBER
// from its raw bytes: one exponent byte then up to 20 base-100 mantissa bytes
// (negatives carry a trailing 0x66 terminator). The value is
// sign × mantissa × 100^(exp100 - n + 1), where exp100 is the signed base-100
// exponent of the most significant digit and the mantissa is the n base-100
// digits laid out two decimal places each.
//
// It performs no validity gating — callers without a column type must pre-check
// with isOracleNumber. ok is false only for empty or degenerate input.
func formatOracleNumber(b []byte) (string, bool) {
	if len(b) == 0 {
		return "", false
	}

	if len(b) == 1 && b[0] == 0x80 {
		return "0", true
	}

	positive := b[0]&0x80 != 0

	var (
		exp100 int
		digits []int
	)

	if positive {
		exp100 = int(b[0]&0x7f) - 65
		for _, c := range b[1:] {
			digits = append(digits, int(c)-1)
		}
	} else {
		end := len(b)
		if b[end-1] == 0x66 { // strip the negative terminator (102)
			end--
		}

		exp100 = int((b[0]^0xff)&0x7f) - 65
		for _, c := range b[1:end] {
			digits = append(digits, 101-int(c))
		}
	}

	if len(digits) == 0 {
		return "", false
	}

	// Lay the base-100 digits out two decimal places each; the whole run then
	// represents mantissa × 100^(exp100 - len(digits) + 1).
	var mant strings.Builder
	for _, d := range digits {
		if d < 0 || d > 99 {
			return "", false
		}

		fmt.Fprintf(&mant, "%02d", d)
	}

	s := placeDecimalPoint(mant.String(), 2*(exp100-len(digits)+1))
	if !positive {
		s = "-" + s
	}

	return s, true
}

// isOracleNumber reports whether b is a valid Oracle NUMBER encoding. It mirrors
// the driver's validity rules so that text values (which carry no type tag on
// the wire) are not misread as numbers: positive mantissa bytes are 1..100 and
// negative ones 2..101, with a length and terminator check.
func isOracleNumber(b []byte) bool {
	n := len(b)
	if n < 2 || n > 21 {
		return false
	}

	if b[0]&0x80 != 0 { // positive
		if b[1] < 2 || b[n-1] < 2 {
			return false
		}

		for _, c := range b[1:] {
			if c < 1 || c > 100 {
				return false
			}
		}

		return true
	}

	// Negative: an optional 0x66 terminator, otherwise the full 20 mantissa bytes.
	end := n
	if b[n-1] == 0x66 {
		end--
	} else if n <= 20 {
		return false
	}

	if end < 2 || b[1] > 100 || b[end-1] > 100 {
		return false
	}

	for _, c := range b[1:end] {
		if c < 2 || c > 101 {
			return false
		}
	}

	return true
}

// placeDecimalPoint formats mantissa × 10^shift, trimming leading integer zeros
// and trailing fractional zeros (e.g. "0314",-2 → "3.14"; "50",-2 → "0.5").
func placeDecimalPoint(mant string, shift int) string {
	if shift >= 0 {
		mant += strings.Repeat("0", shift)
		if t := strings.TrimLeft(mant, "0"); t != "" {
			return t
		}

		return "0"
	}

	frac := -shift

	var intPart, fracPart string
	if len(mant) > frac {
		intPart, fracPart = mant[:len(mant)-frac], mant[len(mant)-frac:]
	} else {
		intPart, fracPart = "0", strings.Repeat("0", frac-len(mant))+mant
	}

	if intPart = strings.TrimLeft(intPart, "0"); intPart == "" {
		intPart = "0"
	}

	if fracPart = strings.TrimRight(fracPart, "0"); fracPart == "" {
		return intPart
	}

	return intPart + "." + fracPart
}

// continuationDescriptorMarker (0x15) appears after each row in a continuation
// packet. It is followed by a descriptor that encodes which columns have new
// values in the NEXT row: [flag] [count] [bitmask] then 0x07.
const continuationDescriptorMarker = 0x15

// parseContinuationRows decodes rows from a TTC continuation packet (func=0x06).
//
// A continuation packet is a header followed by the same compressed row stream
// as the QueryResult row area, so the row decoding itself lives in the shared
// parseRowStream. This function only locates the stream:
//   - The first 0x07 in the header marks the start of row data.
//   - The header bitmask (at header_end-2) selects the columns carried in the
//     first row; later rows are selected by their 0x15 descriptors.
//
// prevRow is the last row of the previous packet so unchanged (compressed-away)
// columns can be filled in.
func parseContinuationRows(
	payload []byte, numCols int, prevRow []string, colTypes []int, shape oerShape, lob lobRowShape,
) [][]interface{} {
	if numCols == 0 || len(payload) < 15 {
		return nil
	}

	// A packet that *opens* with a ROW_HEADER is not a continuation of a stream
	// already running: it is the fetch itself, arriving in a round trip of its
	// own. Oracle sends one whenever the select list carries a LOB — prefetch is
	// off for those — so the QueryResult that described the query carried no rows
	// at all and every row of the fetch is here. The header is the same object
	// rowDataStart reads at the head of a QueryResult, in whichever dialect the
	// session speaks, and none of it fits in the 25-byte window the scan below
	// searches (the 64-bit header alone is 50 bytes), which is why such a fetch
	// used to be read as carrying nothing.
	if start := fetchRowDataStart(payload, numCols, shape); start >= 0 {
		return parseRowStream(payload, start, numCols, allColumns(numCols), nil, colTypes, shape, lob)
	}

	// Find the first 0x07 in the header area (marks start of row data).
	headerEnd := -1
	for i := 1; i < 25 && i < len(payload); i++ {
		if payload[i] == 0x07 {
			headerEnd = i
			break
		}
	}

	if headerEnd < 0 {
		return nil
	}

	// Parse header bitmask to determine which columns are sent in the first row.
	// The bitmask is at headerEnd-2 (the byte before the trailing 0x00 before 0x07).
	activeCols := allColumns(numCols)
	if headerEnd >= 3 {
		bitmask := payload[headerEnd-2]
		if cols := bitmaskToColumns(bitmask, numCols); len(cols) > 0 {
			activeCols = cols
		}
	}

	// Carry forward the previous packet's last row for compressed-away columns.
	prev := make([]string, numCols)
	if len(prevRow) == numCols {
		copy(prev, prevRow)
	}

	return parseRowStream(payload, headerEnd+1, numCols, activeCols, prev, colTypes, shape, lob)
}

// decodeRowValue decodes a single captured column value by its TTC type. NUMBER
// uses formatOracleNumber directly (correct for negatives/fractionals the
// type-less heuristic mis-reads); BINARY_FLOAT/DOUBLE undo Oracle's sortable
// byte transform (which the heuristic can't decode at all); RAW renders as hex
// so binary content isn't mistaken for text. Every other type — and any column
// with no known type — falls through to decodeOracleRawValue, preserving the
// established string/temporal formats.
func decodeRowValue(colTypes []int, col int, b []byte) string {
	if col >= 0 && col < len(colTypes) {
		switch colTypes[col] {
		case tnsTypeNUMBER:
			if s, ok := formatOracleNumber(b); ok {
				return s
			}
		case tnsTypeBINFLOAT, tnsTypeBINDOUBLE:
			if s, ok := decodeOracleBinaryFloatString(b); ok {
				return s
			}
		case tnsTypeRAW, tnsTypeLONGRAW:
			// RAW is binary; render it as hex so printable byte runs aren't
			// mistaken for text by the ASCII-first heuristic.
			return hex.EncodeToString(b)
		}
	}

	return decodeOracleRawValue(b)
}

// rowValueShape says how a column's value is laid out in the row stream. Only
// the first is a length-prefixed datum; the other three carry bytes behind the
// value that the scalar walk has no business reading as the next column.
type rowValueShape int

const (
	// rowValueScalar is a length-prefixed value, the shape every column had
	// before LOBs and object types were looked at.
	rowValueScalar rowValueShape = iota

	// rowValueLOBLocator is a CLOB/NCLOB/BLOB/BFILE: a length-prefixed locator
	// and then a fixed block of LOB framing.
	rowValueLOBLocator

	// rowValueObjectImage is an opaque type (XMLTYPE) or a named object type: a
	// length-prefixed locator, then framing, then the object's own image.
	rowValueObjectImage

	// rowValueLongInline is a LONG or LONG RAW: the value as a CLR, then the
	// column's indicator and return code. See readInlineLongColumn.
	rowValueLongInline
)

// rowValueShapeOf reads a column's shape off the describe's type codes. With no
// type codes — the heuristic path — every column is a scalar, which is the
// behavior this package had throughout.
func rowValueShapeOf(colTypes []int, col int) rowValueShape {
	if col < 0 || col >= len(colTypes) {
		return rowValueScalar
	}

	switch colTypes[col] {
	case tnsTypeCLOB, tnsTypeBLOB, tnsTypeBFILE:
		return rowValueLOBLocator
	case tnsTypeOPAQUE, tnsTypeNamedObject:
		return rowValueObjectImage
	case tnsTypeLONG, tnsTypeLONGRAW:
		return rowValueLongInline
	}

	return rowValueScalar
}

// rowColumnIsFramed reports whether this column's reading steps over bytes a
// scalar walk would have read as the next column's value — which is both what
// picks the reading in readRowColumn and what arms parseRowStream's
// rowEndsAtMarker check.
//
// It takes the dialect because one of the four shapes is dialect-dependent: a
// LONG column is read inline on the thin dialect and left on the scalar reading
// on the two OCI ones, where nothing has been recorded. Asking the question in
// one place is what keeps the two from drifting — a column read as a scalar but
// counted as framed would hold an OCI row to a terminator it never had to land
// on before.
func rowColumnIsFramed(shape oerShape, colTypes []int, col int) bool {
	switch rowValueShapeOf(colTypes, col) {
	case rowValueLOBLocator, rowValueObjectImage:
		return true

	case rowValueLongInline:
		// Both OCI dialects frame their LOB columns their own way and no
		// recording says what they do with a LONG one, so they keep the reading
		// they have always had rather than inheriting a shape measured
		// somewhere else. See docs/oracle.md.
		return !shape.fixedWidth

	case rowValueScalar:
	}

	return false
}

// The LOB column's framing, and it is a *reading* rather than the pair of
// skip lengths it started as, because the three dialects spell it three ways.
//
// The first measurement had one recording to work from — sqlplus on the 64-bit
// OCI dialect, testdata/oci64_lob.hex — and turned it into "sixteen bytes after
// a locator, three after a NULL one". Recording the same query on the other two
// (testdata/oci_lob.hex, testdata/go_ora_lob.pcapng) says the sixteen were the
// sum of four fields, and that two of the three dialects do not add up to it:
//
//	64-bit OCI     maxSize ub4 LE · size ub8 LE · chunkSize ub4 LE · locator CLR
//	4-byte OCI     maxSize ub4 LE · size compressed · chunkSize ub4 LE · locator CLR
//	compressed     maxSize · (size · chunkSize) · locator CLR, all compressed
//	  or           content CLR · the column's indicator and return code
//
// So the 4-byte dialect's LOB column is six bytes shorter than the skip it was
// being given, and every row of such a fetch was refused — the same silent
// "no rows" the whole LOB reading exists to end, one dialect over.
//
// The two OCI dialects both spell the locator's length twice: as the ub4
// maxSize ahead of the header and as the CLR length byte behind it. Only a
// column where the two agree is read as a locator, which is what makes the skip
// a measurement rather than an arithmetic that happens to land (the same rule
// readObjectImage holds the object image's header to). A zero maxSize is the
// NULL LOB and ends the column on the spot — the four bytes the old
// lobNullTrailerLen counted with the length byte in front of it.
//
// The compressed dialect has **two** shapes rather than one, and the two are
// not near-misses: a client that re-declared its LOB columns as LONG gets the
// value in the row, a client that did not gets a locator. Nothing in the
// describe says which — the column records of the two recordings are identical
// — so the branch is decided off the client's own define block
// (execDefineLOBShape), and a session that stated nothing reads locators,
// because that is what the server sends when nothing was asked.
//
// None of it is load-bearing on its own: a wrong skip drifts the columns after
// it, and parseRowStream then refuses the whole row (rowEndsAtMarker) rather
// than capturing one that decoded into the framing.
const (
	// lobFixedMaxSizeLen and lobFixedChunkSizeLen are the two little-endian
	// ub4s both OCI dialects agree on.
	lobFixedMaxSizeLen   = 4
	lobFixedChunkSizeLen = 4

	// lobFixedSize64Len is the ub8 between them on the 64-bit dialect. The
	// 4-byte one sends a TTC compressed integer there instead, which is the
	// whole of the difference between them.
	lobFixedSize64Len = 8

	// inlineLongTrailingInts is how many compressed integers follow the value of
	// a column the server sent inline on the thin dialect — a LONG, a LONG RAW,
	// or a LOB a client re-declared as one: the column's indicator and its
	// return code.
	inlineLongTrailingInts = 2
)

// skipCompressedInt steps over a TTC compressed integer at payload[offset] and
// returns the offset behind it.
//
// It is a skip rather than a read because nothing here needs the value, and
// because the one field it is used on that carries a sign — the thin dialect's
// NULL LOB sends 0x81 0x01, a one-byte -1 — is a field readCompressedInt
// refuses outright (it reads the 0x80 flag as a length of 129).
func skipCompressedInt(payload []byte, offset int) (int, bool) {
	if offset >= len(payload) {
		return 0, false
	}

	size := int(payload[offset]) & 0x7F
	if size > 8 {
		return 0, false
	}

	next := offset + 1 + size
	if next > len(payload) {
		return 0, false
	}

	return next, true
}

// readFixedLOBColumn reads a LOB column on either OCI dialect: the header
// above, then the locator, whose length has to be the one the header already
// gave. The captured value is the placeholder — the contents are not in this
// packet and dbbat will not go and ask for them (see lobLocatorPlaceholder).
func readFixedLOBColumn(
	payload []byte, offset int, shape oerShape, colTypes []int, col int,
) (string, int, bool) {
	if offset+lobFixedMaxSizeLen > len(payload) {
		return "", 0, false
	}

	maxSize := int(binary.LittleEndian.Uint32(payload[offset : offset+lobFixedMaxSizeLen]))
	offset += lobFixedMaxSizeLen

	if maxSize == 0 {
		return "", offset, true // a NULL LOB: the column is its zero maxSize and nothing else
	}

	if shape.fixedWidth64 {
		offset += lobFixedSize64Len
	} else {
		next, ok := skipCompressedInt(payload, offset)
		if !ok {
			return "", 0, false
		}

		offset = next
	}

	offset += lobFixedChunkSizeLen
	if offset >= len(payload) {
		return "", 0, false
	}

	locatorLen := int(payload[offset])
	offset++

	if locatorLen != maxSize || offset+locatorLen > len(payload) {
		return "", 0, false
	}

	return lobLocatorPlaceholder(colTypes, col), offset + locatorLen, true
}

// readCompressedLOBColumn reads a LOB column on the thin dialect, under the
// reading this session's client asked for.
//
// The two shapes are not each other's near-misses, they are unrelated: one is a
// LONG column's value, the other a locator with its sizes in front of it. And
// the bytes do not say which — the column records of the two are identical,
// because the ask was made on the client's side of the wire. So the branch is
// decided by execDefineLOBShape, off the client's own define block, and never
// by trying one reading and falling back to the other.
//
// lobRowLocator is the branch a session that asked for nothing gets, because a
// locator is what Oracle sends unless a client re-declared the column as a
// LONG. A define dbbat could not walk therefore lands here too, where a wrong
// walk costs the row rather than filling it with framing bytes.
func readCompressedLOBColumn(
	payload []byte, offset int, shape oerShape, colTypes []int, col int, lob lobRowShape,
) (string, int, bool) {
	if lob == lobRowInline {
		return readInlineLongColumn(payload, offset, shape, colTypes, col)
	}

	return readCompressedLOBLocatorColumn(payload, offset, colTypes, col)
}

// lobCompressedLocatorMaxSkips is how many compressed integers may sit between
// a thin locator column's leading size and the locator itself.
//
// Two recordings, two answers: go-ora's `lob fetch=post` sends the size and
// then the locator with nothing in between (testdata/go_ora_lob_stream.pcapng),
// while python-oracledb thin sends the LOB's own size and its chunk size as
// well (testdata/python_thin_lob.pcapng) — which is the OCI dialects' four-field
// header spelled in compressed integers. The walk steps over them rather than
// counting them, and it is the size agreeing with the locator's own length that
// says where it stopped, so a third client adding or dropping one of the two
// costs nothing. Two is the measured maximum and the bound that keeps a walk
// which has drifted from wandering into the next column.
const lobCompressedLocatorMaxSkips = 2

// readCompressedLOBLocatorColumn reads a LOB column on the thin dialect when
// the row carries a locator: a leading size as a compressed integer, then the
// LOB's own size and chunk size on the clients that send them, then the locator
// as a CLR.
//
// The reading validates itself the way readFixedLOBColumn's does — the locator
// length is spelled **twice**, once as the leading size and once as the CLR's
// own length byte, and only a column where the two agree is read as a locator.
// That is also what locates the CLR: the walk steps forward over compressed
// integers until the byte it is looking at is that size and that many bytes are
// there to be had.
//
// A zero leading size is the NULL LOB and ends the column there, exactly as it
// does on both OCI dialects. The captured value is the placeholder — the
// contents are not in this packet and dbbat will not go and ask for them (see
// lobLocatorPlaceholder).
func readCompressedLOBLocatorColumn(
	payload []byte, offset int, colTypes []int, col int,
) (string, int, bool) {
	maxSize, n := readCompressedInt(payload[offset:])
	if n == 0 {
		return "", 0, false
	}

	offset += n

	if maxSize == 0 {
		return "", offset, true // a NULL LOB: the column is its zero size and nothing else
	}

	// A locator's length is a single CLR byte, so a size that cannot be spelled
	// by one is a walk that landed on the wrong bytes rather than a LOB.
	if maxSize > lobLocatorMaxLen {
		return "", 0, false
	}

	for range lobCompressedLocatorMaxSkips + 1 {
		if offset < len(payload) && int(payload[offset]) == maxSize && offset+1+maxSize <= len(payload) {
			return lobLocatorPlaceholder(colTypes, col), offset + 1 + maxSize, true
		}

		next, ok := skipCompressedInt(payload, offset)
		if !ok {
			return "", 0, false
		}

		offset = next
	}

	return "", 0, false
}

// lobLocatorMaxLen bounds a locator's declared length. A CLR spells its length
// in one byte, so anything past 0xFB — the first of the four values that mean
// something other than a length — is not one.
const lobLocatorMaxLen = 0xFB

// readInlineLongColumn reads a column whose value the server put in the row
// followed by the column's indicator and return code: a CLR carrying the value,
// then two compressed integers. A NULL is the empty CLR with the same two
// behind it (spelled -1 and 1405 — ORA-01403, "fetched column value is NULL" —
// rather than 0 and 0, which is why they are skipped as integers instead of
// being counted as a fixed block).
//
// Two families arrive in that shape and they are the same shape because they
// are the same thing. A **LOB** a thin client re-declared as a LONG is, from
// the server's side, a LONG column (see execDefineLOBShape). A **genuine** LONG
// or LONG RAW column — one the describe itself reports as type 8 or 24 — is one
// without the re-declaration. Measured 2026-09-22 on both thin drivers
// (testdata/go_ora_long.pcapng, testdata/python_thin_long.pcapng): identical
// framing, down to the `81 01` / `02 05 7d` a NULL spells its pair with.
//
// The one difference the LONG recordings added is the **value's** encoding. An
// inlined LOB arrived as a short-form CLR, so this read a single length byte
// and never saw anything else; a genuine LONG arrives as the 0xFE long form
// whatever its length — 20 bytes of text came back as `fe 01 14 … 00`. So the
// value is read as a CLR proper, in the long form this session negotiated
// (shape.bigClrChunks), which also fixes the case that was always there and
// never recorded: an inlined LOB over the 252-byte short-form limit is chunked
// too, and was costing its row.
func readInlineLongColumn(
	payload []byte, offset int, shape oerShape, colTypes []int, col int,
) (string, int, bool) {
	if offset >= len(payload) {
		return "", 0, false
	}

	raw, consumed := readCLRVariant(payload[offset:], shape.bigClrChunks)
	if consumed == 0 || len(raw) > rowValueMaxLen {
		return "", 0, false
	}

	offset += consumed

	for range inlineLongTrailingInts {
		next, ok := skipCompressedInt(payload, offset)
		if !ok {
			return "", 0, false
		}

		offset = next
	}

	if len(raw) == 0 {
		return "", offset, true
	}

	return decodeRowValue(colTypes, col, raw), offset, true
}

// objectImageSearchWindow bounds how far past an object locator readObjectImage
// will look for the image header. The two dialects measured put it 12 and 14
// bytes in; the window is wide enough for a third to differ and narrow enough
// that it cannot wander into the next row.
const objectImageSearchWindow = 32

// readObjectImage finds the image that follows an object or opaque locator and
// returns it together with the offset of the next column's value.
//
// It reads the image header rather than hard-coding its distance, because that
// distance is one of the things the OCI dialects spell differently (12 bytes of
// framing on the 64-bit one, 14 on the 4-byte one) and because the header
// validates itself: the image length arrives twice, once as a four-byte
// little-endian field and once as a single byte, with a constant 0x01 0x00 in
// between. Two encodings of one number agreeing is what makes a hit a
// measurement instead of a pattern that happened to match.
//
// An image longer than 255 bytes cannot be spelled by the one-byte field, so it
// is not matched and the row is refused. That is deliberate: no such image has
// been recorded, and a guess at how the longer form is framed would be a guess
// the capture then presents as a value.
//
// It was a pure skip until the image was decoded (decodeObjectImage): the
// bounds it already measured are the bounds of the value, so returning the
// bytes as well as the resume offset is the whole of the change.
func readObjectImage(payload []byte, offset int) ([]byte, int, bool) {
	const imageHeaderLen = 7

	for i := offset; i+imageHeaderLen <= len(payload) && i-offset <= objectImageSearchWindow; i++ {
		if payload[i+4] != 0x01 || payload[i+5] != 0x00 {
			continue
		}

		size := binary.LittleEndian.Uint32(payload[i : i+4])
		if size > 0xFF || byte(size) != payload[i+6] {
			continue
		}

		start := i + imageHeaderLen

		end := start + int(size)
		if end > len(payload) {
			return nil, 0, false
		}

		return payload[start:end], end, true
	}

	return nil, 0, false
}

// The object image's own encoding, and the reason an object column no longer
// captures the handle in front of it.
//
// The image opens with a flag byte, then the image's **own** length as a TTC
// compressed integer — a third spelling of the number the outer header already
// gave twice, and the check that makes this a reading rather than a cast. Two
// flags are decoded and everything else is refused:
//
//	0x84  a named object type: the attributes follow, each one a CLR, in the
//	      same length-prefixed encoding the rest of the row uses.
//	      Measured: `84 01 08 02 c1 02 01 78` is dbbat_cap_obj(1, 'x') —
//	      length 8, then the NUMBER 1 as `c1 02` and the string `x` as `78`.
//	0x85  an opaque type (SYS.XMLTYPE): a 0x01, then a big-endian ub4 naming
//	      what the payload behind it is, then the payload to the end of the
//	      image. Measured: `85 01 0c 01 00000014 3c 61 2f 3e` is
//	      XMLTYPE('<a/>') — length 12, kind 0x14 (text), payload `<a/>`.
//
// The two images above are the only two in the corpus: the whole of
// testdata/ carries them and nothing else of this shape (oci_describe.hex and
// oci64_describe.hex hold the first, oci_lob.hex and oci64_lob.hex the second).
// The reading is pinned against both dialects of each, and it agrees with
// go-ora's own — same flags, same compressed length, same `1`-then-ub4 header
// on the opaque one, same eight bytes before an XMLTYPE's text.
//
// Everything not measured fails closed to the locator hex the column captured
// before, rather than to a value that would read as data: the 0x88 collection
// flag, an opaque kind other than text (0x11 is a locator, so the payload is
// not in this packet either), an attribute walk that does not land exactly on
// the image's end, and the 0xFE/0xFF CLR forms — a chunked or NULL attribute,
// neither of which has been recorded.
const (
	// objectImageFlagNamedType is the flag byte of a named object type's
	// image, whose body is its attributes.
	objectImageFlagNamedType = 0x84

	// objectImageFlagOpaque is the flag byte of an opaque type's image
	// (SYS.XMLTYPE), whose body is a kind and a payload.
	objectImageFlagOpaque = 0x85

	// opaqueImageKindText is the only opaque kind decoded: the payload behind
	// it is the value's own text. 0x11, the other one go-ora reads, is a LOB
	// locator — the data is not in the packet, so there is nothing to render.
	opaqueImageKindText = 0x14

	// opaqueImageKindLen is the width of the kind field, a big-endian ub4.
	opaqueImageKindLen = 4

	// objectImageMaxAttributes bounds the attribute walk. An image is at most
	// 255 bytes and every attribute costs its length byte, so this can only be
	// reached by a walk that is already wrong.
	objectImageMaxAttributes = 255
)

// decodeObjectImage renders an object or opaque column's image as the value it
// carries, or reports that it could not be read.
//
// A false return is the fail-closed path and it is the point of the split:
// readRowColumn then captures the locator hex it always captured, so an image
// shape nothing has recorded costs the reader nothing and invents nothing.
func decodeObjectImage(image []byte) (string, bool) {
	const flagLen = 1

	if len(image) < flagLen {
		return "", false
	}

	declared, n := readCompressedInt(image[flagLen:])
	if n == 0 || declared != len(image) {
		return "", false
	}

	body := image[flagLen+n:]

	switch image[0] {
	case objectImageFlagNamedType:
		return decodeNamedObjectAttributes(body)
	case objectImageFlagOpaque:
		return decodeOpaqueImageBody(body)
	default:
		return "", false
	}
}

// decodeNamedObjectAttributes walks a named object type's attributes and joins
// them into one readable value.
//
// The attributes carry no types — the row says how long each one is and nothing
// more, and the describe names the object's type without describing its shape —
// so each is rendered by decodeOracleRawValue, the same type-less reading this
// package already applies wherever a column's type code is not known. On the
// one recorded object that is `1` and `x` rather than `c102` and `78`.
//
// The walk must consume the image **exactly**. A remainder or an overrun means
// the attributes are not laid out the way this reads them, and the whole image
// is refused rather than reported in part.
func decodeNamedObjectAttributes(body []byte) (string, bool) {
	values := make([]string, 0, 4)

	for offset := 0; offset < len(body); {
		length := int(body[offset])
		if length >= 0xFE {
			return "", false // a chunked CLR or a NULL attribute: unrecorded
		}

		offset++

		if offset+length > len(body) || len(values) >= objectImageMaxAttributes {
			return "", false
		}

		values = append(values, decodeOracleRawValue(body[offset:offset+length]))
		offset += length
	}

	return "(" + strings.Join(values, ", ") + ")", true
}

// decodeOpaqueImageBody reads an opaque type's image body: a constant 0x01, a
// big-endian ub4 naming the payload's kind, and the payload to the end of the
// image.
func decodeOpaqueImageBody(body []byte) (string, bool) {
	const markerLen = 1

	if len(body) < markerLen+opaqueImageKindLen || body[0] != 0x01 {
		return "", false
	}

	kind := binary.BigEndian.Uint32(body[markerLen : markerLen+opaqueImageKindLen])
	if kind != opaqueImageKindText {
		return "", false
	}

	return string(body[markerLen+opaqueImageKindLen:]), true
}

// lobLocatorPlaceholder is what dbbat captures for a LOB column, and the
// decision is deliberate enough to state: **the locator's bytes are not the
// value and dbbat will not go and fetch the one they name.**
//
// A locator is a handle into the server — it names a LOB, changes from fetch to
// fetch, and says nothing a reader of the audit trail could use. The contents
// arrive, when a client wants them, in LOB reads of that client's own; for
// dbbat to issue those itself would be dbbat running statements on the
// session's behalf, which is out of bounds however convenient the result would
// read.
//
// So the column is captured as a marker naming its type. It is not the empty
// string, which is what a NULL captures as, and not the locator hex, which
// would read as data. A NULL LOB — a zero-length locator — captures as "" like
// every other NULL.
func lobLocatorPlaceholder(colTypes []int, col int) string {
	if col >= 0 && col < len(colTypes) {
		switch colTypes[col] {
		case tnsTypeBLOB:
			return "<BLOB locator>"
		case tnsTypeBFILE:
			return "<BFILE locator>"
		}
	}

	return "<CLOB locator>"
}

// rowValueMaxLen bounds a single column value's length byte. Oracle's own
// maximum for an inline datum, and the guard that keeps a walk that has drifted
// into framing from claiming most of the packet as one value.
const rowValueMaxLen = 4000

// readRowColumn reads one column's value at payload[offset] and returns it with
// the offset the next column starts at.
//
// A scalar column is a one-byte length and then that many bytes. The other
// three shapes are not: an object or opaque column is a length-prefixed locator
// followed by framing and the object's own image, a LOB column is a header
// whose layout is the dialect's own (see readFixedLOBColumn and
// readCompressedLOBColumn), and a LONG column is a CLR with its indicator and
// return code behind it. Reading any of them as a scalar and then carrying on
// is what used to lose the whole rest of the row.
func readRowColumn(
	payload []byte, offset int, shape oerShape, colTypes []int, col int, lob lobRowShape,
) (string, int, bool) {
	if offset >= len(payload) {
		return "", 0, false
	}

	switch rowValueShapeOf(colTypes, col) {
	case rowValueLOBLocator:
		if shape.fixedWidth {
			return readFixedLOBColumn(payload, offset, shape, colTypes, col)
		}

		return readCompressedLOBColumn(payload, offset, shape, colTypes, col, lob)

	case rowValueLongInline:
		if rowColumnIsFramed(shape, colTypes, col) {
			return readInlineLongColumn(payload, offset, shape, colTypes, col)
		}

	case rowValueScalar, rowValueObjectImage:
	}

	valLen := int(payload[offset])
	offset++

	if valLen > rowValueMaxLen || offset+valLen > len(payload) {
		return "", 0, false
	}

	raw := payload[offset : offset+valLen]
	offset += valLen

	switch rowValueShapeOf(colTypes, col) {
	case rowValueLOBLocator:
		// Answered above, before the length byte was read: a LOB column does
		// not open with one, which is the whole of what the dialects differ on.
		return "", 0, false

	case rowValueObjectImage:
		image, next, ok := readObjectImage(payload, offset)
		if !ok {
			return "", 0, false
		}

		// The image is the value; the locator in front of it is a handle. Only
		// an image this package has measured is rendered — anything else keeps
		// the locator hex the column captured before (decodeObjectImage).
		if value, ok := decodeObjectImage(image); ok {
			return value, next, true
		}

		return decodeRowValue(colTypes, col, raw), next, true

	case rowValueScalar, rowValueLongInline:
		// rowValueLongInline reaches here only on an OCI dialect, where it is
		// deliberately still read as a scalar — see the switch above.
		fallthrough
	default:
		if valLen == 0 {
			return "", offset, true
		}

		return decodeRowValue(colTypes, col, raw), offset, true
	}
}

// rowEndsAtMarker reports whether offset sits on something that can legitimately
// follow a row: the 0x07 / 0x15 separators, the 0x08 footer, or the end of the
// payload.
//
// It is the check that makes the locator skips above safe to get wrong. A row
// with no locator in it is accepted wherever it ends, exactly as before — the
// stream simply stops at the first byte readRowSeparator does not recognize. A
// row that needed a skip is accepted only if the columns after that skip landed
// on their own values, which is what a clean terminator says and what a drifted
// walk almost never produces.
func rowEndsAtMarker(payload []byte, offset int) bool {
	if offset >= len(payload) {
		return true
	}

	switch payload[offset] {
	case 0x07, continuationDescriptorMarker, 0x08:
		return true
	}

	return rowEndsAtEndOfData(payload, offset)
}

// rowEndsAtEndOfData reports whether offset sits on the OER object that ends a
// fetch — ORA-01403, "no data found", the fourth thing that can legitimately
// follow the last row of a row area.
//
// It is here because two clients end the same fetch differently: go-ora's rows
// are followed by the 0x08 footer and then the OER, python-oracledb thin's run
// straight into the OER with no footer at all (testdata/python_thin_long.pcapng
// against testdata/go_ora_long.pcapng, the same two queries). Without this, a
// python session lost the **last** row of every fetch with a framed column in
// it — a LOB, an object or a LONG — because the row was refused for landing on
// a perfectly good terminator.
//
// The signature is deliberately the whole object rather than its 0x04 marker
// byte: the seven leading fields have to decode, and the error field has to be
// 1403. A fourth accepted byte value would have widened the gate that makes
// every framed reading in this package safe to get wrong; a decoded end-of-data
// object is a measurement, the same kind the readings themselves are.
//
// The end-of-call bit decodeOERAt demands is *not* required, because this
// object does not carry it: measured `callStatus 1, errNum 1403` on both thin
// recordings. That bit separates an OER that ends the **call** from one
// traveling inside a stream, which is a different question from the one asked
// here.
func rowEndsAtEndOfData(payload []byte, offset int) bool {
	info, _ := decodeOERFieldsAt(payload, offset)

	return info != nil && info.ErrorCode == oraNoDataFound
}

// parseRowStream decodes a run of compressed rows starting at payload[offset].
//
// Both the QueryResult (func=0x10) row area and continuation (func=0x06) packets
// use this identical encoding: each row sends length-prefixed values only for
// its active columns; columns absent from a row keep their previous value. Rows
// are separated by either a bare 0x07 (all columns active next) or a compression
// descriptor 0x15 [flag] [count] [bitmask] 0x07 (bitmask = active columns next).
// The stream ends at the 0x08 footer, an ORA-01403 marker, or malformed bytes.
//
// activeCols are the columns carried in the FIRST row; prev seeds the carried-
// over values (nil for a fresh QueryResult, the prior packet's last row for a
// continuation). colTypes holds the per-column TTC type code for type-aware
// value decoding (nil → heuristic). There is no row cap — the markers bound the
// scan, and the caller (captureRow) enforces the configured result-size limits.
func parseRowStream(
	payload []byte, offset, numCols int, activeCols []int, prev []string, colTypes []int, shape oerShape,
	lob lobRowShape,
) [][]interface{} {
	if numCols == 0 {
		return nil
	}

	cur := make([]string, numCols)
	copy(cur, prev)

	var rows [][]interface{}

	for offset < len(payload) {
		// The end-of-rows footer is the 3-byte sequence 0x08 0x01 0x06. A bare
		// 0x08 must NOT end the scan on its own: it is also a valid column value
		// length (an 8-byte first column value, e.g. the string "sqlcl-ok"),
		// which previously made such rows vanish. Match the full footer instead.
		if offset+3 <= len(payload) &&
			payload[offset] == 0x08 && payload[offset+1] == 0x01 && payload[offset+2] == 0x06 {
			break
		}

		if offset+9 <= len(payload) && string(payload[offset:offset+9]) == "ORA-01403" {
			break // end-of-data marker
		}

		row := make([]interface{}, numCols)
		for i := range numCols {
			row[i] = cur[i] // default: carried-over value
		}

		valid := true
		skipped := false

		for _, col := range activeCols {
			decoded, next, ok := readRowColumn(payload, offset, shape, colTypes, col, lob)
			if !ok {
				valid = false

				break
			}

			skipped = skipped || rowColumnIsFramed(shape, colTypes, col)
			row[col] = decoded
			cur[col] = decoded
			offset = next
		}

		// A row that stepped over a locator's framing is only kept when it comes
		// out on a row marker: the skip lengths are measured, so a wrong one has
		// to cost the row rather than fill it with framing bytes.
		if valid && skipped && !rowEndsAtMarker(payload, offset) {
			valid = false
		}

		if !valid {
			break
		}

		rows = append(rows, row)

		next, newOffset, cont := readRowSeparator(payload, offset, numCols)
		if !cont {
			break
		}

		activeCols = next
		offset = newOffset
	}

	return rows
}

// readRowSeparator consumes the marker that follows a row and returns the
// columns active in the next row, the advanced offset, and whether parsing
// should continue. It handles a bare 0x07 separator (all columns active next)
// and the 0x15 [flag] [count] [bitmask] 0x07 compression descriptor. Any other
// byte (notably the 0x08 footer) ends the stream.
func readRowSeparator(payload []byte, offset, numCols int) ([]int, int, bool) {
	if offset >= len(payload) {
		return nil, offset, false
	}

	switch payload[offset] {
	case 0x07:
		return allColumns(numCols), offset + 1, true
	case continuationDescriptorMarker:
		// Descriptor: 0x15 [flag] [count] [bitmask...] 0x07. The bitmask spans
		// ceil(numCols/8) bytes. Parse it structurally rather than scanning for
		// the 0x07 terminator — a bitmask byte can itself be 0x07 (e.g. columns
		// 0,1,2 → 0x07), which would otherwise truncate the descriptor and leave
		// the real terminator to corrupt the next row.
		bitmaskBytes := (numCols + 7) / 8
		maskStart := offset + 3 // skip 0x15, flag, count
		maskEnd := maskStart + bitmaskBytes

		if maskEnd > len(payload) {
			return nil, len(payload), false
		}

		next := bitmaskColumns(payload[maskStart:maskEnd], numCols)

		end := maskEnd
		if end < len(payload) && payload[end] == 0x07 {
			end++ // consume the 0x07 terminator
		}

		if len(next) == 0 {
			next = allColumns(numCols)
		}

		return next, end, true
	default:
		return nil, offset, false
	}
}

// bitmaskToColumns converts a single-byte column bitmask to a sorted slice of
// column indices. Bit 0 = column 0, bit 1 = column 1, etc.
func bitmaskToColumns(bitmask byte, numCols int) []int {
	return bitmaskColumns([]byte{bitmask}, numCols)
}

// bitmaskColumns converts a (possibly multi-byte, little-endian) column bitmask
// to a sorted slice of active column indices: byte 0 holds columns 0-7, byte 1
// columns 8-15, and so on.
func bitmaskColumns(mask []byte, numCols int) []int {
	var cols []int

	for col := range numCols {
		if b := col / 8; b < len(mask) && mask[b]&(1<<(col%8)) != 0 {
			cols = append(cols, col)
		}
	}

	return cols
}

// allColumns returns a slice [0, 1, 2, ..., n-1].
func allColumns(n int) []int {
	cols := make([]int, n)
	for i := range n {
		cols[i] = i
	}

	return cols
}

// findBytes finds the first occurrence of pattern in data.
func findBytes(data, pattern []byte) int {
	for i := 0; i <= len(data)-len(pattern); i++ {
		match := true
		for j := range pattern {
			if data[i+j] != pattern[j] {
				match = false
				break
			}
		}

		if match {
			return i
		}
	}

	return -1
}

// looksLikeSQL returns true if the string appears to be SQL text: it opens with
// a statement verb that ends at a word boundary.
//
// The boundary is the fix, not a detail. A bare prefix match read the
// `GRANTED_ROLE='DBA'` in DBeaver's privilege probe as a GRANT statement and
// `DELETE_RULE, …` as a DELETE, which is how the loose scan came to hand the
// gate a fragment starting inside a column name (see ttc_exec_statement.go).
func looksLikeSQL(s string) bool {
	if len(s) < 2 {
		return false
	}

	return startsWithSQLVerb(s)
}

// decodeVarLen decodes a variable-length integer used in TTC.
// Returns the value and the number of bytes consumed.
func decodeVarLen(data []byte) (uint32, int, error) {
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("%w: no data for length", ErrOALL8TooShort)
	}

	first := data[0]

	switch {
	case first < oall8LenShort:
		return uint32(first), 1, nil
	case first == oall8LenShort:
		if len(data) < 3 {
			return 0, 0, fmt.Errorf("%w: need 3 bytes for short extended length", ErrOALL8TooShort)
		}

		return uint32(binary.BigEndian.Uint16(data[1:3])), 3, nil
	default: // 0xFF
		if len(data) < 5 {
			return 0, 0, fmt.Errorf("%w: need 5 bytes for long extended length", ErrOALL8TooShort)
		}

		return binary.BigEndian.Uint32(data[1:5]), 5, nil
	}
}

// decodeBindValues extracts bind values from the remaining OALL8 payload.
// Each bind value is encoded as: length (varlen) + value bytes.
// NULL values have length 0.
func decodeBindValues(data []byte, count int) []string {
	values := make([]string, 0, count)
	offset := 0

	for i := 0; i < count; i++ {
		if offset >= len(data) {
			break
		}

		// Bind value length
		valLen, bytesRead, err := decodeVarLen(data[offset:])
		if err != nil {
			break
		}

		offset += bytesRead

		if valLen == 0 {
			values = append(values, "NULL")
			continue
		}

		if offset+int(valLen) > len(data) {
			break
		}

		valBytes := data[offset : offset+int(valLen)]
		offset += int(valLen)

		values = append(values, decodeBindValue(valBytes))
	}

	return values
}

// decodeBindValue renders a single bind value. Bind values carry no inline type
// tag, so it prefers readable text (including UTF-8, which the row-capture
// ASCII-first heuristic would mangle), then a valid Oracle NUMBER (so a numeric
// bind shows as its decimal rather than hex), and finally falls back to hex for
// other binary content.
func decodeBindValue(b []byte) string {
	if len(b) == 0 {
		return "NULL"
	}

	if !isBinaryData(b) {
		return string(b)
	}

	if s, ok := decodeOracleNumberToString(b); ok {
		return s
	}

	return hex.EncodeToString(b)
}

// isBinaryData checks if data is binary (non-text) content.
// Returns true if the data is not valid UTF-8 or contains control characters.
func isBinaryData(data []byte) bool {
	if !utf8.Valid(data) {
		return true
	}

	for _, r := range string(data) {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return true
		}
	}

	return false
}
