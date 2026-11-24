package oracle

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// utf16BE encodes s the way a server sends a national-character value.
func utf16BE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(units))

	for i, u := range units {
		binary.BigEndian.PutUint16(out[2*i:], u)
	}

	return out
}

// syntheticDescribe builds a classic-layout describe of VARCHAR2 columns named
// names, the column at index national having charset form 2.
func syntheticDescribe(names []string, national int) []byte {
	ci := func(v int) []byte {
		if v == 0 {
			return []byte{0}
		}

		return []byte{1, byte(v)}
	}

	out := []byte{byte(TTCFuncQueryResult), 0} // size 0: no prefix
	out = append(out, ci(100)...)              // maxRowSize
	out = append(out, ci(len(names))...)
	out = append(out, 0) // skip byte

	for i, name := range names {
		csfrm := byte(1)
		if i == national {
			csfrm = charsetFormNational
		}

		out = append(out, tnsTypeVARCHAR, 0, 0, 0) // type, flag, precision, scale
		out = append(out, ci(40)...)               // maxLen
		out = append(out, ci(0)...)                // maxNoOfArrayElements
		out = append(out, ci(0)...)                // contFlag
		out = append(out, ci(0)...)                // toID
		out = append(out, ci(0)...)                // version
		out = append(out, ci(1)...)                // charsetID
		out = append(out, csfrm)
		out = append(out, ci(0)...) // maxCharLen
		out = append(out, ci(0)...) // oaccollid
		out = append(out, 1, 0)     // allowNull, v7 name length
		out = append(out, ci(len(name))...)
		out = append(out, byte(len(name)))
		out = append(out, name...)
		out = append(out, ci(0)...) // schema
		out = append(out, ci(0)...) // type name
		out = append(out, ci(0)...) // trailing ints
		out = append(out, ci(0)...)
	}

	return out
}

func TestParseColumnDescribes_NationalCharsetForm(t *testing.T) {
	t.Parallel()

	descs := parseColumnDescribes(syntheticDescribe([]string{"NAME", "LABEL"}, 1), oerShape{})
	require.Len(t, descs, 2)
	assert.False(t, descs[0].National)
	assert.True(t, descs[1].National)
	assert.Equal(t, []int{tnsTypeVARCHAR, tnsTypeVARCHAR | colTypeNational}, describeRowTypes(descs))
	// The plain slice, which the define-block matching compares, is unmarked.
	assert.Equal(t, []int{tnsTypeVARCHAR, tnsTypeVARCHAR}, describeColumnTypes(descs))

	result := decodeQueryResultV2(syntheticDescribe([]string{"NAME", "LABEL"}, 1), oerShape{}, lobRowLocator)
	require.NotNil(t, result)
	assert.Equal(t, []bool{false, true}, result.ColumnNational)
}

func TestDecodeRowValue_NationalCharset(t *testing.T) {
	t.Parallel()

	national := []int{tnsTypeVARCHAR | colTypeNational}
	value := utf16BE("Bâtiment 1")

	assert.Equal(t, "Bâtiment 1", decodeRowValue(national, 0, value))
	assert.Equal(t, "abc", decodeRowValue(national, 0, utf16BE("abc")))

	// Not a guess: without the describe's mark the value stays hex.
	assert.Equal(t, hex.EncodeToString(value), decodeRowValue(nil, 0, value))
	assert.Equal(t, hex.EncodeToString(value), decodeRowValue([]int{tnsTypeVARCHAR}, 0, value))

	// A national column holding bytes that are not UTF-16 falls back.
	assert.Equal(t, "abc", decodeRowValue(national, 0, []byte("abc")))
}

func TestRowColumnTypes_CarriesNationalMark(t *testing.T) {
	t.Parallel()

	columns := []columnDef{{TypeCode: tnsTypeVARCHAR}, {TypeCode: tnsTypeVARCHAR, National: true}}

	assert.Equal(t, []int{tnsTypeVARCHAR, tnsTypeVARCHAR | colTypeNational}, rowColumnTypes(columns))
	assert.Equal(t, []int{tnsTypeVARCHAR, tnsTypeVARCHAR}, columnTypeCodes(columns))
	assert.Equal(t, tnsTypeVARCHAR, colType(rowColumnTypes(columns), 1))
}
