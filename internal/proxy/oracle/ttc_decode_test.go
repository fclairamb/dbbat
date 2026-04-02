package oracle

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildOALL8 creates a well-formed OALL8 TTC payload for testing.
func buildOALL8(sql string, binds []string, cursorID uint16) []byte {
	buf := make([]byte, 0, 64)

	// Function code
	buf = append(buf, byte(TTCFuncOALL8))

	// Options (4 bytes)
	buf = append(buf, 0x00, 0x00, 0x00, 0x00)

	// Cursor ID (2 bytes BE)
	cidBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(cidBuf, cursorID)
	buf = append(buf, cidBuf...)

	// SQL length + text
	buf = append(buf, encodeVarLen(uint32(len(sql)))...)
	buf = append(buf, []byte(sql)...)

	// Bind count (2 bytes BE)
	bindCount := make([]byte, 2)
	binary.BigEndian.PutUint16(bindCount, uint16(len(binds)))
	buf = append(buf, bindCount...)

	// Bind values
	for _, v := range binds {
		buf = append(buf, encodeVarLen(uint32(len(v)))...)
		buf = append(buf, []byte(v)...)
	}

	return buf
}

// buildOALL8WithNulls creates an OALL8 payload with mixed NULL and non-NULL binds.
func buildOALL8WithNulls(sql string, binds []interface{}, cursorID uint16) []byte {
	buf := make([]byte, 0, 64)

	buf = append(buf, byte(TTCFuncOALL8))
	buf = append(buf, 0x00, 0x00, 0x00, 0x00) // options
	cidBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(cidBuf, cursorID)
	buf = append(buf, cidBuf...)

	buf = append(buf, encodeVarLen(uint32(len(sql)))...)
	buf = append(buf, []byte(sql)...)

	bindCount := make([]byte, 2)
	binary.BigEndian.PutUint16(bindCount, uint16(len(binds)))
	buf = append(buf, bindCount...)

	for _, v := range binds {
		if v == nil {
			buf = append(buf, 0x00) // length 0 = NULL
		} else {
			s := fmt.Sprintf("%v", v)
			buf = append(buf, encodeVarLen(uint32(len(s)))...)
			buf = append(buf, []byte(s)...)
		}
	}

	return buf
}

// buildOALL8WithBinaryBind creates an OALL8 payload with a binary bind value.
func buildOALL8WithBinaryBind(sql string, binaryVal []byte, cursorID uint16) []byte {
	buf := make([]byte, 0, 64)

	buf = append(buf, byte(TTCFuncOALL8))
	buf = append(buf, 0x00, 0x00, 0x00, 0x00) // options
	cidBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(cidBuf, cursorID)
	buf = append(buf, cidBuf...)

	buf = append(buf, encodeVarLen(uint32(len(sql)))...)
	buf = append(buf, []byte(sql)...)

	bindCount := make([]byte, 2)
	binary.BigEndian.PutUint16(bindCount, 1)
	buf = append(buf, bindCount...)

	// Binary value with non-printable bytes
	buf = append(buf, encodeVarLen(uint32(len(binaryVal)))...)
	buf = append(buf, binaryVal...)

	return buf
}

// buildOFETCH creates a well-formed OFETCH TTC payload for testing.
func buildOFETCH(cursorID uint16, fetchSize uint32) []byte {
	buf := make([]byte, ofetchMinPayloadSize)
	buf[0] = byte(TTCFuncOFETCH)
	binary.BigEndian.PutUint16(buf[1:3], cursorID)
	binary.BigEndian.PutUint32(buf[3:7], fetchSize)

	return buf
}

// encodeVarLen encodes a length value using TTC variable-length encoding.
func encodeVarLen(val uint32) []byte {
	switch {
	case val < uint32(oall8LenShort):
		return []byte{byte(val)}
	case val <= 0xFFFF:
		buf := make([]byte, 3)
		buf[0] = oall8LenShort
		binary.BigEndian.PutUint16(buf[1:3], uint16(val))

		return buf
	default:
		buf := make([]byte, 5)
		buf[0] = oall8LenLong
		binary.BigEndian.PutUint32(buf[1:5], val)

		return buf
	}
}

func TestDecodeOALL8_SimpleSELECT(t *testing.T) {
	t.Parallel()

	payload := buildOALL8("SELECT * FROM employees WHERE id = :1", []string{"42"}, 7)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, "SELECT * FROM employees WHERE id = :1", result.SQL)
	assert.Equal(t, uint16(7), result.CursorID)
	assert.Equal(t, []string{"42"}, result.BindValues)
}

func TestDecodeOALL8_NoBinds(t *testing.T) {
	t.Parallel()

	payload := buildOALL8("SELECT SYSDATE FROM DUAL", nil, 1)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, "SELECT SYSDATE FROM DUAL", result.SQL)
	assert.Empty(t, result.BindValues)
}

func TestDecodeOALL8_MultipleBinds(t *testing.T) {
	t.Parallel()

	sql := "INSERT INTO t (a, b, c) VALUES (:1, :2, :3)"
	binds := []string{"hello", "42", "2024-01-15"}
	payload := buildOALL8(sql, binds, 3)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, sql, result.SQL)
	assert.Equal(t, binds, result.BindValues)
}

func TestDecodeOALL8_PLSQLBlock(t *testing.T) {
	t.Parallel()

	sql := "BEGIN my_package.do_something(:1, :2); END;"
	payload := buildOALL8(sql, []string{"arg1", "arg2"}, 5)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.True(t, result.IsPLSQL())
	assert.Equal(t, sql, result.SQL)
}

func TestDecodeOALL8_DECLAREBlock(t *testing.T) {
	t.Parallel()

	sql := "DECLARE v NUMBER; BEGIN SELECT 1 INTO v FROM DUAL; END;"
	payload := buildOALL8(sql, nil, 5)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.True(t, result.IsPLSQL())
}

func TestDecodeOALL8_LargeSQL(t *testing.T) {
	t.Parallel()

	sql := "SELECT " + strings.Repeat("col, ", 1000) + "col FROM t"
	payload := buildOALL8(sql, nil, 10)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, sql, result.SQL)
}

func TestDecodeOALL8_EmptySQL(t *testing.T) {
	t.Parallel()

	payload := buildOALL8("", nil, 1)
	_, err := decodeOALL8(payload)
	assert.ErrorIs(t, err, ErrEmptySQL)
}

func TestDecodeOALL8_UnicodeSQL(t *testing.T) {
	t.Parallel()

	sql := "SELECT * FROM données WHERE nom = :1"
	payload := buildOALL8(sql, []string{"Éric"}, 2)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, sql, result.SQL)
	assert.Equal(t, []string{"Éric"}, result.BindValues)
}

func TestDecodeOALL8_NullBindValue(t *testing.T) {
	t.Parallel()

	payload := buildOALL8WithNulls("UPDATE t SET col = :1 WHERE id = :2", []interface{}{nil, 42}, 3)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, "NULL", result.BindValues[0])
	assert.Equal(t, "42", result.BindValues[1])
}

func TestDecodeOALL8_BinaryBindValue(t *testing.T) {
	t.Parallel()

	payload := buildOALL8WithBinaryBind("INSERT INTO t (raw_col) VALUES (:1)", []byte{0xDE, 0xAD, 0xBE, 0xEF}, 4)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, "deadbeef", result.BindValues[0])
}

func TestDecodeOALL8_CorruptPayload(t *testing.T) {
	t.Parallel()

	_, err := decodeOALL8([]byte{0x0E, 0x00, 0x01})
	assert.Error(t, err)
}

func TestDecodeOALL8_ExtendedLengthShort(t *testing.T) {
	t.Parallel()

	// SQL longer than 253 bytes — uses 0xFE + uint16 encoding
	sql := strings.Repeat("X", 300)
	payload := buildOALL8(sql, nil, 1)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, sql, result.SQL)
}

func TestDecodeOALL8_ExtendedLengthLong(t *testing.T) {
	t.Parallel()

	// SQL longer than 65535 bytes — uses 0xFF + uint32 encoding
	sql := strings.Repeat("Y", 70000)
	payload := buildOALL8(sql, nil, 1)
	result, err := decodeOALL8(payload)
	require.NoError(t, err)
	assert.Equal(t, sql, result.SQL)
}

func TestDecodeOALL8_FuzzInputs(t *testing.T) {
	t.Parallel()

	// Ensure various random-ish payloads don't panic
	inputs := [][]byte{
		nil,
		{},
		{0x0E},
		{0x0E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
		{0x0E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xFF},
		{0x0E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x03, 'A', 'B', 'C'},
		// Extended length but truncated
		{0x0E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xFE, 0x01},
	}
	for i, input := range inputs {
		// Must not panic
		_, _ = decodeOALL8(input)
		_ = i
	}
}

func TestDecodeOFETCH(t *testing.T) {
	t.Parallel()

	payload := buildOFETCH(7, 100)
	result, err := decodeOFETCH(payload)
	require.NoError(t, err)
	assert.Equal(t, uint16(7), result.CursorID)
	assert.Equal(t, uint32(100), result.FetchSize)
}

func TestDecodeOFETCH_TooShort(t *testing.T) {
	t.Parallel()

	_, err := decodeOFETCH([]byte{0x11, 0x00})
	assert.ErrorIs(t, err, ErrOFETCHTooShort)
}

func TestDecodeOFETCH_LargeFetchSize(t *testing.T) {
	t.Parallel()

	payload := buildOFETCH(1, 10000)
	result, err := decodeOFETCH(payload)
	require.NoError(t, err)
	assert.Equal(t, uint32(10000), result.FetchSize)
}

func TestDecodeVarLen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []byte
		wantVal  uint32
		wantRead int
	}{
		{"single byte 0", []byte{0}, 0, 1},
		{"single byte 100", []byte{100}, 100, 1},
		{"single byte 253", []byte{253}, 253, 1},
		{"short extended 300", encodeVarLen(300), 300, 3},
		{"short extended 65535", encodeVarLen(65535), 65535, 3},
		{"long extended 70000", encodeVarLen(70000), 70000, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			val, read, err := decodeVarLen(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.wantVal, val)
			assert.Equal(t, tt.wantRead, read)
		})
	}
}

func TestIsPLSQL(t *testing.T) {
	t.Parallel()

	assert.True(t, (&OALL8Result{SQL: "BEGIN proc; END;"}).IsPLSQL())
	assert.True(t, (&OALL8Result{SQL: "DECLARE v NUMBER; BEGIN NULL; END;"}).IsPLSQL())
	assert.True(t, (&OALL8Result{SQL: "  begin proc; end;  "}).IsPLSQL())
	assert.False(t, (&OALL8Result{SQL: "SELECT 1 FROM DUAL"}).IsPLSQL())
	assert.False(t, (&OALL8Result{SQL: "INSERT INTO t VALUES (1)"}).IsPLSQL())
}

// --- TTC Response test helpers ---

// buildTTCResponse creates a TTC response payload with column definitions and rows.
func buildTTCResponse(cols []columnDef, rows [][]interface{}) []byte {
	buf := make([]byte, 0, 128)

	// Function code (Response)
	buf = append(buf, byte(TTCFuncResponse))
	// Sequence number
	buf = append(buf, 0x01)
	// Error code (0 = success)
	buf = append(buf, 0x00, 0x00, 0x00, 0x00)
	// Cursor ID
	buf = append(buf, 0x00, 0x01)
	// Row count
	rowCount := make([]byte, 4)
	binary.BigEndian.PutUint32(rowCount, uint32(len(rows)))
	buf = append(buf, rowCount...)
	// Error flag (0)
	buf = append(buf, 0x00, 0x00)

	// Column count
	colCount := make([]byte, 2)
	binary.BigEndian.PutUint16(colCount, uint16(len(cols)))
	buf = append(buf, colCount...)

	// Column definitions
	for _, col := range cols {
		// Name
		buf = append(buf, encodeVarLen(uint32(len(col.Name)))...)
		buf = append(buf, []byte(col.Name)...)
		// Type code
		buf = append(buf, col.TypeCode)
		// Size
		sizeBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(sizeBuf, col.Size)
		buf = append(buf, sizeBuf...)
		// Precision
		buf = append(buf, col.Precision)
		// Scale
		buf = append(buf, col.Scale)
		// Nullable
		if col.Nullable {
			buf = append(buf, 0x01)
		} else {
			buf = append(buf, 0x00)
		}
	}

	// Row data
	for _, row := range rows {
		for _, val := range row {
			if val == nil {
				buf = append(buf, 0x00) // NULL
				continue
			}
			valStr := fmt.Sprintf("%v", val)
			buf = append(buf, encodeVarLen(uint32(len(valStr)))...)
			buf = append(buf, []byte(valStr)...)
		}
	}

	// More-data flag (false)
	buf = append(buf, 0x00)

	return buf
}

// buildTTCErrorResponse creates a TTC error response payload.
func buildTTCErrorResponse(errCode int, errMsg string) []byte {
	buf := make([]byte, 0, 64)

	// Function code (Response)
	buf = append(buf, byte(TTCFuncResponse))
	// Sequence number
	buf = append(buf, 0x01)
	// Error code
	errCodeBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(errCodeBuf, uint32(errCode))
	buf = append(buf, errCodeBuf...)
	// Cursor ID
	buf = append(buf, 0x00, 0x00)
	// Row count (0)
	buf = append(buf, 0x00, 0x00, 0x00, 0x00)
	// Error flag (non-zero)
	buf = append(buf, 0x00, 0x01)
	// Error message length + message
	msgLenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(msgLenBuf, uint16(len(errMsg)))
	buf = append(buf, msgLenBuf...)
	buf = append(buf, []byte(errMsg)...)

	return buf
}

// buildTTCResponseWithMoreData creates a TTC response with the more-data flag set.
func buildTTCResponseWithMoreData(moreData bool) []byte {
	buf := make([]byte, 0, 32)

	buf = append(buf, byte(TTCFuncResponse))
	buf = append(buf, 0x01)
	buf = append(buf, 0x00, 0x00, 0x00, 0x00) // no error
	buf = append(buf, 0x00, 0x01)             // cursor
	buf = append(buf, 0x00, 0x00, 0x00, 0x00) // row count
	buf = append(buf, 0x00, 0x00)             // error flag

	// Column count = 0
	buf = append(buf, 0x00, 0x00)

	// More-data flag
	if moreData {
		buf = append(buf, 0x01)
	} else {
		buf = append(buf, 0x00)
	}

	return buf
}

func TestDecodeTTCResponse_ColumnDefinitions(t *testing.T) {
	t.Parallel()

	resp := buildTTCResponse(
		[]columnDef{
			{Name: "ID", TypeCode: OracleTypeNUMBER, Size: 22},
			{Name: "NAME", TypeCode: OracleTypeVARCHAR2, Size: 100},
			{Name: "CREATED", TypeCode: OracleTypeDATE, Size: 7},
		},
		nil,
	)
	result, err := decodeTTCResponse(resp)
	require.NoError(t, err)
	assert.Len(t, result.Columns, 3)
	assert.Equal(t, "ID", result.Columns[0].Name)
	assert.Equal(t, OracleTypeNUMBER, result.Columns[0].TypeCode)
	assert.Equal(t, "NAME", result.Columns[1].Name)
	assert.Equal(t, OracleTypeVARCHAR2, result.Columns[1].TypeCode)
	assert.Equal(t, "CREATED", result.Columns[2].Name)
	assert.Equal(t, OracleTypeDATE, result.Columns[2].TypeCode)
}

func TestDecodeTTCResponse_WithRows(t *testing.T) {
	t.Parallel()

	resp := buildTTCResponse(
		[]columnDef{{Name: "ID", TypeCode: OracleTypeVARCHAR2}, {Name: "NAME", TypeCode: OracleTypeVARCHAR2}},
		[][]interface{}{{"1", "Alice"}, {"2", "Bob"}},
	)
	result, err := decodeTTCResponse(resp)
	require.NoError(t, err)
	assert.Len(t, result.Rows, 2)
}

func TestDecodeTTCResponse_ErrorResponse(t *testing.T) {
	t.Parallel()

	resp := buildTTCErrorResponse(942, "ORA-00942: table or view does not exist")
	result, err := decodeTTCResponse(resp)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, 942, result.ErrorCode)
	assert.Contains(t, result.ErrorMessage, "ORA-00942")
}

func TestDecodeTTCResponse_MoreDataFlag(t *testing.T) {
	t.Parallel()

	resp := buildTTCResponseWithMoreData(true)
	result, err := decodeTTCResponse(resp)
	require.NoError(t, err)
	assert.True(t, result.MoreData)
}

func TestDecodeTTCResponse_NoMoreData(t *testing.T) {
	t.Parallel()

	resp := buildTTCResponseWithMoreData(false)
	result, err := decodeTTCResponse(resp)
	require.NoError(t, err)
	assert.False(t, result.MoreData)
}

func TestDecodeTTCResponse_SuccessNoError(t *testing.T) {
	t.Parallel()

	resp := buildTTCResponse(nil, nil)
	result, err := decodeTTCResponse(resp)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, 0, result.ErrorCode)
}

func TestDecodeTTCResponse_TooShort(t *testing.T) {
	t.Parallel()

	_, err := decodeTTCResponse([]byte{0x08, 0x01})
	assert.Error(t, err)
}

func TestDecodeColumnDef(t *testing.T) {
	t.Parallel()

	data := make([]byte, 0, 32)
	data = append(data, encodeVarLen(4)...)
	data = append(data, []byte("NAME")...)
	data = append(data, OracleTypeVARCHAR2)
	sizeBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(sizeBuf, 100)
	data = append(data, sizeBuf...)
	data = append(data, 0)    // precision
	data = append(data, 0)    // scale
	data = append(data, 0x01) // nullable

	col, bytesRead, err := decodeColumnDef(data)
	require.NoError(t, err)
	assert.Equal(t, "NAME", col.Name)
	assert.Equal(t, OracleTypeVARCHAR2, col.TypeCode)
	assert.Equal(t, uint32(100), col.Size)
	assert.True(t, col.Nullable)
	assert.Equal(t, len(data), bytesRead)
}
