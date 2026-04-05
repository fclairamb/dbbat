package oracle

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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
		resp.IsError = true
		resp.ErrorCode = int(errCode)

		// Try to extract error message
		if len(payload) >= 16 {
			msgLen := binary.BigEndian.Uint16(payload[14:16])
			if len(payload) >= 16+int(msgLen) && msgLen > 0 {
				resp.ErrorMessage = string(payload[16 : 16+msgLen])
			} else {
				resp.ErrorMessage = fmt.Sprintf("ORA-%05d", errCode)
			}
		}

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
	ErrOFETCHTooShort   = errors.New("OFETCH payload too short")
	ErrSQLLengthInvalid = errors.New("OALL8 SQL length exceeds payload")
)

// OALL8Result contains the decoded fields from an OALL8 (parse+execute) message.
type OALL8Result struct {
	SQL        string
	CursorID   uint16
	BindValues []string
}

// IsPLSQL returns true if the SQL text is a PL/SQL block.
func (r *OALL8Result) IsPLSQL() bool {
	normalized := strings.ToUpper(strings.TrimSpace(r.SQL))
	return strings.HasPrefix(normalized, "BEGIN") || strings.HasPrefix(normalized, "DECLARE")
}

// OFETCHResult contains the decoded fields from an OFETCH message.
type OFETCHResult struct {
	CursorID  uint16
	FetchSize uint32
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

	if sqlLen == 0 {
		return nil, ErrEmptySQL
	}

	// SQL text
	if offset+int(sqlLen) > len(ttcPayload) {
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
func decodePiggybackExecSQL(ttcPayload []byte) (*OALL8Result, error) {
	if len(ttcPayload) < 52 {
		return nil, fmt.Errorf("%w: piggyback exec needs at least 52 bytes, got %d", ErrOALL8TooShort, len(ttcPayload))
	}

	// Strategy: scan the payload for SQL text. Different Oracle client drivers
	// (oracledb thin, JDBC thin) place the SQL at slightly different offsets
	// (50-54 typically). We scan a range and validate the extracted text.
	for offset := 40; offset < 70 && offset < len(ttcPayload)-1; offset++ {
		sql, scanErr := extractSQLAtOffset(ttcPayload, offset)
		if scanErr == nil && sql != "" {
			return &OALL8Result{SQL: sql}, nil
		}
	}

	// Last resort: find SQL keywords directly in the payload
	sql := findSQLInPayload(ttcPayload)
	if sql != "" {
		return &OALL8Result{SQL: sql}, nil
	}

	return nil, fmt.Errorf("%w: could not find SQL text in piggyback exec payload", ErrEmptySQL)
}

// decodeExecSQL extracts SQL text from an execute-with-SQL message (func=0x11).
//
// Different Oracle client drivers use func=0x11 with different sub-operations:
//   - DBeaver/JDBC thin: sub=0x69, SQL at TTC offset 57-63
//   - Python oracledb thin: sub=0x98, SQL at TTC offset 63-67
//
// The SQL is preceded by a run of zero bytes and its length is encoded with
// the standard varlen encoding.
func decodeExecSQL(ttcPayload []byte) (*OALL8Result, error) {
	if len(ttcPayload) < 30 {
		return nil, fmt.Errorf("%w: exec needs at least 30 bytes, got %d", ErrOALL8TooShort, len(ttcPayload))
	}

	// Scan for SQL text at known offsets across client drivers.
	for offset := 50; offset <= 75 && offset < len(ttcPayload)-1; offset++ {
		sql, err := extractSQLAtOffset(ttcPayload, offset)
		if err == nil && sql != "" {
			return &OALL8Result{SQL: sql}, nil
		}
	}

	// Fallback: find SQL keywords directly
	sql := findSQLInPayload(ttcPayload)
	if sql != "" {
		return &OALL8Result{SQL: sql}, nil
	}

	return nil, fmt.Errorf("%w: could not find SQL text in JDBC exec payload", ErrEmptySQL)
}

// findSQLInPayload scans the raw payload for SQL text by looking for SQL keywords.
// Used as a fallback when length-prefix decoding fails.
func findSQLInPayload(payload []byte) string {
	keywords := []string{
		"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP",
		"ALTER", "BEGIN", "DECLARE", "WITH", "MERGE", "CALL",
	}

	for _, kw := range keywords {
		kwBytes := []byte(kw)
		idx := findBytes(payload, kwBytes)
		if idx < 0 {
			continue
		}

		// Found a keyword — extract until we hit a non-SQL byte
		// SQL ends at a null byte, or at the end of printable ASCII
		end := idx
		for end < len(payload) && payload[end] >= 0x0A && payload[end] <= 0x7E {
			end++
		}

		if end > idx+2 {
			return strings.TrimSpace(string(payload[idx:end]))
		}
	}

	return ""
}

// extractSQLAtOffset tries to read a length-prefixed SQL string at the given offset.
// Returns the SQL text, bytes consumed, and error.
func extractSQLAtOffset(data []byte, offset int) (string, error) {
	if offset >= len(data) {
		return "", ErrOALL8TooShort
	}

	sqlLen, bytesRead, err := decodeVarLen(data[offset:])
	if err != nil || sqlLen == 0 || sqlLen > 32768 {
		return "", ErrEmptySQL
	}

	sqlStart := offset + bytesRead
	sqlEnd := sqlStart + int(sqlLen)

	if sqlEnd > len(data) {
		return "", ErrSQLLengthInvalid
	}

	sqlText := string(data[sqlStart:sqlEnd])

	// Validate that it looks like SQL (starts with a keyword or is mostly printable)
	if !looksLikeSQL(sqlText) {
		return "", ErrEmptySQL
	}

	return sqlText, nil
}

// QueryResultV2 contains parsed data from a v315+ TTC QueryResult (func=0x10).
type QueryResultV2 struct {
	Columns []string
	Rows    [][]string
	NoData  bool // true if ORA-01403 (normal end-of-data)
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
func decodeQueryResultV2(ttcPayload []byte) *QueryResultV2 {
	if len(ttcPayload) < 20 {
		return nil
	}

	result := &QueryResultV2{}

	// Check for ORA-01403 (no data found) — this is a normal end-of-data marker
	if idx := findBytes(ttcPayload, []byte("ORA-01403")); idx >= 0 {
		result.NoData = true
	}

	// Phase 1: Find column names
	// Column names appear in the area BEFORE the 0x06 0x22 row data marker
	markerIdx := findBytes(ttcPayload, []byte{0x06, 0x22})
	columnArea := ttcPayload
	if markerIdx > 0 {
		columnArea = ttcPayload[:markerIdx]
	}

	result.Columns = scanColumnNames(columnArea)

	if len(result.Columns) == 0 {
		return result
	}

	// Phase 2: Find row values
	// Row values appear after the column definitions. We look for a marker
	// pattern that separates column defs from row data.
	// The row data area starts roughly after the column definitions.
	result.Rows = scanRowValues(ttcPayload, len(result.Columns))

	return result
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

// scanRowValues extracts row values from the payload.
// Each row contains `numCols` length-prefixed values.
//
// Row data layout (after the 0x06 0x22 marker + descriptor):
//
//	[0x07]                  — separator before first row
//	[len1] [val1]           — first column value
//	[len2] [val2]           — second column value (0x00 = NULL)
//	...
//	[0x07]                  — separator between rows
//	[len1] [val1] ...       — next row
//	[0x08] [0x01] [0x06]    — end-of-rows footer
func scanRowValues(data []byte, numCols int) [][]string {
	rowStart := findRowDataStart(data)
	if rowStart < 0 || numCols == 0 {
		return nil
	}

	var rows [][]string
	offset := rowStart
	endOfData := len(data)
	maxRows := 100

	for len(rows) < maxRows && offset < endOfData {
		row := make([]string, 0, numCols)
		valid := true

		for col := 0; col < numCols; col++ {
			if offset >= endOfData {
				valid = false
				break
			}

			valLen := int(data[offset])
			offset++

			if valLen == 0 {
				row = append(row, "") // NULL
				continue
			}

			if valLen > 4000 || offset+valLen > endOfData {
				valid = false
				break
			}

			valBytes := data[offset : offset+valLen]
			offset += valLen

			row = append(row, decodeOracleRawValue(valBytes))
		}

		if !valid || len(row) != numCols {
			break
		}

		rows = append(rows, row)

		// After a row, expect either:
		// - 0x08 0x01 0x06: end-of-rows footer → stop
		// - 0x07: row separator → continue to next row
		// - anything else: stop
		if offset >= endOfData {
			break
		}

		if data[offset] == 0x08 {
			break // Footer (0x08 0x01 0x06) or end of data
		}

		if data[offset] == 0x07 {
			offset++ // Skip row separator, continue to next row
			continue
		}

		break // Unknown byte — stop
	}

	return rows
}

// findRowDataStart locates where row data begins in the response.
// Finds the 0x06 0x22 marker, skips the descriptor, and positions
// after the 0x07 separator that precedes the first row.
func findRowDataStart(data []byte) int {
	marker := []byte{0x06, 0x22}
	idx := findBytes(data, marker)
	if idx < 0 {
		return -1
	}

	// Skip past the marker + descriptor to find the 0x07 before first row
	for offset := idx + 2; offset < len(data)-1; offset++ {
		if data[offset] == 0x07 {
			return offset + 1 // Start reading values after the 0x07
		}
	}

	return -1
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

// decodeOracleNumberToString converts Oracle NUMBER format to a string.
// Oracle NUMBER: [exponent byte] [mantissa digits...]
// Exponent byte: value - 193 gives the power of 100
// Each mantissa byte: value - 1 gives a two-digit number (00-99)
func decodeOracleNumberToString(b []byte) (string, bool) {
	if len(b) < 2 {
		return "", false
	}

	exp := int(b[0])

	// Check for zero
	if exp == 128 && len(b) == 1 {
		return "0", true
	}

	// Positive numbers: exponent >= 193
	if exp < 193 || exp > 213 {
		return "", false // Not a simple positive number
	}

	// Convert mantissa digits
	power := exp - 193
	var result int64

	for i := 1; i < len(b); i++ {
		digit := int64(b[i]) - 1
		if digit < 0 || digit > 99 {
			return "", false
		}

		result = result*100 + digit
	}

	// Apply power of 100
	for i := len(b) - 2; i < power; i++ {
		result *= 100
	}

	return fmt.Sprintf("%d", result), true
}

// continuationDescriptorMarker (0x15) appears after each row in a continuation
// packet. It is followed by a descriptor that encodes which columns have new
// values in the NEXT row: [flag] [count] [bitmask] then 0x07.
const continuationDescriptorMarker = 0x15

// parseContinuationRows decodes rows from a TTC continuation packet (func=0x06).
//
// Oracle's continuation format uses column-level compression:
//   - A header bitmask (at header_end-2) indicates which columns have new values
//     in the first row of the packet.
//   - After each row, a descriptor (0x15 [flag] [count] [bitmask] 0x07) indicates
//     which columns have new values in the NEXT row.
//   - Columns not in the bitmask retain their values from the previous row.
//
// The prevRow parameter provides the last row from the previous packet (or the
// QueryResult) so that unchanged columns can be filled in correctly.
//
//nolint:gocognit,nestif,cyclop // Binary protocol parser requires many branches for different field types and markers.
func parseContinuationRows(payload []byte, numCols int, prevRow []string) [][]interface{} {
	if numCols == 0 || len(payload) < 15 {
		return nil
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

	// Initialize previous row values from the provided lastRow.
	prev := make([]string, numCols)
	if len(prevRow) == numCols {
		copy(prev, prevRow)
	}

	offset := headerEnd + 1
	var rows [][]interface{}

	for offset < len(payload) {
		if payload[offset] == 0x08 {
			break
		}

		// Check for ORA-01403 end marker
		if offset+9 <= len(payload) && string(payload[offset:offset+9]) == "ORA-01403" {
			break
		}

		// Read values for active columns; inactive columns keep previous values.
		row := make([]interface{}, numCols)
		for i := range numCols {
			row[i] = prev[i] // Default: previous value
		}

		valid := true

		for _, col := range activeCols {
			if offset >= len(payload) {
				valid = false

				break
			}

			valLen := int(payload[offset])
			offset++

			if valLen == 0 {
				row[col] = ""
				prev[col] = ""

				continue
			}

			if valLen > 4000 || offset+valLen > len(payload) {
				valid = false

				break
			}

			decoded := decodeOracleRawValue(payload[offset : offset+valLen])
			row[col] = decoded
			prev[col] = decoded
			offset += valLen
		}

		if !valid {
			break
		}

		rows = append(rows, row)

		// Parse the descriptor after the row to determine active columns for the next row.
		if offset >= len(payload) {
			break
		}

		switch payload[offset] {
		case continuationDescriptorMarker:
			offset++ // skip 0x15

			// Read descriptor bytes until 0x07 or 0x08.
			var desc []byte
			for offset < len(payload) && payload[offset] != 0x07 && payload[offset] != 0x08 {
				desc = append(desc, payload[offset])
				offset++
			}

			// Descriptor format: [flag] [count] [bitmask]
			// The bitmask is at index 2 (for ≤8 columns).
			if len(desc) >= 3 {
				activeCols = bitmaskToColumns(desc[2], numCols)
			} else {
				activeCols = allColumns(numCols)
			}

			// Skip 0x07 separator
			if offset < len(payload) && payload[offset] == 0x07 {
				offset++
			}
		case 0x07:
			offset++
			activeCols = allColumns(numCols) // Simple separator: all columns in next row
		default:
			break // Unknown byte — stop
		}
	}

	return rows
}

// bitmaskToColumns converts a column bitmask to a sorted slice of column indices.
// Bit 0 = column 0, bit 1 = column 1, etc.
func bitmaskToColumns(bitmask byte, numCols int) []int {
	var cols []int

	for bit := range numCols {
		if bitmask&(1<<bit) != 0 {
			cols = append(cols, bit)
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

// looksLikeSQL returns true if the string appears to be SQL text.
func looksLikeSQL(s string) bool {
	if len(s) < 2 {
		return false
	}

	upper := strings.ToUpper(strings.TrimSpace(s))
	sqlKeywords := []string{
		"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP",
		"ALTER", "TRUNCATE", "MERGE", "CALL", "BEGIN", "DECLARE", "WITH", "GRANT", "REVOKE",
		"EXPLAIN", "SET", "COMMIT", "ROLLBACK", "SAVEPOINT", "LOCK", "COMMENT",
	}

	for _, kw := range sqlKeywords {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}

	return false
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

		// Detect binary values (non-UTF8 or non-printable)
		if isBinaryData(valBytes) {
			values = append(values, hex.EncodeToString(valBytes))
		} else {
			values = append(values, string(valBytes))
		}
	}

	return values
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

// OFETCH binary layout:
//
//	Offset  Size  Field
//	0       1     Function code (0x11)
//	1       2     Cursor ID (uint16 BE)
//	3       4     Fetch size / row count (uint32 BE)

const ofetchMinPayloadSize = 7 // func(1) + cursor(2) + fetchsize(4)

// decodeOFETCH decodes an OFETCH TTC payload (starting from the function code byte).
func decodeOFETCH(ttcPayload []byte) (*OFETCHResult, error) {
	if len(ttcPayload) < ofetchMinPayloadSize {
		return nil, fmt.Errorf("%w: got %d bytes, need at least %d", ErrOFETCHTooShort, len(ttcPayload), ofetchMinPayloadSize)
	}

	// Skip function code (1 byte)
	cursorID := binary.BigEndian.Uint16(ttcPayload[1:3])
	fetchSize := binary.BigEndian.Uint32(ttcPayload[3:7])

	return &OFETCHResult{
		CursorID:  cursorID,
		FetchSize: fetchSize,
	}, nil
}
