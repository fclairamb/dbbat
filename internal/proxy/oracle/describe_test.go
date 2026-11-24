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
			if sql, _ := findSQLInPayload(ttc); strings.Contains(sql, marker) {
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
		{"go_ora_compressed.pcapng", compressedSQLMarker, []columnDesc{
			{"GRP", tnsTypeCHAR}, {"NUM", tnsTypeNUMBER}, {"OPT", tnsTypeVARCHAR},
		}},
		{"go_ora_numbers.pcapng", numbersSQLMarker, []columnDesc{
			{"PI", tnsTypeNUMBER},
			{"AMOUNT", tnsTypeNUMBER},
			{"MILLION", tnsTypeNUMBER},
			{"QUARTER", tnsTypeNUMBER},
		}},
		{"go_ora_temporal.pcapng", temporalSQLMarker, []columnDesc{
			{"DT", tnsTypeDATE}, {"TS", tnsTypeTSDTY}, {"TSTZ", tnsTypeTSTZDTY},
		}},
		{"go_ora_largeresult.pcapng", largeResultSQLMarker, []columnDesc{
			{"ID", tnsTypeNUMBER}, {"PAYLOAD", tnsTypeVARCHAR},
		}},
		{"go_ora_colcount.pcapng", colCountSQLMarker, []columnDesc{
			{"N", tnsTypeNUMBER}, {"LEVEL*10", tnsTypeNUMBER},
		}},
		{"go_ora_mixed.pcapng", mixedSQLMarker, []columnDesc{
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

			got := parseColumnDescribes(ttc, oerShape{})
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
		{"go_ora_colcount.pcapng", colCountSQLMarker, []string{"N", "LEVEL*10"}},
		{"go_ora_compressed.pcapng", compressedSQLMarker, []string{"GRP", "NUM", "OPT"}},
		{"go_ora_temporal.pcapng", temporalSQLMarker, []string{"DT", "TS", "TSTZ"}},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()

			td := loadTestDump(t, tc.file)
			ttc := firstQueryResultFor(t, td, tc.marker)

			result := decodeQueryResultV2(ttc, oerShape{}, lobRowLocator)
			require.NotNil(t, result)
			assert.Equal(t, tc.want, result.Columns)
		})
	}
}

// TestParseColumnDescribes_Fallback verifies the parser returns nil (so callers
// fall back to name scanning) for inputs that are not a clean describe.
func TestParseColumnDescribes_Fallback(t *testing.T) {
	t.Parallel()

	assert.Nil(t, parseColumnDescribes(nil, oerShape{}))
	assert.Nil(t, parseColumnDescribes([]byte{0x06, 0x01, 0x02}, oerShape{}), "not a describe (func != 0x10)")
	assert.Nil(t, parseColumnDescribes([]byte{0x10}, oerShape{}), "truncated header")
	// A describe header claiming columns but with no record bytes must bail.
	assert.Nil(t, parseColumnDescribes([]byte{0x10, 0x00, 0x01, 0x02, 0x01, 0x03, 0x00}, oerShape{}))
}

// TestParseColumnDescribes_PythonThinNumberScale pins the describes of
// python-oracledb thin against 23ai, whose NUMBER columns spell the scale as one
// signed byte (0x81, the -127 float sentinel) instead of the compressed int
// go-ora and JDBC send. Read the old way the scale swallowed the first byte of
// the max-length, every record after it drifted and the whole describe was
// discarded, leaving ColumnTypes empty and row values typed by guesswork.
func TestParseColumnDescribes_PythonThinNumberScale(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "python_thin_array_dml.pcapng")

	cases := []struct {
		marker string
		want   []columnDesc
	}{
		{"SELECT id, name FROM dbbat_array_dml WHERE id BETWEEN", []columnDesc{
			{"ID", tnsTypeNUMBER}, {"NAME", tnsTypeVARCHAR},
		}},
		{"SELECT id, note, name FROM dbbat_array_dml", []columnDesc{
			{"ID", tnsTypeNUMBER}, {"NOTE", tnsTypeVARCHAR}, {"NAME", tnsTypeVARCHAR},
		}},
	}

	for _, tc := range cases {
		ttc := firstQueryResultFor(t, td, tc.marker)

		assert.Equal(t, tc.want, parseColumnDescribes(ttc, oerShape{}), tc.marker)

		result := decodeQueryResultV2(ttc, oerShape{}, lobRowLocator)
		require.NotNil(t, result, tc.marker)

		types := make([]int, len(tc.want))
		for i, d := range tc.want {
			types[i] = d.Type
		}

		assert.Equal(t, types, result.ColumnTypes, tc.marker)
	}
}
