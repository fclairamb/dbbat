package oracle

import (
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
const ociCLOBColumnType = 112

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

// lastRecordedFrame is the final frame of a hex fixture — for the describe
// fixtures, the deliberately type-rich query rather than the login probe.
func lastRecordedFrame(t *testing.T, path string) []byte {
	t.Helper()

	frames := recordedFrames(t, path)
	require.NotEmpty(t, frames, "fixture %s must carry at least one frame", path)

	return frames[len(frames)-1]
}
