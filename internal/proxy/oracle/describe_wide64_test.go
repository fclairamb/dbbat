package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The column list of ociDescribeQuery as the 64-bit dialect's fixture records
// it, name and TTC type code, in wire order. It is the bar TestOCIDescribeRecordsParse
// sets for the 4-byte dialect, applied to this one — and it is a bar rather
// than a smoke test because thirteen records only all come out right if every
// field boundary in between is right.
//
// Which thirteen is the measurement. The first eight are the original query,
// unchanged. The five after them were added to separate three things the
// original conflated, and each pulls its weight here:
//
//   - `O`, `OBJLONG` and `X` carry a type OID like `OBJ` does, at type-name
//     lengths 7, 33 and 7 against its 13, at name lengths 1, 7 and 1 against
//     its 3, and — `X` alone — at schema length 3 against `SYSTEM`'s 6. Four
//     records of one shape at three independent lengths is what turns "a layout
//     consistent with one record" into a measurement.
//   - `CL` is a CLOB: a column whose record carries **no** OID, schema or type
//     name although its type is not a scalar one, which is what says the
//     eleven-byte pad tracks the absent OID and not the column's type code.
//   - `TAIL` is an ordinary NUMBER and it is the query's **last** column. In
//     the first recording the object column was last, so the one byte that
//     differed on it could equally have meant "no more records follow". It does
//     not: `TAIL` carries that byte set and `X`, two columns earlier, carries
//     it clear.
var oci64DescribeColumns = []columnDesc{
	{Name: "N2", Type: tnsTypeNUMBER},
	{Name: "BIG", Type: tnsTypeVARCHAR},
	{Name: "FLT", Type: tnsTypeNUMBER},
	{Name: "D", Type: tnsTypeDATE},
	{Name: "TS", Type: tnsTypeTSTZDTY},
	{Name: "C5", Type: tnsTypeCHAR},
	{Name: "R", Type: tnsTypeRAW},
	{Name: "OBJ", Type: ociObjectColumnType},
	{Name: "O", Type: ociObjectColumnType},
	{Name: "OBJLONG", Type: ociObjectColumnType},
	{Name: "CL", Type: ociCLOBColumnType},
	{Name: "X", Type: tnsTypeOPAQUE},
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

	cols := parseColumnDescribes(extractTTCPayload(frames[len(frames)-1]), oci64OERShape())
	assert.Equal(t, oci64DescribeColumns, cols)
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
