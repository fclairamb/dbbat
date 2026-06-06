package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These fixtures are real Oracle 19c wire captures taken through the dbbat
// proxy (MUTU01), one row each:
//
//	systimestamp                                -> TIMESTAMP WITH TIME ZONE, +00:00
//	TIMESTAMP '2026-05-24 12:34:56.789012 +05:30' -> TIMESTAMP WITH TIME ZONE, +05:30
//	localtimestamp                              -> TIMESTAMP (11 bytes)
//	CAST(systimestamp AS TIMESTAMP)             -> TIMESTAMP (11 bytes)
//
// Oracle stores the WITH TIME ZONE instant in UTC; the decoder renders the
// original local wall clock plus the offset suffix.
const (
	capTSTZUTC   = "787e0518171d26362534c0143c" // 2026-05-24 22:28:37.908408 +00:00
	capTSTZPlus  = "787e05180805392f075e20195a" // 2026-05-24 12:34:56.789012 +05:30 (UTC 07:04:56)
	capTSLocal   = "787e0519011d26390fe518"     // 2026-05-25 00:28:37.957343
	capTSCast    = "787e0518171d263a709240"     // 2026-05-24 22:28:37.980456
	capDateOnly7 = "787e05181a2e26"             // a 7-byte DATE must remain DATE
)

func TestDecodeOracleTimestampToString_RealCaptures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		hex  string
		want string
	}{
		{"tstz_utc", capTSTZUTC, "2026-05-24 22:28:37.908408 +00:00"},
		{"tstz_plus0530", capTSTZPlus, "2026-05-24 12:34:56.789012 +05:30"},
		{"ts_local_11b", capTSLocal, "2026-05-25 00:28:37.957343"},
		{"ts_cast_11b", capTSCast, "2026-05-24 22:28:37.980456"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := decodeOracleTimestampToString(mustHex(t, tc.hex))
			require.True(t, ok, "expected a successful timestamp decode")
			assert.Equal(t, tc.want, got)
		})
	}
}

// decodeOracleRawValue is the heuristic path used for row capture. Before the
// fix these timestamps fell through to the hex fallback.
func TestDecodeOracleRawValue_TimestampNotHex(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "2026-05-24 12:34:56.789012 +05:30", decodeOracleRawValue(mustHex(t, capTSTZPlus)))
	assert.Equal(t, "2026-05-25 00:28:37.957343", decodeOracleRawValue(mustHex(t, capTSLocal)))
}

// A named-region zone (high bit set in byte 11) can't be resolved to a numeric
// offset, so it decodes to the UTC wall clock with no offset suffix.
func TestDecodeOracleTimestampToString_RegionZoneFallsBackToUTC(t *testing.T) {
	t.Parallel()

	b := mustHex(t, capTSTZPlus)
	b[11] = 0x99 // high bit set => region id, not a numeric offset

	got, ok := decodeOracleTimestampToString(b)
	require.True(t, ok)
	assert.Equal(t, "2026-05-24 07:04:56.789012", got, "region tz should show stored UTC wall clock")
}

func TestDecodeOracleTimestampToString_RejectsNonTimestamp(t *testing.T) {
	t.Parallel()

	// Wrong length (7-byte DATE) must not be claimed as a timestamp.
	_, ok := decodeOracleTimestampToString(mustHex(t, capDateOnly7))
	assert.False(t, ok)

	// 11 bytes but an impossible month/day must be rejected.
	_, ok = decodeOracleTimestampToString([]byte{120, 126, 99, 99, 1, 1, 1, 0, 0, 0, 0})
	assert.False(t, ok)
}

// Type-aware path (decodeOracleValue) must now carry the offset for TIMESTAMPTZ.
func TestDecodeOracleValue_TimestampTZ(t *testing.T) {
	t.Parallel()

	got, err := decodeOracleValue(OracleTypeTIMESTAMPTZ, mustHex(t, capTSTZPlus))
	require.NoError(t, err)
	assert.Equal(t, "2026-05-24T12:34:56.789012000+05:30", got)

	got, err = decodeOracleValue(OracleTypeTIMESTAMP, mustHex(t, capTSCast))
	require.NoError(t, err)
	assert.Equal(t, "2026-05-24T22:28:37.980456000", got)
}
