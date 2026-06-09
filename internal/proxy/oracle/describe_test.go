package oracle

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// firstQueryResultFor returns the QueryResult (func 0x10) describe payload of
// the first query in td whose SQL contains marker.
func firstQueryResultFor(t *testing.T, td *testDump, marker string) []byte {
	t.Helper()

	started := false

	for _, pkt := range td.Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData || len(tns.Payload) < ttcDataFlagsSize+1 {
			continue
		}

		fc, _ := parseTTCFunctionCode(tns.Payload)
		ttc := extractTTCPayload(tns.Payload)

		if pkt.Direction == dump.DirClientToServer {
			if strings.Contains(findSQLInPayload(ttc), marker) {
				started = true
			}

			continue
		}

		if started && fc == TTCFuncQueryResult {
			return ttc
		}
	}

	t.Fatalf("no QueryResult found for marker %q", marker)

	return nil
}

// TestParseColumnDescribes checks that the describe-record parser recovers the
// exact column names and TTC type codes for every live-Oracle fixture — the
// names the heuristic scanner can't (single-char "N", unnamed "LEVEL*10") and
// the per-column type that unblocks type-aware value decoding.
func TestParseColumnDescribes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		file   string
		marker string
		want   []columnDesc
	}{
		{"go_ora_compressed.dbbat-dump", compressedSQLMarker, []columnDesc{
			{"GRP", tnsTypeCHAR}, {"NUM", tnsTypeNUMBER}, {"OPT", tnsTypeVARCHAR},
		}},
		{"go_ora_numbers.dbbat-dump", numbersSQLMarker, []columnDesc{
			{"PI", tnsTypeNUMBER},
			{"AMOUNT", tnsTypeNUMBER},
			{"MILLION", tnsTypeNUMBER},
			{"QUARTER", tnsTypeNUMBER},
		}},
		{"go_ora_temporal.dbbat-dump", temporalSQLMarker, []columnDesc{
			{"DT", tnsTypeDATE}, {"TS", tnsTypeTSDTY}, {"TSTZ", tnsTypeTSTZDTY},
		}},
		{"go_ora_largeresult.dbbat-dump", largeResultSQLMarker, []columnDesc{
			{"ID", tnsTypeNUMBER}, {"PAYLOAD", tnsTypeVARCHAR},
		}},
		{"go_ora_colcount.dbbat-dump", colCountSQLMarker, []columnDesc{
			{"N", tnsTypeNUMBER}, {"LEVEL*10", tnsTypeNUMBER},
		}},
		{"go_ora_mixed.dbbat-dump", mixedSQLMarker, []columnDesc{
			{"ID", tnsTypeNUMBER},
			{"GRP", tnsTypeNUMBER},
			{"AMOUNT", tnsTypeNUMBER},
			{"MAYBE_DAY", tnsTypeDATE},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()

			td := loadTestDump(t, tc.file)
			ttc := firstQueryResultFor(t, td, tc.marker)

			got := parseColumnDescribes(ttc)
			require.NotNil(t, got, "parser should succeed on a real describe")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDecodeQueryResultV2_RealColumnNames checks that decodeQueryResultV2 now
// surfaces the describe-record column names through the live decode path —
// in particular the single-char "N" and unnamed "LEVEL*10" that used to be
// captured as synthetic COL1/COL2.
func TestDecodeQueryResultV2_RealColumnNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		file   string
		marker string
		want   []string
	}{
		{"go_ora_colcount.dbbat-dump", colCountSQLMarker, []string{"N", "LEVEL*10"}},
		{"go_ora_compressed.dbbat-dump", compressedSQLMarker, []string{"GRP", "NUM", "OPT"}},
		{"go_ora_temporal.dbbat-dump", temporalSQLMarker, []string{"DT", "TS", "TSTZ"}},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()

			td := loadTestDump(t, tc.file)
			ttc := firstQueryResultFor(t, td, tc.marker)

			result := decodeQueryResultV2(ttc)
			require.NotNil(t, result)
			assert.Equal(t, tc.want, result.Columns)
		})
	}
}

// TestParseColumnDescribes_Fallback verifies the parser returns nil (so callers
// fall back to name scanning) for inputs that are not a clean describe.
func TestParseColumnDescribes_Fallback(t *testing.T) {
	t.Parallel()

	assert.Nil(t, parseColumnDescribes(nil))
	assert.Nil(t, parseColumnDescribes([]byte{0x06, 0x01, 0x02}), "not a describe (func != 0x10)")
	assert.Nil(t, parseColumnDescribes([]byte{0x10}), "truncated header")
	// A describe header claiming columns but with no record bytes must bail.
	assert.Nil(t, parseColumnDescribes([]byte{0x10, 0x00, 0x01, 0x02, 0x01, 0x03, 0x00}))
}
