package mssql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseTypeInfoFraming pins the part that has to be exact: how many bytes a
// TYPE_INFO occupies and how its values are framed. A wrong length here loses
// the whole token stream; a wrong interpretation only makes one cell ugly.
func TestParseTypeInfoFraming(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		encoded  []byte
		wantKind valueKind
		wantSize int
		wantLen  int // bytes the TYPE_INFO itself occupies
	}{
		{"int", []byte{typeInt4}, kindFixed, 4, 1},
		{"bigint", []byte{typeInt8}, kindFixed, 8, 1},
		{"nullable int", []byte{typeIntN, 4}, kindByte, 4, 2},
		{"uniqueidentifier", []byte{typeGUID, 16}, kindByte, 16, 2},
		{"decimal(18,4)", []byte{typeDecimalN, 9, 18, 4}, kindByte, 9, 4},
		{"date", []byte{typeDateN}, kindByte, 0, 1},
		{"time(7)", []byte{typeTimeN, 7}, kindByte, 0, 2},
		{"datetime2(3)", []byte{typeDateTime2N, 3}, kindByte, 0, 2},
		{"datetimeoffset(7)", []byte{typeDateTimeOffsetN, 7}, kindByte, 0, 2},
		{"varbinary(50)", []byte{typeBigVarBinary, 50, 0}, kindUShort, 50, 3},
		{"varbinary(max)", []byte{typeBigVarBinary, 0xFF, 0xFF}, kindPLP, 0xFFFF, 3},
		{"nvarchar(100)", []byte{typeNVarChar, 200, 0, 0, 0, 0, 0, 0}, kindUShort, 200, 8},
		{"nvarchar(max)", []byte{typeNVarChar, 0xFF, 0xFF, 0, 0, 0, 0, 0}, kindPLP, 0xFFFF, 8},
		{"varchar(30)", []byte{typeBigVarChar, 30, 0, 0, 0, 0, 0, 0}, kindUShort, 30, 8},
		{"text", []byte{typeText, 0x10, 0, 0, 0, 0, 0, 0, 0, 0}, kindTextPtr, 16, 10},
		{"image", []byte{typeImage, 0x10, 0, 0, 0}, kindTextPtr, 16, 5},
		{"sql_variant", []byte{typeVariant, 0x10, 0, 0, 0}, kindVariant, 16, 5},
		{"xml without a schema", []byte{typeXML, 0x00}, kindPLP, 0, 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			info, next, err := parseTypeInfo(tc.encoded, 0)
			require.NoError(t, err)
			assert.Equal(t, tc.wantKind, info.kind)
			assert.Equal(t, tc.wantSize, info.size)
			assert.Equal(t, tc.wantLen, next, "the TYPE_INFO must consume exactly its own bytes")
		})
	}
}

// TestParseTypeInfoRejectsAnUnknownType: guessing a length is what mis-reads
// everything after it, so an unmodelled type stops the walk instead.
func TestParseTypeInfoRejectsAnUnknownType(t *testing.T) {
	t.Parallel()

	_, _, err := parseTypeInfo([]byte{0x01}, 0)
	require.ErrorIs(t, err, ErrUnknownDataType)
}

// TestReadValueFraming covers the value encodings, NULLs included.
func TestReadValueFraming(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		info     typeInfo
		encoded  []byte
		wantNull bool
		wantRaw  []byte
		wantNext int
	}{
		{
			name: "fixed int", info: typeInfo{id: typeInt4, kind: kindFixed, size: 4},
			encoded: []byte{1, 0, 0, 0, 0xFF}, wantRaw: []byte{1, 0, 0, 0}, wantNext: 4,
		},
		{
			name: "byte-length null", info: typeInfo{id: typeIntN, kind: kindByte, size: 4},
			encoded: []byte{0, 0xFF}, wantNull: true, wantNext: 1,
		},
		{
			name: "ushort-length null", info: typeInfo{id: typeNVarChar, kind: kindUShort},
			encoded: []byte{0xFF, 0xFF}, wantNull: true, wantNext: 2,
		},
		{
			name: "ushort-length empty string", info: typeInfo{id: typeNVarChar, kind: kindUShort},
			encoded: []byte{0x00, 0x00}, wantRaw: []byte{}, wantNext: 2,
		},
		{
			name: "PLP null", info: typeInfo{id: typeNVarChar, kind: kindPLP},
			encoded:  []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			wantNull: true, wantNext: 8,
		},
		{
			name: "PLP in two chunks", info: typeInfo{id: typeBigVarBinary, kind: kindPLP},
			encoded: []byte{
				4, 0, 0, 0, 0, 0, 0, 0, // total
				2, 0, 0, 0, 0xAA, 0xBB, // chunk 1
				2, 0, 0, 0, 0xCC, 0xDD, // chunk 2
				0, 0, 0, 0, // terminator
			},
			wantRaw: []byte{0xAA, 0xBB, 0xCC, 0xDD}, wantNext: 24,
		},
		{
			name: "PLP of unknown length", info: typeInfo{id: typeBigVarBinary, kind: kindPLP},
			encoded: []byte{
				0xFE, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
				1, 0, 0, 0, 0x2A,
				0, 0, 0, 0,
			},
			wantRaw: []byte{0x2A}, wantNext: 17,
		},
		{
			name: "text pointer null", info: typeInfo{id: typeText, kind: kindTextPtr},
			encoded: []byte{0}, wantNull: true, wantNext: 1,
		},
		{
			name: "text pointer value", info: typeInfo{id: typeText, kind: kindTextPtr},
			encoded: append(append([]byte{2, 0xAB, 0xCD}, make([]byte, 8)...),
				3, 0, 0, 0, 'a', 'b', 'c'),
			wantRaw: []byte("abc"), wantNext: 18,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, isNull, next, err := readValue(tc.info, tc.encoded, 0)
			require.NoError(t, err)
			assert.Equal(t, tc.wantNull, isNull)
			assert.Equal(t, tc.wantNext, next, "the value must consume exactly its own bytes")

			if !tc.wantNull {
				assert.Equal(t, tc.wantRaw, raw)
			}
		})
	}
}

// TestJSONValueForRendersTypes covers the interpretation half: best-effort, but
// it should still be right for everything a normal result set is made of.
func TestJSONValueForRendersTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info typeInfo
		raw  []byte
		want any
	}{
		{"tinyint", typeInfo{id: typeIntN}, []byte{200}, int64(200)},
		{"smallint", typeInfo{id: typeIntN}, []byte{0xFF, 0xFF}, int64(-1)},
		{"int", typeInfo{id: typeInt4}, []byte{0x2A, 0, 0, 0}, int64(42)},
		{"bigint", typeInfo{id: typeInt8}, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, int64(-1)},
		{"bit", typeInfo{id: typeBitN}, []byte{1}, true},
		{"float", typeInfo{id: typeFlt8}, []byte{0, 0, 0, 0, 0, 0, 0xF0, 0x3F}, 1.0},
		{"smallmoney", typeInfo{id: typeMoney4}, []byte{0x10, 0x27, 0, 0}, "1.0000"},
		{
			"money",
			typeInfo{id: typeMoney},
			[]byte{0, 0, 0, 0, 0x10, 0x27, 0, 0},
			"1.0000",
		},
		{
			"decimal(10,2)",
			typeInfo{id: typeDecimalN, scale: 2},
			[]byte{1, 0x39, 0x30, 0, 0},
			"123.45",
		},
		{
			"negative decimal",
			typeInfo{id: typeDecimalN, scale: 2},
			[]byte{0, 0x39, 0x30, 0, 0},
			"-123.45",
		},
		{
			"uniqueidentifier",
			typeInfo{id: typeGUID},
			[]byte{0x78, 0x56, 0x34, 0x12, 0x34, 0x12, 0x56, 0x34, 0x9A, 0xBC, 1, 2, 3, 4, 5, 6},
			"12345678-1234-3456-9abc-010203040506",
		},
		{"nvarchar", typeInfo{id: typeNVarChar}, stringToUCS2("café ☕"), "café ☕"},
		{"varchar utf-8", typeInfo{id: typeBigVarChar}, []byte("plain"), "plain"},
		{"varchar latin-1", typeInfo{id: typeBigVarChar}, []byte{0xE9, 't', 0xE9}, "été"},
		{"date", typeInfo{id: typeDateN}, []byte{0xFA, 0x49, 0x0B}, "2026-08-07"},
		{
			"datetime",
			typeInfo{id: typeDateTimeN},
			[]byte{0x9F, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			"2026-08-07T00:00:00",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, jsonValueFor(tc.info, tc.raw, false))
		})
	}
}

// TestJSONValueForBinaryIsTagged: a binary blob becomes a tagged base64 object,
// the same shape the MySQL proxy uses, rather than mojibake.
func TestJSONValueForBinaryIsTagged(t *testing.T) {
	t.Parallel()

	value := jsonValueFor(typeInfo{id: typeBigVarBinary}, []byte{0x00, 0xFF}, false)

	tagged, ok := value.(map[string]string)
	require.True(t, ok)
	assert.Equal(t, "AP8=", tagged["$bytes"])
	assert.Equal(t, "varbinary", tagged["$type"])
}

// TestJSONValueForNull covers both ways a value can be absent.
func TestJSONValueForNull(t *testing.T) {
	t.Parallel()

	assert.Nil(t, jsonValueFor(typeInfo{id: typeInt4}, []byte{1, 0, 0, 0}, true))
	assert.Nil(t, jsonValueFor(typeInfo{id: typeInt4}, nil, false))
}
