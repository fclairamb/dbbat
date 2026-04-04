package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildTTCDataPayload(funcCode byte, extra []byte) []byte {
	// TNS Data payload: [data_flags: 2 bytes] [func_code: 1 byte] [extra...]
	payload := []byte{0x00, 0x00, funcCode}
	if len(extra) > 0 {
		payload = append(payload, extra...)
	}

	return payload
}

func TestParseTTCFunctionCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
		want    TTCFunctionCode
	}{
		{"OALL8", buildTTCDataPayload(0x0E, []byte{0x01, 0x02}), TTCFuncOALL8},
		{"OFETCH", buildTTCDataPayload(0x11, []byte{0x01}), TTCFuncOFETCH},
		{"OCLOSE", buildTTCDataPayload(0x05, []byte{0x01}), TTCFuncOCLOSE},
		{"Piggyback", buildTTCDataPayload(0x03, []byte{0x5e}), TTCFuncPiggyback},
		{"Response", buildTTCDataPayload(0x08, []byte{0x01}), TTCFuncResponse},
		{"OCANCEL", buildTTCDataPayload(0x14, []byte{0x01}), TTCFuncOCANCEL},
		{"QueryResult", buildTTCDataPayload(0x10, []byte{0x01}), TTCFuncQueryResult},
		{"SetProtocol", buildTTCDataPayload(0x01, []byte{0x01}), TTCFuncSetProtocol},
		{"SetDataTypes", buildTTCDataPayload(0x02, []byte{0x01}), TTCFuncSetDataTypes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fc, err := parseTTCFunctionCode(tt.payload)
			require.NoError(t, err)
			assert.Equal(t, tt.want, fc)
		})
	}
}

func TestParseTTCFunctionCode_EmptyPayload(t *testing.T) {
	t.Parallel()
	_, err := parseTTCFunctionCode([]byte{})
	assert.ErrorIs(t, err, ErrTTCPayloadTooShort)
}

func TestParseTTCFunctionCode_TooShort(t *testing.T) {
	t.Parallel()
	_, err := parseTTCFunctionCode([]byte{0x00, 0x00}) // only flags, no func code
	assert.ErrorIs(t, err, ErrTTCPayloadTooShort)
}

func TestParseTTCFunctionCode_DataFlagsPrefixed(t *testing.T) {
	t.Parallel()
	payload := []byte{0x00, 0x00, 0x0E} // flags=0, func=OALL8
	fc, err := parseTTCFunctionCode(payload)
	require.NoError(t, err)
	assert.Equal(t, TTCFuncOALL8, fc)
}

func TestParseTTCFunctionCode_UnknownCode(t *testing.T) {
	t.Parallel()
	payload := buildTTCDataPayload(0xFE, nil)
	fc, err := parseTTCFunctionCode(payload)
	require.NoError(t, err)
	assert.Equal(t, TTCFunctionCode(0xFE), fc)
}

func TestTTCFunctionCode_Stringer(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "OALL8", TTCFuncOALL8.String())
	assert.Equal(t, "OFETCH", TTCFuncOFETCH.String())
	assert.Equal(t, "OCLOSE", TTCFuncOCLOSE.String())
	assert.Equal(t, "Response", TTCFuncResponse.String())
	assert.Equal(t, "OSETPRO", TTCFuncSetProtocol.String())
	assert.Equal(t, "ODTYPES", TTCFuncSetDataTypes.String())
	assert.Equal(t, "PIGGYBACK", TTCFuncPiggyback.String())
	assert.Equal(t, "QRESULT", TTCFuncQueryResult.String())
	assert.Equal(t, "0xfe", TTCFunctionCode(0xFE).String())
}

func TestIsPiggybackExecSQL(t *testing.T) {
	t.Parallel()

	// func=0x03, sub=0x5e → true
	assert.True(t, IsPiggybackExecSQL([]byte{0x03, 0x5e, 0x01}))

	// func=0x03, sub=0x76 (auth) → false
	assert.False(t, IsPiggybackExecSQL([]byte{0x03, 0x76, 0x01}))

	// Too short → false
	assert.False(t, IsPiggybackExecSQL([]byte{0x03}))
}

func TestIsPiggybackClose(t *testing.T) {
	t.Parallel()

	assert.True(t, IsPiggybackClose([]byte{0x03, 0x09, 0x05}))
	assert.False(t, IsPiggybackClose([]byte{0x03, 0x5e, 0x01}))
}

func TestDecodePiggybackExecSQL(t *testing.T) {
	t.Parallel()

	t.Run("SELECT 1 FROM DUAL", func(t *testing.T) {
		t.Parallel()
		// Real captured payload from oracledb thin client → Oracle 19c
		payload, _ := hexDecode("035e030280610001011201010d0000000102047fffffff0000000000000000000000010000000000000000000000000000001253454c45435420312046524f4d204455414c0101000000000000010100")
		result, err := decodePiggybackExecSQL(payload)
		require.NoError(t, err)
		assert.Equal(t, "SELECT 1 FROM DUAL", result.SQL)
	})

	t.Run("SELECT COUNT query", func(t *testing.T) {
		t.Parallel()
		// Captured: SELECT COUNT(*) FROM all_users
		payload, _ := hexDecode("035e040280610001011e01010d0000000102047fffffff0000000000000000000000010000000000000000000000000000001e53454c45435420434f554e54282a292046524f4d20616c6c5f7573657273010100000000000001010002800000000000")
		result, err := decodePiggybackExecSQL(payload)
		require.NoError(t, err)
		assert.Equal(t, "SELECT COUNT(*) FROM all_users", result.SQL)
	})

	t.Run("too short payload", func(t *testing.T) {
		t.Parallel()
		_, err := decodePiggybackExecSQL([]byte{0x03, 0x5e, 0x01})
		assert.Error(t, err)
	})
}

func hexDecode(s string) ([]byte, error) { //nolint:unparam // error kept for API consistency
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var v byte
		for j := 0; j < 2; j++ {
			c := s[i+j]
			switch {
			case c >= '0' && c <= '9':
				v = v*16 + (c - '0')
			case c >= 'a' && c <= 'f':
				v = v*16 + (c - 'a' + 10)
			case c >= 'A' && c <= 'F':
				v = v*16 + (c - 'A' + 10)
			}
		}
		b[i/2] = v
	}

	return b, nil
}
