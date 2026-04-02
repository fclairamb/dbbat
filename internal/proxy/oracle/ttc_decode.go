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
		return columnDef{}, 0, fmt.Errorf("column def too short")
	}

	offset := 0

	// Name length + name
	nameLen, bytesRead, err := decodeVarLen(data[offset:])
	if err != nil {
		return columnDef{}, 0, err
	}

	offset += bytesRead

	if offset+int(nameLen) > len(data) {
		return columnDef{}, 0, fmt.Errorf("column name exceeds payload")
	}

	name := string(data[offset : offset+int(nameLen)])
	offset += int(nameLen)

	// Type code (1 byte)
	if offset >= len(data) {
		return columnDef{}, 0, fmt.Errorf("no type code")
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
		return nil, 0, fmt.Errorf("empty row data")
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
			return nil, 0, fmt.Errorf("row value exceeds payload")
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
	oall8MinPayloadSize = 8  // func(1) + options(4) + cursor(2) + sql_len(1)
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
