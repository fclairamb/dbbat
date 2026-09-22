package decode

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// tnsPacket wraps a payload in the v315+ header, whose length is the 4-byte
// field at [0:4] and whose 2-byte field therefore reads as zero.
func tnsPacket(packetType byte, payload []byte) []byte {
	total := tnsHeaderLen + len(payload)
	header := make([]byte, tnsHeaderLen)

	binary.BigEndian.PutUint32(header[:4], uint32(total))
	header[4] = packetType

	return mysqlConcat(header, payload)
}

// tnsLegacyPacket wraps a payload in the pre-315 header, whose length is the
// 2-byte field at [0:2]. Connect and Accept always use it.
func tnsLegacyPacket(packetType byte, payload []byte) []byte {
	total := tnsHeaderLen + len(payload)
	header := make([]byte, tnsHeaderLen)

	binary.BigEndian.PutUint16(header[:2], uint16(total))
	header[4] = packetType

	return mysqlConcat(header, payload)
}

// tnsDataPacket builds a Data packet: two bytes of data flags, then the TTC
// message.
func tnsDataPacket(flags uint16, ttc []byte) []byte {
	return tnsPacket(tnsTypeData, mysqlConcat(binary.BigEndian.AppendUint16(nil, flags), ttc))
}

// tnsHandshakePayload builds a Connect or Accept body, whose first field is
// the TNS version.
func tnsHandshakePayload(version uint16, size int) []byte {
	payload := make([]byte, size)
	binary.BigEndian.PutUint16(payload[:2], version)

	return payload
}

// TestOracleSplitter_Trace is the golden test for the whole shape of the
// output, over a session that connects, authenticates, runs two statements and
// logs off. It pins the two framing cases the splitter exists for — several
// packets in one TCP payload, and one packet split across two — and both
// length encodings.
func TestOracleSplitter_Trace(t *testing.T) {
	t.Parallel()

	split := newOracleSplitter(Options{})

	got := feedSplitter(t, split, 0, dump.DirClientToServer,
		tnsLegacyPacket(tnsTypeConnect, tnsHandshakePayload(319, 32)))
	got = append(got, feedSplitter(t, split, 12*ms, dump.DirServerToClient,
		tnsLegacyPacket(tnsTypeAccept, tnsHandshakePayload(319, 24)))...)

	// One client flush carrying the protocol negotiation and the type table.
	got = append(got, feedSplitter(t, split, 31*ms, dump.DirClientToServer, mysqlConcat(
		tnsDataPacket(0, []byte{ttcMsgSetProtocol, 0x07, 0x06}),
		tnsDataPacket(0, mysqlConcat([]byte{ttcMsgSetDataTypes}, make([]byte, 40))),
	))...)

	got = append(got, feedSplitter(t, split, 44*ms, dump.DirServerToClient, mysqlConcat(
		tnsDataPacket(0, []byte{ttcMsgSetProtocol, 0x00}),
		tnsDataPacket(0, mysqlConcat([]byte{ttcMsgSetDataTypes}, make([]byte, 40))),
	))...)

	// The exec arrives split mid-packet: the first feed must yield nothing.
	exec := tnsDataPacket(0, mysqlConcat(
		[]byte{ttcMsgPiggyback, ttcFuncExecSQL}, make([]byte, 60),
	))

	cut := len(exec) - 20
	got = append(got, feedSplitter(t, split, 61*ms, dump.DirClientToServer, exec[:cut])...)
	got = append(got, feedSplitter(t, split, 75*ms, dump.DirClientToServer, exec[cut:])...)

	got = append(got, feedSplitter(t, split, 91*ms, dump.DirServerToClient,
		tnsDataPacket(0, mysqlConcat([]byte{ttcMsgQueryResult}, make([]byte, 120))))...)

	got = append(got, feedSplitter(t, split, 120*ms, dump.DirClientToServer,
		tnsDataPacket(0, []byte{ttcMsgPiggyback2, ttcFuncCloseCursors, 0x01}))...)

	got = append(got, feedSplitter(t, split, 140*ms, dump.DirServerToClient,
		tnsPacket(tnsTypeMarker, []byte{0x01, 0x00, 0x00}))...)

	got = append(got, feedSplitter(t, split, 160*ms, dump.DirClientToServer,
		tnsDataPacket(tnsDataFlagEOF, nil))...)

	assert.Equal(t, []string{
		`0s C> Connect version=319 (40 bytes)`,
		`12ms <S Accept version=319 (32 bytes)`,
		`31ms C> OSETPRO (3 bytes)`,
		`31ms C> ODTYPES (41 bytes)`,
		`44ms <S OSETPRO (2 bytes)`,
		`44ms <S ODTYPES (41 bytes)`,
		`75ms C> PIGGYBACK exec-sql (62 bytes)`,
		`91ms <S QRESULT (121 bytes)`,
		`120ms C> PIGGYBACK2 close-cursors (3 bytes)`,
		`140ms <S Marker(break)`,
		`160ms C> Data(flags=0x0040)`,
	}, got)
}

// TestOracleSplitter_AuthenticationNeverDumped pins the one redaction --rows
// does not lift, in both directions: the server's answer to an O5LOGON step
// carries the session key and has no function code of its own to recognize it
// by, so it is redacted by what it answers.
func TestOracleSplitter_AuthenticationNeverDumped(t *testing.T) {
	t.Parallel()

	split := newOracleSplitter(Options{ShowRows: true})

	lines := feedSplitter(t, split, 0, dump.DirClientToServer,
		tnsDataPacket(0, mysqlConcat([]byte{ttcMsgPiggyback, ttcFuncAuthPhase1}, []byte("AUTH_SESSKEY-request"))))
	lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient,
		tnsDataPacket(0, mysqlConcat([]byte{ttcMsgResponse}, []byte("AUTH_SESSKEY=server-secret"))))...)
	lines = append(lines, feedSplitter(t, split, 2*ms, dump.DirClientToServer,
		tnsDataPacket(0, mysqlConcat([]byte{ttcMsgPiggyback, ttcFuncAuthPhase2}, []byte("AUTH_PASSWORD=verifier"))))...)
	lines = append(lines, feedSplitter(t, split, 3*ms, dump.DirServerToClient,
		tnsDataPacket(0, mysqlConcat([]byte{ttcMsgResponse}, []byte("AUTH_VERSION_NO=server-proof"))))...)
	lines = append(lines, feedSplitter(t, split, 4*ms, dump.DirServerToClient,
		tnsDataPacket(0, mysqlConcat([]byte{ttcMsgResponse}, make([]byte, 20))))...)

	assert.Equal(t, []string{
		"0s C> PIGGYBACK auth-phase-1 (22 bytes, redacted)",
		"1ms <S Response (27 bytes, redacted)",
		"2ms C> PIGGYBACK auth-phase-2 (24 bytes, redacted)",
		"3ms <S Response (29 bytes, redacted)",
		"4ms <S Response (21 bytes)",
	}, lines)

	joined := strings.Join(lines, "\n")
	for _, secret := range []string{"server-secret", "verifier", "server-proof", "AUTH_SESSKEY"} {
		assert.NotContains(t, joined, secret)
	}
}

// TestOracleSplitter_ExtendedConnect covers the one packet whose declared
// length does not bound it: from TNS 315 a Connect's descriptor is appended
// after the header block with a 2-byte size of its own.
func TestOracleSplitter_ExtendedConnect(t *testing.T) {
	t.Parallel()

	split := newOracleSplitter(Options{})

	const descriptor = "(DESCRIPTION=(SERVICE_NAME=orcl))"

	payload := tnsHandshakePayload(319, 66)
	binary.BigEndian.PutUint16(payload[tnsConnectDataOffsetPos:tnsConnectDataOffsetPos+2],
		uint16(tnsHeaderLen+len(payload)))

	extended := mysqlConcat(
		binary.BigEndian.AppendUint16(nil, uint16(2+len(descriptor))),
		[]byte(descriptor),
	)

	packet := mysqlConcat(tnsLegacyPacket(tnsTypeConnect, payload), extended)

	// Split across two feeds so the extension has to be waited for.
	assert.Empty(t, feedSplitter(t, split, 0, dump.DirClientToServer, packet[:len(packet)-10]))
	assert.Equal(t,
		[]string{`1ms C> Connect version=319 (109 bytes)`},
		feedSplitter(t, split, ms, dump.DirClientToServer, packet[len(packet)-10:]),
	)
}

// TestOracleSplitter_OutOfSync checks a truncated capture is reported rather
// than silently producing nonsense: DBB_DUMP_MAX_SIZE drops whole packets.
func TestOracleSplitter_OutOfSync(t *testing.T) {
	t.Parallel()

	split := newOracleSplitter(Options{})

	_, err := split.Feed(&dump.Packet{
		Direction: dump.DirClientToServer,
		Data:      []byte{0x00, 0x04, 0x00, 0x00, tnsTypeData, 0x00, 0x00, 0x00},
	})
	require.ErrorIs(t, err, ErrOutOfSync)
}

// TestOracleSplitter_PartialMessagesYieldNothing checks a packet that
// completes no message produces no line rather than an error.
func TestOracleSplitter_PartialMessagesYieldNothing(t *testing.T) {
	t.Parallel()

	split := newOracleSplitter(Options{})

	packet := tnsDataPacket(0, []byte{ttcMsgPiggyback, ttcFuncExecSQL, 0x01, 0x02})

	assert.Empty(t, feedSplitter(t, split, 0, dump.DirClientToServer, packet[:3]))
	assert.Empty(t, feedSplitter(t, split, ms, dump.DirClientToServer, packet[3:len(packet)-1]))
	assert.Equal(t,
		[]string{`2ms C> PIGGYBACK exec-sql (4 bytes)`},
		feedSplitter(t, split, 2*ms, dump.DirClientToServer, packet[len(packet)-1:]),
	)
}

// oracleFixture is a recorded session from the Oracle proxy's own corpus.
// Unlike the other four protocols there are real captures in the tree, so the
// end-to-end test runs against bytes a real client actually sent rather than
// against bytes this package synthesized.
const oracleFixture = "go_ora.pcapng"

// TestFile_Oracle decodes a recorded go-ora session end to end, reader
// included.
func TestFile_Oracle(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "proxy", "oracle", "testdata", oracleFixture)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture %s not available", path)
	}

	var out strings.Builder
	require.NoError(t, File(path, Options{}, &out))

	lines := textOf(t, out.String())
	require.Greater(t, len(lines), 20)

	assert.Equal(t, []string{
		"# oracle session 019d5abe-f187-7ac0-a740-8ca4670fa006",
		"C> Connect version=317 (396 bytes)",
		"<S Accept version=317 (45 bytes)",
		"C> NativeServices(151 bytes)",
		"<S NativeServices(117 bytes)",
		"C> OSETPRO (18 bytes)",
		"<S OSETPRO (181 bytes)",
		"C> ODTYPES (2631 bytes)",
		"<S ODTYPES (2714 bytes)",
		"C> PIGGYBACK auth-phase-1 (184 bytes, redacted)",
		"<S Response (361 bytes, redacted)",
		"C> PIGGYBACK auth-phase-2 (1208 bytes, redacted)",
		"<S Response (2110 bytes, redacted)",
	}, lines[:13])

	// The session ends the way every recorded one does: a logoff, then the
	// empty Data packet carrying the end-of-file flag.
	assert.Contains(t, out.String(), "PIGGYBACK logoff")
	assert.Contains(t, out.String(), "Data(flags=0x0040)")

	// Every statement in this capture ran against a real database, and none of
	// its rows reach the trace.
	for _, name := range []string{"exec-sql", "QRESULT"} {
		assert.Contains(t, out.String(), name)
	}
}
