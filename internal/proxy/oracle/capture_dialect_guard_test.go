//go:build capture || integration

// The regression test for the capture harness's dialect guard.
//
// The guard itself (requireRecordedDialect, capture_refcursor_test.go) only
// ever runs with a live Oracle container in front of it, so nothing caught a
// change to the two predicates it asks. This holds them to a pair of synthetic
// one-frame recordings built out of fixtures already in testdata/: no Docker,
// no client, no relay — the bytes are the evidence.
package oracle

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// syntheticDialectDump writes frame into a one-packet recording as a
// client-to-server TNS Data packet and returns its path, which is the only
// input shape the two predicates read.
func syntheticDialectDump(t *testing.T, sessionID string, dir byte, frame []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), sessionID+".pcapng")

	w := newCaptureWriter(t, path, sessionID)
	require.NoError(t, w.WritePacket(dir, encodeTNSPacket(TNSPacketTypeData, frame)))
	require.NoError(t, w.Close())

	return path
}

// TestRecordedDialectPredicatesReadTheirOwnFixtures pins both directions of the
// guard's reading against one frame of each dialect.
//
// The 64-bit frame is taken from oci64_refcursor_drives.hex, which was recorded
// through dbbat, so it satisfies the strict reading as well as the relaxed one.
// The 4-byte frame is its counterpart in oci_refcursor_drives.hex, whose byte 3
// is the 0x01 pointer flag — neither reading may call it 64-bit.
func TestRecordedDialectPredicatesReadTheirOwnFixtures(t *testing.T) {
	t.Parallel()

	wide64Dump := syntheticDialectDump(t, "synthetic-oci64-drive",
		dump.DirClientToServer, recordedFrames(t, oci64RefCursorDrivesFixture)[0])
	fourByteDump := syntheticDialectDump(t, "synthetic-oci-drive",
		dump.DirClientToServer, recordedFrames(t, ociRefCursorDrivesFixture)[0])

	// The relaxed probe, which is what refuses a 64-bit recording arriving at
	// the 4-byte harness (requireRecordedDialect's second direction).
	assert.True(t, recordedDialectLooksWide64(t, wide64Dump),
		"a 64-bit client frame must carry the 64-bit op header's shape")
	assert.False(t, recordedDialectLooksWide64(t, fourByteDump),
		"the 4-byte dialect's pointer flag at byte 3 must not read as 64-bit")

	// usesWide64OpHeader's reuse, which is what requireRecordedDialect asks in
	// its `=container` direction and what picks the fixture pair afterwards.
	assert.True(t, recordedDialectIsWide64(t, wide64Dump),
		"a frame recorded through dbbat must satisfy the strict reading too")
	assert.False(t, recordedDialectIsWide64(t, fourByteDump),
		"the 4-byte dialect never satisfies the strict reading")
}

// TestRecordedDialectPredicatesIgnoreServerFrames keeps both predicates keyed
// on what the *client* marshals. A server payload that happens to fit the shape
// says nothing about the client's dialect, and reading one would let the
// upstream decide which fixture set a recording is filed under.
func TestRecordedDialectPredicatesIgnoreServerFrames(t *testing.T) {
	t.Parallel()

	path := syntheticDialectDump(t, "synthetic-oci64-drive-inbound",
		dump.DirServerToClient, recordedFrames(t, oci64RefCursorDrivesFixture)[0])

	assert.False(t, recordedDialectLooksWide64(t, path), "server frames are not the client's dialect")
	assert.False(t, recordedDialectIsWide64(t, path), "server frames are not the client's dialect")
}
