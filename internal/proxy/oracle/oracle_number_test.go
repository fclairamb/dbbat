package oracle

import (
	"testing"

	goora "github.com/sijms/go-ora/v2/converters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecodeOracleNumber checks the NUMBER decoder against hand-encoded byte
// sequences and cross-checks every case against go-ora's canonical decoder, so
// a wrong hand-encoding can't make a buggy decoder look correct.
func TestDecodeOracleNumberToString_Goora(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		bytes []byte
		want  string
	}{
		{"zero", []byte{0x80}, "0"},
		{"one", []byte{0xc1, 0x02}, "1"},
		{"forty_two", []byte{0xc1, 0x2b}, "42"},
		{"hundred", []byte{0xc2, 0x02}, "100"},
		{"pi_2dp", []byte{0xc1, 0x04, 0x0f}, "3.14"},
		{"twelve_point_five", []byte{0xc1, 0x0d, 0x33}, "12.5"},
		{"half", []byte{0xc0, 0x33}, "0.5"},
		{"big", []byte{0xc5, 0x0d, 0x23, 0x39, 0x4f, 0x5b}, "1234567890"},
		{"neg_forty_two", []byte{0x3e, 0x3b, 0x66}, "-42"},
		{"neg_pi", []byte{0x3e, 0x62, 0x57, 0x66}, "-3.14"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Ground truth: the reference driver's decoder.
			ref, err := goora.NumberToString(tc.bytes)
			require.NoError(t, err)
			require.Equalf(t, tc.want, ref, "hand-encoded bytes for %s are wrong", tc.name)

			got, ok := decodeOracleNumberToString(tc.bytes)
			require.True(t, ok, "decoder rejected a valid NUMBER")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDecodeRowValue_NumberTypeFixesNegatives shows why type-aware decoding
// matters: a negative NUMBER's bytes are all printable ASCII, so the type-less
// heuristic mis-reads them as text, while decoding by the NUMBER column type
// recovers the real value.
func TestDecodeRowValue_NumberTypeFixesNegatives(t *testing.T) {
	t.Parallel()

	negFortyTwo := []byte{0x3e, 0x3b, 0x66} // Oracle NUMBER -42

	// Heuristic (no type) is fooled — the bytes look like printable ASCII.
	assert.NotEqual(t, "-42", decodeOracleRawValue(negFortyTwo))

	// With the NUMBER column type, decodeRowValue gets it right.
	assert.Equal(t, "-42", decodeRowValue([]int{tnsTypeNUMBER}, 0, negFortyTwo))

	// No type available → falls back to the heuristic.
	assert.Equal(t, decodeOracleRawValue(negFortyTwo), decodeRowValue(nil, 0, negFortyTwo))
}

// TestDecodeOracleNumber_RejectsText ensures the validity gate keeps the decoder
// from misreading printable strings as numbers (the proxy has no column type to
// disambiguate, so decodeOracleRawValue tries ASCII first and relies on this).
func TestDecodeOracleNumber_RejectsText(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"abc", "SELECT", "hello", "id", "name"} {
		_, ok := decodeOracleNumberToString([]byte(s))
		assert.Falsef(t, ok, "text %q must not decode as a number", s)
	}
}
