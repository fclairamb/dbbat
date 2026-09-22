package oracle

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two describes of a recorded session, name and TTC type code, in wire
// order — and one pair of lists for **both** OCI dialects on purpose. The two
// fixtures are two marshalings of the same two queries against the same server,
// so the reading that says they agree is the reading that says both walks are
// right; two pairs of lists would let one drift into agreeing with itself.
//
// ociDescribeColumns is ociDescribeQuery, unchanged since the 4-byte dialect's
// walk was written against it.
var ociDescribeColumns = []columnDesc{
	{Name: "N2", Type: tnsTypeNUMBER},
	{Name: "BIG", Type: tnsTypeVARCHAR},
	{Name: "FLT", Type: tnsTypeNUMBER},
	{Name: "D", Type: tnsTypeDATE},
	{Name: "TS", Type: tnsTypeTSTZDTY},
	{Name: "C5", Type: tnsTypeCHAR},
	{Name: "R", Type: tnsTypeRAW},
	{Name: "OBJ", Type: ociObjectColumnType},
}

// ociDescribeObjectValue is what the `OBJ` column captures on **both** fixtures:
// the object's own image, rendered, rather than the locator in front of it.
//
// The column is `dbbat_cap_obj(1, 'x')` and its image is `84 01 08 02 c1 02 01
// 78` — the flag, the length 8, then the NUMBER 1 and the string `x` as two
// ordinary CLRs. The attributes carry no types, so each is read by
// decodeOracleRawValue, the same type-less reading the package applies to any
// column whose type code it does not have.
//
// Sharing one constant across the two dialects is the argument for the change
// rather than a convenience. The locators these two sessions captured for this
// one value did **not** match — `...5c0f1a6dae5600fc...` on the 64-bit
// recording against `...5c0f18b7351e0110...` on the 4-byte one, forty seconds
// apart against the same row — because a locator is a per-fetch handle. The
// image matches to the byte.
const ociDescribeObjectValue = "(1, x)"

// ociDescribeTypedColumns is ociDescribeTypedQuery, the describe added to place
// the 64-bit record's extra 25 bytes. Every column in it separates something the
// list above conflates, and each pulls its weight here:
//
//   - `OBJ`, `O` and `OBJLONG` are one shape at type-name lengths 13, 7 and 33
//     and name lengths 3, 1 and 7. Three samples at two independent lengths is
//     what turns "a layout consistent with one record" into a measurement.
//   - `X` is that shape again at a schema length of 3 against `SYSTEM`'s 6 —
//     and its type code, 58, is one isKnownTNSType did not cover at all until
//     this fixture produced it (tnsTypeOPAQUE).
//   - `CL` is a CLOB: no OID, no schema, no type name, and not a scalar type
//     either, which is what says the eleven-byte pad tracks the absent OID and
//     not the column's type code.
//   - `TAIL` is an ordinary NUMBER and it is **last**. In the first recording
//     the object column was last, so the one byte that differed on it could
//     equally have meant "no more records follow". It does not: `TAIL` carries
//     that byte set and `X` carries it clear.
var ociDescribeTypedColumns = []columnDesc{
	{Name: "OBJ", Type: ociObjectColumnType},
	{Name: "O", Type: ociObjectColumnType},
	{Name: "OBJLONG", Type: ociObjectColumnType},
	{Name: "X", Type: tnsTypeOPAQUE},
	{Name: "CL", Type: ociCLOBColumnType},
	{Name: "TAIL", Type: tnsTypeNUMBER},
}

// ociCLOBColumnType is the TTC type code for a CLOB locator, which
// isKnownTNSType's 60..127 range already covered. The XMLTYPE column's — 58 —
// did not, and adding it is a finding this fixture produced rather than a
// convenience: see tnsTypeOPAQUE.
const ociCLOBColumnType = tnsTypeCLOB

// TestOCI64DescribeRecordsParse is the 64-bit dialect's half of
// TestOCIDescribeRecordsParse, and it is the test the whole fixture exists for.
//
// Before this, parseColumnDescribes was handed the **4-byte** layout on a
// 64-bit session — describeWireLayout keyed on `fixedWidth` alone — so it
// misaligned on the first record and returned nil, and every one of that
// session's row captures fell back to scanAndPadColumnNames. The fallback is
// the gap 2026-09-20-02-oracle-oci-row-capture-still-scans-for-column-names.md
// describes: on this very query it invents `SYSTEM` and `DBBAT_CAP_OBJ` as
// column names, because they live in the record as the other two DLCs behind
// the name.
func TestOCI64DescribeRecordsParse(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64Describes)
	require.NotEmpty(t, frames, "the fixture must carry the session's describes")

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)
		require.Equalf(t, byte(TTCFuncQueryResult), ttc[0], "frame %d must be a describe", i)

		assert.NotEmptyf(t, parseColumnDescribes(ttc, oci64OERShape()),
			"every describe a 64-bit OCI session receives must parse under its own reading: frame %d", i)
	}

	require.Len(t, frames, 3, "the fixture must carry the login probe and both describes")

	assert.Equal(t, ociDescribeColumns,
		parseColumnDescribes(extractTTCPayload(frames[1]), oci64OERShape()))
	assert.Equal(t, ociDescribeTypedColumns,
		parseColumnDescribes(extractTTCPayload(frames[2]), oci64OERShape()))
}

// TestOCI64DescribeIsNotOfferedToTheOtherTwoEncodings is the gate every reading
// in this package carries, on the describe path: the encoding comes from the
// session, so a real describe must decode under the one shape it was recorded
// from and under neither of the others. A payload offered three layouts is a
// payload with three chances to produce a plausible-looking column list.
//
// Both describes checked here are the session's **rich** one, and that is worth
// stating rather than hiding: on the single-column login probe at the head of
// each fixture, the 4-byte reading of the 64-bit payload does not fail — it
// yields one column with an empty name. One record is a short run of small
// integers, and a short run of small integers is not evidence of anything. The
// guarantee that keeps that harmless is upstream of this test and is not a
// property of the bytes: the encoding is asked of the session's learned
// oerShape and never sniffed from the payload, so no describe is ever offered
// two readings to be mistaken between.
func TestOCI64DescribeIsNotOfferedToTheOtherTwoEncodings(t *testing.T) {
	t.Parallel()

	oci64 := extractTTCPayload(lastRecordedFrame(t, oci64Describes))

	assert.NotEmpty(t, parseColumnDescribes(oci64, oci64OERShape()),
		"the fixture must parse under the shape it was recorded from")
	assert.Nil(t, parseColumnDescribes(oci64, ociOERShape()),
		"a 64-bit OCI describe must not be read as a 4-byte OCI one")
	assert.Nil(t, parseColumnDescribes(oci64, oerShape{}),
		"a 64-bit OCI describe must not be read as a compressed one")

	oci := extractTTCPayload(lastRecordedFrame(t, ociDescribes))

	assert.Nil(t, parseColumnDescribes(oci, oci64OERShape()),
		"and the 4-byte dialect's describe must not be read as a 64-bit one")
}

// TestOCI64RowCaptureCarriesTheDescribesValues is the 64-bit dialect's half of
// TestOCIRowCaptureCarriesTheDescribesColumnNames, and it asserts the half that
// test asserts and this dialect did not have: the **values**.
//
// The names came right when parseColumnDescribes learned this dialect's records.
// The values did not, and the gap was visible rather than theoretical: a 64-bit
// OCI session wrote its rows to query_rows as empty JSON objects, because
// the reading it had looked for a two-byte `06 22` ROW_HEADER marker that is
// `06 01 22 xx` here and therefore occurs nowhere in the payload at all.
//
// The expected values are the 4-byte fixture's own, column for column, and that
// is the point: the two fixtures are the same two queries against the same
// server, so anything but agreement on `N2`, `BIG`, `FLT`, `C5`, `R` and `OBJ`
// is a reading that found bytes rather than values. The two temporal columns are
// the recordings' own clocks, ~40 seconds apart, so they are the one pair that
// cannot be shared — they are spelled out at this recording's timestamps.
func TestOCI64RowCaptureCarriesTheDescribesValues(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64Describes)
	require.GreaterOrEqual(t, len(frames), 2, "the fixture must carry both describes")

	tests := []struct {
		name  string
		frame int
		want  map[string]interface{}
	}{
		{
			name:  "an expression column the scanner cannot see at all",
			frame: 0,
			want:  map[string]interface{}{ociExpressionColumnName: "SYSTEM"},
		},
		{
			name:  "eight columns, two of them one character long",
			frame: 1,
			want: map[string]interface{}{
				"N2":  "1",
				"BIG": "x",
				"FLT": "0.3333333333333333333333333333333333333333",
				"D":   "2026-09-22 08:25:03",
				"TS":  "2026-09-22 08:25:03.830104 +00:00",
				"C5":  "ab   ",
				"R":   "7a7a",
				"OBJ": ociDescribeObjectValue,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ttc := extractTTCPayload(frames[tc.frame])

			s, rowStore, _ := newCapturingSession(t, 10000)
			s.oer = oci64OERShape()
			require.True(t, s.oerShapeSnapshot().fixedWidth64,
				"the session must speak the encoding the frame was recorded on")

			s.trackerMu.Lock()
			s.handleQueryResultV2(ttc)
			s.trackerMu.Unlock()

			require.NotNil(t, s.tracker.pendingQuery, "the fetch must still be open")

			s.tracker.pendingQuery.rowSink.Flush(t.Context())

			rows := rowStore.rowData(t)
			require.Len(t, rows, 1, "the frame carries exactly one row")
			assert.Equal(t, tc.want, rows[0])
		})
	}
}

// TestOCI64RowValuesAreNotOfferedToTheOtherTwoEncodings is the row half of the
// gate TestOCI64DescribeIsNotOfferedToTheOtherTwoEncodings holds on the describe
// path, and it is what keeps the 64-bit ROW_HEADER reading from being a third
// chance at a plausible-looking row: the encoding comes from the session, and
// each fixture must yield its row under the one shape it was recorded from and
// nothing under the other.
//
// The reading fails closed by construction — it validates the header's own
// column count against the describe's and requires the ROW_DATA byte exactly
// where the header ends — so the failure here is "no rows", never different
// ones.
func TestOCI64RowValuesAreNotOfferedToTheOtherTwoEncodings(t *testing.T) {
	t.Parallel()

	oci64 := extractTTCPayload(recordedFrames(t, oci64Describes)[1])
	oci := extractTTCPayload(recordedFrames(t, ociDescribes)[1])

	require.Len(t, decodeQueryResultV2(oci64, oci64OERShape(), lobRowLocator).Rows, 1,
		"the fixture must yield its row under the shape it was recorded from")
	assert.Empty(t, decodeQueryResultV2(oci64, ociOERShape(), lobRowLocator).Rows,
		"a 64-bit OCI fetch must not be read as a 4-byte OCI one")
	assert.Empty(t, decodeQueryResultV2(oci64, oerShape{}, lobRowLocator).Rows,
		"a 64-bit OCI fetch must not be read as a compressed one")

	require.Len(t, decodeQueryResultV2(oci, ociOERShape(), lobRowLocator).Rows, 1,
		"and the 4-byte dialect's fetch must still yield its own row")
	assert.Empty(t, decodeQueryResultV2(oci, oci64OERShape(), lobRowLocator).Rows,
		"a 4-byte OCI fetch must not be read as a 64-bit one")
}

// TestOCI64RowHeaderPatternIsUniqueInTheCorpus is what turns "a layout
// consistent with two records" into a measurement, and it is the reason the
// walk hard-codes the header's length instead of scanning forward for the
// ROW_DATA byte the way the 4-byte reading does.
//
// It runs the pattern the walk keys on — 0x06, the 0x22 flag two bytes later,
// and a 0x07 exactly 50 bytes in — over **every frame of every .hex fixture in
// the corpus**, without the column-count check the walk also applies. Every
// match is a real 64-bit ROW_HEADER, at the column count its own describe
// declares. Nowhere else, and on no 4-byte or compressed payload.
//
// The last of them is the one the pattern was not originally written for: it is
// the head of a **packet**, not a run of bytes inside a describe, because a
// select list with a LOB in it defers the whole fetch (see oci64LOBFrames). That
// it matches there under the same reading is what says fetchRowDataStart is the
// same measurement rather than a second one.
func TestOCI64RowHeaderPatternIsUniqueInTheCorpus(t *testing.T) {
	t.Parallel()

	fixtures, err := filepath.Glob("testdata/*.hex")
	require.NoError(t, err)
	require.NotEmpty(t, fixtures)

	type hit struct {
		fixture string
		frame   int
		count   uint32
	}

	var hits []hit

	for _, fixture := range fixtures {
		for i, frame := range recordedFrames(t, fixture) {
			for p := 0; p+wide64RowHeaderLen < len(frame); p++ {
				if frame[p] != ttcMsgRowHeader ||
					!isRowHeaderFlag(frame[p+wide64RowHeaderFlagOffset]) ||
					frame[p+wide64RowHeaderLen] != ttcMsgBindOutput {
					continue
				}

				hits = append(hits, hit{
					fixture: fixture,
					frame:   i,
					count: binary.LittleEndian.Uint32(
						frame[p+wide64RowHeaderCountOffset : p+wide64RowHeaderCountOffset+4]),
				})
			}
		}
	}

	// oci64_long.hex's four are the same thing again on the fixture recorded
	// for the LONG columns: its login probe, and the packets its two
	// four-column fetches arrive in — a LONG in the select list defers a fetch
	// exactly as a LOB does. The first of those fetches takes two round trips,
	// which is why it accounts for two of them: frame 3 is the packet whose
	// flag carries 0x02 instead of 0x22, matched here because the reading is
	// masked. Widening the flag adds exactly that one hit to the corpus and
	// nothing else — no describe, no 4-byte payload, no compressed one.
	assert.Equal(t, []hit{
		{fixture: oci64Describes, frame: 0, count: 1},
		{fixture: oci64Describes, frame: 1, count: 8},
		{fixture: oci64LOBFrames, frame: 0, count: 1},
		{fixture: oci64LOBFrames, frame: 2, count: 13},
		{fixture: oci64LongFrames, frame: 0, count: 1},
		{fixture: oci64LongFrames, frame: 2, count: 4},
		{fixture: oci64LongFrames, frame: 3, count: 4},
		{fixture: oci64LongFrames, frame: 5, count: 4},
	}, hits)
}

// lastRecordedFrame is the final frame of a hex fixture — for the describe
// fixtures, the deliberately type-rich query rather than the login probe.
func lastRecordedFrame(t *testing.T, path string) []byte {
	t.Helper()

	frames := recordedFrames(t, path)
	require.NotEmpty(t, frames, "fixture %s must carry at least one frame", path)

	return frames[len(frames)-1]
}
