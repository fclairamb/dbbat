package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ociDescribes is every describe response of a recorded sqlplus session. See
// capture_oci_describe_test.go.
const ociDescribes = "testdata/oci_describe.hex"

// TestOCIDescribeRecordsParse is what the fixed-width column record is pinned
// against, and it is pinned against *values* rather than against alignment.
//
// Until this existed, parseColumnDescribes read TTC compressed integers only, so
// an OCI session's describes never parsed and its column names came from the
// heuristic scanner — which guesses at what a describe record spells out. The
// last describe in the fixture is a deliberately awkward one: a
// `VARCHAR2(4000)` whose maximum length does not fit a byte, a `NUMBER(10,2)`
// with a real precision and scale, `1/3` whose scale is the -127 float
// sentinel, temporal types, a `CHAR(5)`, a `RAW`, and an object column whose
// record carries a non-null 16-byte type OID.
//
// That last column is the one that settles a question no run of zeros could: the
// type OID arrives as a four-byte little-endian length of 16 followed by a CLR,
// which is what says the field is a DLC in this encoding too — and therefore
// what fixes how many bytes the two integers before it may occupy.
func TestOCIDescribeRecordsParse(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, ociDescribes)

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)
		require.Equalf(t, byte(TTCFuncQueryResult), ttc[0], "frame %d must be a describe", i)

		assert.NotEmptyf(t, parseColumnDescribes(ttc, true),
			"every describe an OCI session receives must parse under the fixed-width reading: frame %d", i)
	}

	cols := parseColumnDescribes(extractTTCPayload(frames[len(frames)-1]), true)

	assert.Equal(t, []columnDesc{
		{Name: "N2", Type: tnsTypeNUMBER},
		{Name: "BIG", Type: tnsTypeVARCHAR},
		{Name: "FLT", Type: tnsTypeNUMBER},
		{Name: "D", Type: tnsTypeDATE},
		{Name: "TS", Type: tnsTypeTSTZDTY},
		{Name: "C5", Type: tnsTypeCHAR},
		{Name: "R", Type: tnsTypeRAW},
		{Name: "OBJ", Type: ociObjectColumnType},
	}, cols)
}

// ociObjectColumnType is the TTC type code 23ai reports for an object column in
// a describe. go-ora names no constant for it — its enum stops at OCIRef (110)
// and picks up again at JSON (119) — but isKnownTNSType's 60..127 range covers
// it, which is the alignment proof the walk actually relies on.
const ociObjectColumnType = 121

// TestOCIDescribeIsNotOfferedToAThinSession is the same gate the REF-cursor walk
// carries, on the describe path: the encoding comes from the session, so an OCI
// describe must yield nothing under the compressed reading and vice versa. A
// payload offered both layouts is a payload with two chances to produce a
// plausible-looking column list.
func TestOCIDescribeIsNotOfferedToAThinSession(t *testing.T) {
	t.Parallel()

	oci := extractTTCPayload(recordedFrames(t, ociDescribes)[0])

	assert.NotEmpty(t, parseColumnDescribes(oci, true),
		"the fixture must parse under the shape it was recorded from")
	assert.Nil(t, parseColumnDescribes(oci, false),
		"an OCI describe must not be read as a compressed one")
}
