package oracle

import (
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

	// The name scanner misses single-char and unnamed expression columns
	// (e.g. `SELECT 1 FROM dual`, `SELECT level AS n`), which would undercount
	// columns and corrupt row framing. The describe header carries the
	// authoritative count — pad up to it with synthetic names so the row stream
	// is parsed with the right column count.
	if n, ok := describeColumnCount(ttcPayload); ok && n > len(result.Columns) {
		for i := len(result.Columns); i < n; i++ {
			result.Columns = append(result.Columns, fmt.Sprintf("COL%d", i+1))
		}
	}

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

// describeColumnCount reads the authoritative column count from a v315 describe
// message (TTC func 0x10), whose header is:
//
//	[0x10] [size] [size bytes] [maxRowSize: compressed int] [colCount: compressed int]
//
// Returns false if the payload is not a describe header or the count is out of a
// sane range, in which case callers fall back to the scanned column names.
func describeColumnCount(ttcPayload []byte) (int, bool) {
	if len(ttcPayload) < 3 || ttcPayload[0] != byte(TTCFuncQueryResult) {
		return 0, false
	}

	pos := 1
	size := int(ttcPayload[pos])
	pos += 1 + size // skip the size byte and the size-bytes prefix

	if pos >= len(ttcPayload) {
		return 0, false
	}

	_, n1 := readCompressedInt(ttcPayload[pos:]) // maxRowSize
	if n1 == 0 {
		return 0, false
	}

	pos += n1

	count, n2 := readCompressedInt(ttcPayload[pos:])
	if n2 == 0 || count <= 0 || count > 1000 {
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
func scanRowValues(data []byte, numCols int) [][]string {
	rowStart := findRowDataStart(data)
	if rowStart < 0 || numCols == 0 {
		return nil
	}

	rows := parseRowStream(data, rowStart, numCols, allColumns(numCols), nil)

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
// 11-12 carry the zone: a numeric offset (tzHour = b[11]-20, tzMin = b[12]-60)
// when b[11]'s high bit is clear, or a named-region id (not resolvable to a
// numeric offset here) when it is set — region values decode to the UTC wall
// clock without an offset suffix.
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
	// (Oracle stores the instant in UTC) plus the offset suffix.
	if len(b) == 13 && b[11]&0x80 == 0 {
		offsetSec := (int(b[11])-20)*3600 + (int(b[12])-60)*60
		if offsetSec < -15*3600 || offsetSec > 15*3600 {
			return "", false
		}

		local := t.In(time.FixedZone("", offsetSec))

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

	// Carry forward the previous packet's last row for compressed-away columns.
	prev := make([]string, numCols)
	if len(prevRow) == numCols {
		copy(prev, prevRow)
	}

	return parseRowStream(payload, headerEnd+1, numCols, activeCols, prev)
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
// continuation). There is no row cap — the markers bound the scan, and the
// caller (captureRow) enforces the configured result-size limits.
func parseRowStream(payload []byte, offset, numCols int, activeCols []int, prev []string) [][]interface{} {
	if numCols == 0 {
		return nil
	}

	cur := make([]string, numCols)
	copy(cur, prev)

	var rows [][]interface{}

	for offset < len(payload) {
		if payload[offset] == 0x08 {
			break // end-of-rows footer (0x08 0x01 0x06)
		}

		if offset+9 <= len(payload) && string(payload[offset:offset+9]) == "ORA-01403" {
			break // end-of-data marker
		}

		row := make([]interface{}, numCols)
		for i := range numCols {
			row[i] = cur[i] // default: carried-over value
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
				cur[col] = ""

				continue
			}

			if valLen > 4000 || offset+valLen > len(payload) {
				valid = false

				break
			}

			decoded := decodeOracleRawValue(payload[offset : offset+valLen])
			row[col] = decoded
			cur[col] = decoded
			offset += valLen
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
