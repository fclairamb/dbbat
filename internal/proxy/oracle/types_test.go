package oracle

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeOracleNumber(t *testing.T) {
	tests := []struct {
		name     string
		raw      []byte
		expected string
	}{
		{"zero", []byte{0x80}, "0"},
		{"one", []byte{0xC1, 0x02}, "1"},
		{"negative_one", []byte{0x3E, 0x64, 0x66}, "-1"},
		{"large", []byte{0xC3, 0x0D, 0x23, 0x39}, "123456"},
		{"decimal", []byte{0xC1, 0x04, 0x0F}, "3.14"},
		{"ten", []byte{0xC1, 0x0B}, "10"},
		{"hundred", []byte{0xC2, 0x02}, "100"},
		{"negative_hundred", []byte{0x3D, 0x64, 0x66}, "-100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := decodeOracleNumber(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDecodeOracleNumber_EmptyBytes(t *testing.T) {
	_, err := decodeOracleNumber([]byte{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidNumberData)
}

func TestDecodeOracleDate(t *testing.T) {
	tests := []struct {
		name     string
		raw      []byte
		expected time.Time
	}{
		{"2024-03-15 14:30:00", []byte{120, 124, 3, 15, 15, 31, 1}, time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC)},
		{"2000-01-01 00:00:00", []byte{120, 100, 1, 1, 1, 1, 1}, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"1999-12-31 23:59:59", []byte{119, 199, 12, 31, 24, 60, 60}, time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := decodeOracleDate(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDecodeOracleDate_WrongLength(t *testing.T) {
	_, err := decodeOracleDate([]byte{1, 2, 3})
	assert.ErrorIs(t, err, ErrInvalidDateLength)
}

func TestDecodeOracleTimestamp(t *testing.T) {
	raw := append(
		[]byte{120, 124, 6, 15, 11, 31, 1},
		[]byte{0x05, 0xF5, 0xE1, 0x00}..., // 100_000_000 nanoseconds = 0.1s
	)
	result, err := decodeOracleTimestamp(raw)
	require.NoError(t, err)
	assert.Equal(t, 2024, result.Year())
	assert.Equal(t, time.June, result.Month())
	assert.Equal(t, 100000000, result.Nanosecond())
}

func TestDecodeOracleTimestamp_TooShort(t *testing.T) {
	_, err := decodeOracleTimestamp([]byte{120, 124, 6, 15, 11, 31, 1, 0x00, 0x00})
	assert.ErrorIs(t, err, ErrInvalidTimestampLength)
}

func TestDecodeOracleVARCHAR2(t *testing.T) {
	result, err := decodeOracleValue(OracleTypeVARCHAR2, []byte("hello world"))
	require.NoError(t, err)
	assert.Equal(t, "hello world", result)
}

func TestDecodeOracleVARCHAR2_UTF8(t *testing.T) {
	result, err := decodeOracleValue(OracleTypeVARCHAR2, []byte("café résumé"))
	require.NoError(t, err)
	assert.Equal(t, "café résumé", result)
}

func TestDecodeOracleCHAR_TrimsPadding(t *testing.T) {
	result, err := decodeOracleValue(OracleTypeCHAR, []byte("hello     "))
	require.NoError(t, err)
	assert.Equal(t, "hello", result)
}

func TestDecodeOracleRAW(t *testing.T) {
	result, err := decodeOracleValue(OracleTypeRAW, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	require.NoError(t, err)
	assert.Equal(t, "deadbeef", result)
}

func TestDecodeOracleBINARY_FLOAT(t *testing.T) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, math.Float32bits(3.14))
	result, err := decodeOracleValue(OracleTypeBINFLOAT, buf)
	require.NoError(t, err)
	assert.InDelta(t, 3.14, result, 0.01)
}

func TestDecodeOracleBINARY_DOUBLE(t *testing.T) {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, math.Float64bits(2.718281828))
	result, err := decodeOracleValue(OracleTypeBINDOUBLE, buf)
	require.NoError(t, err)
	assert.InDelta(t, 2.718281828, result, 0.0001)
}

func TestDecodeOracleLOB_ReturnsPlaceholder(t *testing.T) {
	result, err := decodeOracleValue(OracleTypeCLOB, []byte{0x01, 0x02, 0x03})
	require.NoError(t, err)
	assert.Equal(t, "[LOB]", result)

	result, err = decodeOracleValue(OracleTypeBLOB, []byte{0x01, 0x02, 0x03})
	require.NoError(t, err)
	assert.Equal(t, "[LOB]", result)
}

func TestDecodeOracleValue_UnknownType_Base64(t *testing.T) {
	result, err := decodeOracleValue(255, []byte{0x01, 0x02})
	require.NoError(t, err)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}), result)
}

func TestDecodeOracleValue_NilData(t *testing.T) {
	result, err := decodeOracleValue(OracleTypeVARCHAR2, nil)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestDecodeOracleValue_EmptyData(t *testing.T) {
	result, err := decodeOracleValue(OracleTypeVARCHAR2, []byte{})
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestDecodeOracleDate_ViaDispatch(t *testing.T) {
	raw := []byte{120, 124, 3, 15, 15, 31, 1}
	result, err := decodeOracleValue(OracleTypeDATE, raw)
	require.NoError(t, err)
	assert.Equal(t, "2024-03-15T14:30:00", result)
}

func TestDecodeOracleTimestamp_ViaDispatch(t *testing.T) {
	raw := append(
		[]byte{120, 124, 6, 15, 11, 31, 1},
		[]byte{0x05, 0xF5, 0xE1, 0x00}...,
	)
	result, err := decodeOracleValue(OracleTypeTIMESTAMP, raw)
	require.NoError(t, err)
	val, ok := result.(string)
	require.True(t, ok)
	assert.Contains(t, val, "2024-06-15T10:30:00")
}
