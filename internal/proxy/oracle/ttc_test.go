package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTTCDataPayload creates a TNS Data payload with data flags + function code + extra data.
func buildTTCDataPayload(funcCode byte, extra []byte) []byte {
	payload := make([]byte, ttcDataFlagsSize+1+len(extra))
	// data flags = 0x0000
	payload[ttcDataFlagsSize] = funcCode

	if len(extra) > 0 {
		copy(payload[ttcDataFlagsSize+1:], extra)
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
		{"OAUTH", buildTTCDataPayload(0x5E, []byte{0x01}), TTCFuncOAUTH},
		{"OOPEN", buildTTCDataPayload(0x03, []byte{0x01}), TTCFuncOOPEN},
		{"Response", buildTTCDataPayload(0x08, []byte{0x01}), TTCFuncResponse},
		{"OCANCEL", buildTTCDataPayload(0x14, []byte{0x01}), TTCFuncOCANCEL},
		{"OLOBOPS", buildTTCDataPayload(0x44, []byte{0x01}), TTCFuncOLOBOPS},
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
	assert.False(t, fc.IsKnown())
}

func TestTTCFunctionCode_Stringer(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "OALL8", TTCFuncOALL8.String())
	assert.Equal(t, "OFETCH", TTCFuncOFETCH.String())
	assert.Equal(t, "OCLOSE", TTCFuncOCLOSE.String())
	assert.Equal(t, "Response", TTCFuncResponse.String())
	assert.Equal(t, "OSETPRO", TTCFuncSetProtocol.String())
	assert.Equal(t, "ODTYPES", TTCFuncSetDataTypes.String())
	assert.Equal(t, "UNKNOWN(0xfe)", TTCFunctionCode(0xFE).String())
}

func TestTTCFunctionCode_IsKnown(t *testing.T) {
	t.Parallel()
	knownCodes := []TTCFunctionCode{
		TTCFuncSetProtocol, TTCFuncSetDataTypes, TTCFuncOOPEN, TTCFuncOCLOSE,
		TTCFuncResponse, TTCFuncOMarker, TTCFuncOVersion, TTCFuncOALL8,
		TTCFuncOFETCH, TTCFuncOCANCEL, TTCFuncOLOBOPS, TTCFuncOSQL7,
		TTCFuncOAUTH, TTCFuncOSESSKEY,
	}
	for _, fc := range knownCodes {
		assert.True(t, fc.IsKnown(), "should be known: %s", fc)
	}
	assert.False(t, TTCFunctionCode(0xFF).IsKnown())
}

func TestExtractTTCPayload(t *testing.T) {
	t.Parallel()
	payload := buildTTCDataPayload(0x0E, []byte{0xAA, 0xBB})
	ttcPayload := extractTTCPayload(payload)
	require.NotNil(t, ttcPayload)
	assert.Equal(t, byte(0x0E), ttcPayload[0]) // function code
	assert.Equal(t, byte(0xAA), ttcPayload[1])
	assert.Equal(t, byte(0xBB), ttcPayload[2])
}

func TestExtractTTCPayload_TooShort(t *testing.T) {
	t.Parallel()
	assert.Nil(t, extractTTCPayload([]byte{0x00}))
	assert.Nil(t, extractTTCPayload(nil))
}
