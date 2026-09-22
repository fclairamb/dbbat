package oracle

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// The define block's reading, which is the one fact about a thin LOB fetch that
// the server's own frames do not carry. See ttc_define.go.

// TestDefineBlockSaysWhichLOBReadingTheClientAsked is the measurement the whole
// branch rests on, stated on all three thin recordings of one query.
//
// They are the same statement against the same server and their column records
// are byte-identical — it is the *client* frames that differ, and only on this:
//
//   - go-ora with nothing configured sends a define re-declaring each LOB column
//     as a LONG, which is the ask for the bodies;
//   - python-oracledb thin with nothing configured sends a define keeping each
//     LOB column the LOB type it already was, which is the ask for locators;
//   - go-ora with `lob fetch=post` sends no define at all, which is the ask for
//     nothing — and a locator is what the server sends then.
func TestDefineBlockSaysWhichLOBReadingTheClientAsked(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		fixture string
		learned bool
		shape   lobRowShape
	}{
		{goOraLOBFixture, true, lobRowInline},
		{pythonThinLOBFixture, true, lobRowLocator},
		{goOraLOBStreamFixture, false, lobRowLocator},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			shape, ok := lobShapeFromDump(t, loadTestDump(t, tc.fixture), describeColumnTypes(goOraLOBColumns))

			require.Equal(t, tc.learned, ok, "whether the client stated a reading at all")
			assert.Equal(t, tc.shape, shape)
		})
	}
}

// TestDefineBlockIsNotFoundInFramesThatAreNotOne is the gate every reading in
// this package carries, and it matters more here than on most: the walk *finds*
// its start offset rather than computing it, so a frame that is not a define
// is a frame with one chance per byte to look like one.
//
// The whole pcapng corpus is swept for a define naming twelve columns of
// goOraLOBQuery's shape. Exactly three frames may answer — the three recordings
// that really do carry one — and every other client frame of every other
// recording must come back empty-handed.
//
// jdbc_thin_lob.pcapng is the third, and it is the census this test exists for:
// it was refused until the CHAR → VARCHAR2 substitution was added to
// defineTypeAgrees (2026-09-23), and the relaxation is only worth keeping
// because exactly that one recording joined the answers and nothing else did.
// See TestJDBCThinDefineBlockIsReadAndLearnsTheLocator.
func TestDefineBlockIsNotFoundInFramesThatAreNotOne(t *testing.T) {
	t.Parallel()

	entries, err := filepath.Glob(filepath.Join("testdata", "*.pcapng"))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the corpus must not be empty")

	types := describeColumnTypes(goOraLOBColumns)
	found := map[string]int{}

	for _, path := range entries {
		name := filepath.Base(path)

		td := loadTestDump(t, name)
		for _, pkt := range td.Packets {
			if pkt.Direction != dump.DirClientToServer {
				continue
			}

			tns, err := parseTNSFromDumpPacket(pkt.Data)
			if err != nil || tns.Type != TNSPacketTypeData || len(tns.Payload) < ttcDataFlagsSize+1 {
				continue
			}

			if _, ok := execDefineLOBShape(extractTTCPayload(tns.Payload), types); ok {
				found[name]++
			}
		}
	}

	assert.Equal(t, map[string]int{
		goOraLOBFixture:      1,
		pythonThinLOBFixture: 1,
		jdbcThinLOBFixture:   1,
	}, found, "only the three recordings that carry a define may be read as carrying one")
}

// TestDefineBlockIsRefusedWhenMoreThanOneOffsetWalks pins the tie-break, and it
// is a refusal rather than a resolution.
//
// The entries are found by trying every offset the exec header leaves open and
// keeping the one whose entries are as many as the describe named and end on
// the frame's last byte. That is heavily over-determined on a real frame, but
// it is not unique by construction: the bytes below are one column's entry
// twice over, a six-byte MaxLen from the first offset and an all-zero entry
// from the second, both ending on the same last byte.
//
// A second reading is exactly what this package does not do with a row, so it
// does not do it with the frame that decides how to read one either.
func TestDefineBlockIsRefusedWhenMoreThanOneOffsetWalks(t *testing.T) {
	t.Parallel()

	// 60 00 00 00 | 05 60 00 00 00 00 | 00 x8   — one entry, read from 0
	//               ^^ also a DataType of 96, opening a second, all-zero entry
	ambiguous := []byte{
		0x60, 0x00, 0x00, 0x00,
		0x05, 0x60, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	first, next, ok := readDefineEntry(ambiguous, 0)
	require.True(t, ok)
	assert.Equal(t, tnsTypeCHAR, first)
	assert.Len(t, ambiguous, next, "the entry read from 0 ends on the last byte")

	second, next, ok := readDefineEntry(ambiguous, 5)
	require.True(t, ok)
	assert.Equal(t, tnsTypeCHAR, second)
	assert.Len(t, ambiguous, next, "and so does the one read from 5")

	_, ok = execDefineColumnTypes(ambiguous, 0, []int{tnsTypeCHAR})
	assert.False(t, ok, "two offsets that both walk must be refused, not resolved")
}

// TestDefineBlockNeedsEveryColumnToAgreeWithTheDescribe is the other half of
// what makes the walk a measurement: a client mostly echoes the describe back
// for every column it is not changing, so anything else is usually a walk that
// landed on the wrong bytes.
//
// The exceptions are a table of two measured pairs, and **both are one-way**.
// A CHAR column re-declared as a LONG is not a LOB being inlined, it is a
// misread; and a VARCHAR2 column re-declared as a CHAR is not ojdbc's scalar
// spelling, because no recording shows a client writing that direction. The
// one-way half is the whole reason the relaxation is safe, so it is asserted
// here rather than left implied.
func TestDefineBlockNeedsEveryColumnToAgreeWithTheDescribe(t *testing.T) {
	t.Parallel()

	assert.True(t, defineTypeAgrees(tnsTypeCHAR, tnsTypeCHAR))
	assert.True(t, defineTypeAgrees(tnsTypeCLOB, tnsTypeCLOB), "a LOB kept as itself is the ask for locators")
	assert.True(t, defineTypeAgrees(tnsTypeCLOB, tnsTypeLongVarChar), "go-ora's CLOB substitution")
	assert.True(t, defineTypeAgrees(tnsTypeBLOB, tnsTypeLONGRAW), "and its BLOB one")
	assert.True(t, defineTypeAgrees(tnsTypeCHAR, tnsTypeVARCHAR), "ojdbc spells an unchanged CHAR column VARCHAR2")
	assert.False(t, defineTypeAgrees(tnsTypeVARCHAR, tnsTypeCHAR), "but that pair is one-way: no client was seen writing the reverse")
	assert.False(t, defineTypeAgrees(tnsTypeCHAR, tnsTypeLongVarChar), "a scalar is not a LOB being inlined")
	assert.False(t, defineTypeAgrees(tnsTypeLongVarChar, tnsTypeCLOB), "and the LOB pair is one-way too")
	assert.False(t, defineTypeAgrees(tnsTypeCLOB, tnsTypeBLOB), "nor is one LOB type another")
	assert.False(t, defineTypeAgrees(tnsTypeVARCHAR, tnsTypeNUMBER), "and no other scalar pair agrees")
}

// TestMixedLOBDefineLearnsNothing pins the case no recording shows: a client
// that inlined some of its LOB columns and not others.
//
// Reading it would need a per-column shape, and inventing one off a frame
// nothing has sent is how a walk acquires a branch that has never been
// measured. Left unlearned, the fetch reads locators and a row that is not one
// costs itself rather than filling with framing bytes.
func TestMixedLOBDefineLearnsNothing(t *testing.T) {
	t.Parallel()

	describe := []int{tnsTypeCHAR, tnsTypeCLOB, tnsTypeBLOB}

	shape, ok := lobShapeFromDefinedTypes(describe, []int{tnsTypeCHAR, tnsTypeLongVarChar, tnsTypeBLOB})
	assert.False(t, ok, "one inlined and one kept is not a reading")
	assert.Equal(t, lobRowLocator, shape, "and what it falls back to is the locator")

	shape, ok = lobShapeFromDefinedTypes(describe, []int{tnsTypeCHAR, tnsTypeLongVarChar, tnsTypeLONGRAW})
	assert.True(t, ok)
	assert.Equal(t, lobRowInline, shape)

	shape, ok = lobShapeFromDefinedTypes(describe, []int{tnsTypeCHAR, tnsTypeCLOB, tnsTypeBLOB})
	assert.True(t, ok)
	assert.Equal(t, lobRowLocator, shape)

	_, ok = lobShapeFromDefinedTypes([]int{tnsTypeCHAR}, []int{tnsTypeCHAR})
	assert.False(t, ok, "a cursor with no LOB column has no opinion to read")
}

// lobShapeFromDump is what session.learnLOBRowShape does, over a recording's
// client frames: the last reading any of them stated, and whether any did.
func lobShapeFromDump(t *testing.T, td *testDump, describeTypes []int) (lobRowShape, bool) {
	t.Helper()

	var (
		shape  lobRowShape
		stated bool
	)

	for _, pkt := range td.Packets {
		if pkt.Direction != dump.DirClientToServer {
			continue
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData || len(tns.Payload) < ttcDataFlagsSize+1 {
			continue
		}

		if got, ok := execDefineLOBShape(extractTTCPayload(tns.Payload), describeTypes); ok {
			shape, stated = got, true
		}
	}

	return shape, stated
}
