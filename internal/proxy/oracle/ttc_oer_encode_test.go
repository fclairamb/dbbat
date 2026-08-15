package oracle

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPipeConns returns the two ends of an in-memory connection: the first is
// the client, the second what the session writes to.
func newPipeConns(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	return client, server
}

// newTestErrorSession builds a session for the refusal path. It deliberately
// leaves oer at its zero value, so these tests also cover the normalization a
// session that never saw a capability exchange falls back to.
func newTestErrorSession(t *testing.T, conn net.Conn) *session {
	t.Helper()

	s := newTestSession(nil)
	s.clientConn = conn

	return s
}

// The OERs below were measured off Oracle 23ai Free (gvenzl/oracle-free:23-slim)
// by tapping a client's own socket, with dbbat's Accept-flag stripping applied
// so the shape matches what a proxied session sees. They are the ground truth
// this encoder mirrors, and the reason the tail-field count is learned rather
// than derived: both clients negotiate the same TTC field version and the
// server answers them differently.
const (
	// oerFixtureGoOraError is ORA-00942 as sent to go-ora v3: the message CLR
	// follows the wide RetCode/row-count pair directly.
	oerFixtureGoOraError = "0401010108000203ae0000010201" +
		"0c020000000000000000000000000800000000" +
		"00000203ae0045"

	// oerFixturePythonError is the same error as sent to python-oracledb thin,
	// with the two "fields added in Oracle Database 20c" (a SQL type and a
	// server checksum) between that pair and the message.
	oerFixturePythonError = "0401010105000203ae0000010201" +
		"0e030000000000000000000000000400000000" +
		"00000203ae000103" + "00" + "45"

	// oerFixtureGoOraOK / oerFixturePythonOK are the success OER that ends each
	// client's AUTH response — the sample dbbat learns from before any
	// statement runs. They differ by exactly the same two trailing bytes.
	oerFixtureGoOraOK  = "040101010300000000000000000000000000000000000000020000000000000000"
	oerFixturePythonOK = oerFixtureGoOraOK + "0000"
)

// oraErrorText is the message body both error fixtures carry.
const oraErrorText = "ORA-00942: table or view \"SYSTEM\".\"NO_SUCH_TABLE_XYZ\" does not exist\n"

func oerFixture(t *testing.T, prefix string, message string) []byte {
	t.Helper()

	out := decodeHexString(t, prefix)
	if message != "" {
		out = append(out, []byte(message)...)
	}

	return out
}

// TestLearnOERShape_RealServerOERs is the whole justification for learning the
// tail rather than deriving it: two clients, one server, one negotiated TTC
// field version, two different layouts.
func TestLearnOERShape_RealServerOERs(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		hexPrefix string
		message   string
		want      int
	}{
		"go-ora error":  {oerFixtureGoOraError, oraErrorText, 0},
		"python error":  {oerFixturePythonError, oraErrorText, 2},
		"go-ora status": {oerFixtureGoOraOK, "", 0},
		"python status": {oerFixturePythonOK, "", 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			shape := defaultOERShape()
			require.True(t, learnOERShape(&shape, oerFixture(t, tc.hexPrefix, tc.message)))
			assert.Equal(t, tc.want, shape.extraTailFields)
			assert.True(t, shape.tailLearned)
		})
	}
}

// TestLearnOERShape_IgnoresNonOER keeps the scan from teaching itself a tail
// count off row bytes that merely start with 0x04.
func TestLearnOERShape_IgnoresNonOER(t *testing.T) {
	t.Parallel()

	shape := defaultOERShape()
	assert.False(t, learnOERShape(&shape, decodeHexString(t, "1004010203040506070809")))
	assert.False(t, shape.tailLearned)
	assert.Zero(t, shape.extraTailFields)
}

// TestLearnOERShape_EmbeddedInResponse covers the shape dbbat actually sees on
// a statement: the OER sits at the end of a Response (0x08) message, behind the
// return-parameter block.
func TestLearnOERShape_EmbeddedInResponse(t *testing.T) {
	t.Parallel()

	payload := append([]byte{byte(TTCFuncResponse), 0x01, 0x06, 0x03, 0x20, 0x47, 0x4b, 0x00},
		oerFixture(t, oerFixturePythonOK, "")...)

	shape := defaultOERShape()
	require.True(t, learnOERShape(&shape, payload))
	assert.Equal(t, 2, shape.extraTailFields)
}

// TestEncodeOER_MatchesServerFieldLayout walks dbbat's own frame with the same
// decoder that reads a server's, and requires it to land on the message.
func TestEncodeOER_MatchesServerFieldLayout(t *testing.T) {
	t.Parallel()

	for name, extra := range map[string]int{"go-ora shape": 0, "python shape": 2} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			shape := defaultOERShape()
			shape.extraTailFields = extra

			frame := encodeOER(shape, oerSummary{
				CallStatus:   1,
				SeqNumber:    8,
				ErrorCode:    1031,
				ErrorMessage: "ORA-01031: insufficient privileges: read-only grant",
			})

			require.Equal(t, byte(TTCFuncOERR), frame[0], "a server ends a call with message type 0x04")

			// Read back through the learner: it only succeeds when every field
			// lands where a client's parser expects it and the message CLR is
			// exactly where the tail says it is.
			learned := defaultOERShape()
			require.True(t, learnOERShape(&learned, frame))
			assert.Equal(t, extra, learned.extraTailFields)

			// And through this package's own OER decoder, which is a third
			// independent reading of the same bytes.
			info := decodeOERAt(defaultOERShape(), frame, 0)
			require.NotNil(t, info, "the end-of-call bit must be set so a call boundary is recognized")
			assert.Equal(t, 1031, info.ErrorCode)
			assert.Equal(t, 8, info.SeqNumber)
			assert.Equal(t, "ORA-01031: insufficient privileges: read-only grant", info.ErrorMessage)
		})
	}
}

// TestEncodeOER_LegacyTTCVersion drops the batch-error blocks and the wide
// pair, which is what a pre-12c negotiation reads.
func TestEncodeOER_LegacyTTCVersion(t *testing.T) {
	t.Parallel()

	shape := defaultOERShape()
	shape.ttcVersion = 5

	frame := encodeOER(shape, oerSummary{ErrorCode: 1031, ErrorMessage: "ORA-01031: nope"})

	modern := encodeOER(defaultOERShape(), oerSummary{ErrorCode: 1031, ErrorMessage: "ORA-01031: nope"})

	assert.Less(t, len(frame), len(modern),
		"the pre-12c layout has no wide RetCode/row-count pair")
	assert.Contains(t, string(frame), "ORA-01031: nope")
}

// TestEncodeOER_HonoursCapabilityFlags proves the two capability-derived
// variations actually change the frame, so a server that clears either is not
// silently sent the other layout.
func TestEncodeOER_HonoursCapabilityFlags(t *testing.T) {
	t.Parallel()

	full := encodeOER(defaultOERShape(), oerSummary{CallStatus: 1, SeqNumber: 9, ErrorCode: 1031})

	noStatus := defaultOERShape()
	noStatus.hasEndOfCallStatus = false

	noECID := defaultOERShape()
	noECID.hasECIDSequence = false

	assert.Less(t, len(encodeOER(noStatus, oerSummary{CallStatus: 1, SeqNumber: 9, ErrorCode: 1031})), len(full))
	assert.Less(t, len(encodeOER(noECID, oerSummary{CallStatus: 1, SeqNumber: 9, ErrorCode: 1031})), len(full))
}

// TestObserveOERCapabilities_SetProtocolReply reads the flags off a real Set
// Protocol reply, located the way a client locates them (by parsing the
// preamble) rather than by scanning for a byte pattern.
func TestObserveOERCapabilities_SetProtocolReply(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_dml.pcapng")

	var found bool

	for i := range td.Packets {
		pkt, err := parseTNSFromDumpPacket(td.Packets[i].Data)
		if err != nil {
			continue
		}

		shape := oerShape{ttcVersion: 99}
		if !observeOERCapabilities(&shape, pkt.Raw) {
			continue
		}

		found = true

		assert.True(t, shape.hasEndOfCallStatus, "every measured server advertises the end-of-call status")
		assert.True(t, shape.hasECIDSequence, "every measured server advertises the ECID sequence")
		assert.Equal(t, 27, shape.ttcVersion, "Oracle 23ai advertises TTC field version 27")

		break
	}

	require.True(t, found, "the capture must contain a Set Protocol reply")
}

// TestObserveClientTTCVersion_SetDataTypes lowers the negotiated version to the
// client's own, which is the half of the minimum the server does not tell us.
func TestObserveClientTTCVersion_SetDataTypes(t *testing.T) {
	t.Parallel()

	// [func 0x02][charset:2][charset:2][serverFlags][capsLen][caps...]
	caps := make([]byte, 54)
	caps[oerCapFieldVersionIndex] = 11

	body := append([]byte{byte(TTCFuncSetDataTypes), 0x69, 0x03, 0x69, 0x03, 0x03, byte(len(caps))}, caps...)

	shape := defaultOERShape()
	require.True(t, observeClientTTCVersion(&shape, body))
	assert.Equal(t, 11, shape.ttcVersion)

	// A version higher than what is already recorded never raises it: the
	// negotiated value is the minimum of both ends'.
	caps[oerCapFieldVersionIndex] = 24
	body = append([]byte{byte(TTCFuncSetDataTypes), 0x69, 0x03, 0x69, 0x03, 0x03, byte(len(caps))}, caps...)

	require.True(t, observeClientTTCVersion(&shape, body))
	assert.Equal(t, 11, shape.ttcVersion)
}

// TestObserveClientTTCVersion_NotSetDataTypes leaves the shape alone for
// anything else on the wire, a fast-auth bundle included.
func TestObserveClientTTCVersion_NotSetDataTypes(t *testing.T) {
	t.Parallel()

	shape := defaultOERShape()
	before := shape.ttcVersion

	assert.False(t, observeClientTTCVersion(&shape, []byte{byte(TTCFuncSetProtocol), 0x06, 0x00}))
	assert.False(t, observeClientTTCVersion(&shape, nil))
	assert.Equal(t, before, shape.ttcVersion)
}

// TestWriteTTCError_FramesV315 is the regression for the other half of the
// hang: the body was only ever half the problem, the legacy 2-byte TNS length
// header was the rest. A v315 client reads bytes 0..3 as the length, so a
// legacy-framed error announces a multi-megabyte packet and the client blocks
// waiting for bytes that never come.
func TestWriteTTCError_FramesV315(t *testing.T) {
	t.Parallel()

	client, server := newPipeConns(t)

	s := newTestErrorSession(t, server)

	go func() { _ = s.writeTTCError(1031, "read-only grant") }()

	pkt, err := readTNSPacket(client)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeData, pkt.Type)

	// The v315 form: the 2-byte legacy length reads as zero and the real length
	// is the 4-byte field.
	assert.Zero(t, binary.BigEndian.Uint16(pkt.Raw[0:2]), "the legacy length field must be zero")
	assert.Len(t, pkt.Raw, int(binary.BigEndian.Uint32(pkt.Raw[0:4])), "the 4-byte length must cover the frame")

	body := pkt.Payload[ttcDataFlagsSize:]
	require.Equal(t, byte(TTCFuncOERR), body[0])

	info := decodeOERAt(defaultOERShape(), body, 0)
	require.NotNil(t, info)
	assert.Equal(t, 1031, info.ErrorCode)
	assert.True(t, strings.HasPrefix(info.ErrorMessage, "ORA-01031: read-only grant"))
}

// TestWriteTTCError_TruncatesLongMessage keeps a synthesized message inside the
// short CLR form, whose encoding does not depend on what the session
// negotiated for chunked values.
func TestWriteTTCError_TruncatesLongMessage(t *testing.T) {
	t.Parallel()

	client, server := newPipeConns(t)

	s := newTestErrorSession(t, server)

	go func() { _ = s.writeTTCError(1031, strings.Repeat("x", 4000)) }()

	pkt, err := readTNSPacket(client)
	require.NoError(t, err)

	body := pkt.Payload[ttcDataFlagsSize:]
	info := decodeOERAt(defaultOERShape(), body, 0)
	require.NotNil(t, info)
	assert.Len(t, info.ErrorMessage, oerMaxMessageLen)
}

// TestSessionLearnOERTail_TracksSequence keeps a synthesized OER's end-to-end
// sequence walking forward with the session rather than restarting at one.
func TestSessionLearnOERTail_TracksSequence(t *testing.T) {
	t.Parallel()

	s := newTestErrorSession(t, nil)

	s.learnOERTail(oerFixture(t, oerFixtureGoOraError, oraErrorText))
	assert.Equal(t, 8, s.oerSeq, "the fixture carries ECID sequence 8")
	assert.True(t, s.oer.tailLearned)
	_, seq, _ := s.nextOERFrame()
	assert.Equal(t, 9, seq)
}

// oerFixtureSqlplusError is ORA-00942 as Oracle 23ai Free sends it to the
// Homebrew sqlplus 23 client: the same summary object, marshaled as
// fixed-width little-endian integers (only the row count stays compressed) and
// terminated by the TTC end-of-response marker. Both of those had to be
// reproduced before sqlplus stopped hanging on a refusal.
const oerFixtureSqlplusError = "0401000000330000000000ae0300000000020" +
	"0" + "0e00" + "03" + "0000000000000000000000000000000000000000000000" +
	"0e" + "0000000000000000000000000000000000000000" +
	"ae030000" + "00" + "0300000000000000" + "45"

// oerFixtureSqlplusStatus is the ORA-01403 end-of-fetch status from the same
// client, which is the shape learned from a session that never errors.
const oerFixtureSqlplusStatus = "04010000002d000100000" +
	"07b0500000000020000000" +
	"3" + "0000000000000000000000000000000000000000000000" +
	"07" + "0000000000000000000000000000000000000000" +
	"7b050000" + "0101" + "0300000000000000" + "19"

const oraNoDataText = "ORA-01403: no data found\n"

// TestLearnOERShape_FixedWidthOCI covers the OCI/sqlplus encoding: a third
// on-wire shape for the same object, learned the same way.
func TestLearnOERShape_FixedWidthOCI(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		hexPrefix string
		message   string
	}{
		"error":  {oerFixtureSqlplusError, oraErrorText},
		"status": {oerFixtureSqlplusStatus, oraNoDataText},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			payload := append(oerFixture(t, tc.hexPrefix, tc.message), ttcEndOfResponse)

			shape := defaultOERShape()
			require.True(t, learnOERShape(&shape, payload))
			assert.True(t, shape.fixedWidth, "the OCI encoding must not be read as the compressed one")
			assert.Equal(t, 2, shape.extraTailFields)
			assert.True(t, shape.endOfResponse, "the marker sqlplus waits for must be learned")

			// And it must not be read as the *other* OCI encoding. The learner
			// tries the 64-bit layout first, so this is the assertion that
			// keeps a widening of that layout's guards from quietly capturing
			// every Instant Client session — where it would produce a frame
			// twice the size of the one the client is waiting for.
			assert.False(t, shape.fixedWidth64,
				"an Instant Client summary must stay on the 32-bit layout")
			assert.Equal(t, oerFixed32Layout, shape.layoutFor())
		})
	}
}

// TestLearnOERShape_ThinSessionsLearnNoEndOfResponse is the other side of that:
// dbbat clears HAS_END_OF_RESPONSE from the Accept, so a thin client's messages
// carry no marker and dbbat's frame must not invent one.
func TestLearnOERShape_ThinSessionsLearnNoEndOfResponse(t *testing.T) {
	t.Parallel()

	shape := defaultOERShape()
	require.True(t, learnOERShape(&shape, oerFixture(t, oerFixtureGoOraError, oraErrorText)))
	assert.False(t, shape.fixedWidth)
	assert.False(t, shape.fixedWidth64, "a thin session is neither OCI layout")
	assert.False(t, shape.endOfResponse)
}

// TestEncodeOER_FixedWidthRoundTrip walks dbbat's OCI frame back through the
// learner, which is the only reading of it that checks every field offset.
func TestEncodeOER_FixedWidthRoundTrip(t *testing.T) {
	t.Parallel()

	shape := defaultOERShape()
	shape.fixedWidth = true
	shape.extraTailFields = 2

	frame := encodeOER(shape, oerSummary{
		CallStatus:   1,
		SeqNumber:    99,
		ErrorCode:    1031,
		ErrorMessage: "ORA-01031: read-only grant",
	})

	require.Equal(t, byte(TTCFuncOERR), frame[0])
	assert.Len(t, frame, oerFixedWidthPrefixLen+1+2*oerFixedTailFieldWidth+1+len("ORA-01031: read-only grant"),
		"the OCI prefix is a constant 70 bytes, then the compressed row count, the tail and the message")

	learned := defaultOERShape()
	require.True(t, learnOERShape(&learned, frame))
	assert.True(t, learned.fixedWidth)
	assert.Equal(t, 2, learned.extraTailFields)
}

// TestWriteTTCError_OCIFraming is the sqlplus regression: the fixed-width body
// and the end-of-response marker together, because either one alone still
// hangs the client.
func TestWriteTTCError_OCIFraming(t *testing.T) {
	t.Parallel()

	client, server := newPipeConns(t)

	s := newTestErrorSession(t, server)
	s.oer = defaultOERShape()
	s.oer.fixedWidth = true
	s.oer.endOfResponse = true
	s.oer.extraTailFields = 2
	s.oer.tailLearned = true

	go func() { _ = s.writeTTCError(1031, "read-only grant") }()

	pkt, err := readTNSPacket(client)
	require.NoError(t, err)

	body := pkt.Payload[ttcDataFlagsSize:]
	require.Equal(t, byte(TTCFuncOERR), body[0])
	assert.Equal(t, byte(ttcEndOfResponse), body[len(body)-1],
		"sqlplus waits for the end-of-response marker it negotiated")

	assert.Equal(t, uint16(1031), binary.LittleEndian.Uint16(body[oerFixedErrNumOffset:]))
	assert.Equal(t, uint32(1031), binary.LittleEndian.Uint32(body[oerFixedRetCodeOffset:]))
}

// TestWriteTTCError_SeedsOCIEncodingFromAuthFraming keeps a session that never
// got a sample from falling back to the thin-client encoding for an OCI client.
func TestWriteTTCError_SeedsOCIEncodingFromAuthFraming(t *testing.T) {
	t.Parallel()

	client, server := newPipeConns(t)

	s := newTestErrorSession(t, server)
	s.clientWideEncoding = true

	go func() { _ = s.writeTTCError(1031, "read-only grant") }()

	pkt, err := readTNSPacket(client)
	require.NoError(t, err)

	body := pkt.Payload[ttcDataFlagsSize:]
	assert.Equal(t, uint32(1031), binary.LittleEndian.Uint32(body[oerFixedRetCodeOffset:]),
		"an OCI client must get the fixed-width encoding even with nothing learned")
}

// TestServerOERsCarryTheClientCallNumber is the measurement behind
// oerSummary.CallNumber, made on frames that were already in this repo before
// the field was written at all: every OER a real Oracle sends carries the TTC
// sequence number of the request it answers, in the field dbbat used to leave
// at zero.
//
// It is read back through skipOERFixedFields — the same walk the tail learner
// makes — so the field's position is pinned by one decoder, not by an offset
// spelled out twice.
func TestServerOERsCarryTheClientCallNumber(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		hexPrefix string
		want      byte
	}{
		"go-ora":          {oerFixtureGoOraError, 8},
		"python-oracledb": {oerFixturePythonError, 4},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, errCode, callNumber, ok := skipOERFixedFields(defaultOERShape(),
				oerFixture(t, tc.hexPrefix, oraErrorText), 0)

			require.True(t, ok)
			assert.Equal(t, 942, errCode)
			assert.Equal(t, tc.want, callNumber,
				"a server answers the client's own TTC sequence number; dbbat wrote zero here until "+
					"ojdbc 26.1 was measured refusing to read the error out of such a frame")
		})
	}

	// The OCI encoding carries it too, at a constant offset.
	oci := oerFixture(t, oerFixtureSqlplusError, oraErrorText)
	assert.Equal(t, uint32(14), binary.LittleEndian.Uint32(oci[oerFixedCallNumberOffset:]),
		"the sqlplus fixture answers TTC sequence 14")
}

// TestEncodeOER_CarriesTheCallNumber is the writing half: dbbat's own frame has
// to put the number where a server puts it, in both encodings.
//
// This is what ojdbc 26.1 checks before it will read an OER at all
// (T4CTTIfun.receive: `oer.callNumber == this.sequenceNumber`, else
// handleOutOfSequenceError). With a zero here, a live JDBC client reported
// every refusal as "ORA-18745: Execution error in sessionless transaction
// piggybacked call" with the real ORA-01031 demoted to its cause.
func TestEncodeOER_CarriesTheCallNumber(t *testing.T) {
	t.Parallel()

	t.Run("compressed", func(t *testing.T) {
		t.Parallel()

		shape := defaultOERShape()
		shape.extraTailFields = 2

		frame := encodeOER(shape, oerSummary{
			CallStatus:   1,
			SeqNumber:    8,
			ErrorCode:    1031,
			ErrorMessage: "ORA-01031: read-only grant",
			CallNumber:   0x2a,
		})

		_, errCode, callNumber, ok := skipOERFixedFields(shape, frame, 0)
		require.True(t, ok, "the frame must still walk as a summary object")
		assert.Equal(t, 1031, errCode)
		assert.Equal(t, byte(0x2a), callNumber)

		// And the rest of the frame is undisturbed: the tail learner still finds
		// the message exactly where the tail count says it is.
		learned := defaultOERShape()
		require.True(t, learnOERShape(&learned, frame))
		assert.Equal(t, 2, learned.extraTailFields)
	})

	t.Run("fixed width", func(t *testing.T) {
		t.Parallel()

		shape := defaultOERShape()
		shape.fixedWidth = true
		shape.extraTailFields = 2

		frame := encodeOER(shape, oerSummary{
			CallStatus:   1,
			SeqNumber:    8,
			ErrorCode:    1031,
			ErrorMessage: "ORA-01031: read-only grant",
			CallNumber:   0x2a,
		})

		assert.Equal(t, uint32(0x2a), binary.LittleEndian.Uint32(frame[oerFixedCallNumberOffset:]))

		learned := defaultOERShape()
		require.True(t, learnOERShape(&learned, frame))
		assert.True(t, learned.fixedWidth)
		assert.Equal(t, 2, learned.extraTailFields)
	})
}

// TestClientCallNumber covers how the number is picked off the client's own
// request, including the shape that made this bug visible: JDBC staples the
// execute behind a close-cursors list, and the call the client waits on is the
// stapled one, not the closes.
func TestClientCallNumber(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		payload string
		want    byte
		ok      bool
	}{
		// `03 5e 06 00 …` — a piggyback execute, as go-ora sends it.
		"piggyback execute": {"035e0600028021000101", 6, true},

		// `11 69 06 00 <closes> 03 5e 07 00 …` — JDBC's execute behind its
		// close list. Oracle 23ai Free answers this exact packet with an OER
		// carrying 7.
		"close list then execute": {"11690600" + "0101010102" + "035e0700" + "028121000101", 7, true},

		// A close list with nothing stapled behind it is its own call.
		"close list alone": {"116906000101010102", 6, true},

		"too short": {"0311", 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := clientCallNumber(decodeHexString(t, tc.payload))
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSessionRefusalUsesTheClientCallNumber is the wiring: whatever the client
// last asked for is what a refusal ends.
func TestSessionRefusalUsesTheClientCallNumber(t *testing.T) {
	t.Parallel()

	client, server := newPipeConns(t)

	s := newTestErrorSession(t, server)
	s.oer = defaultOERShape()
	s.oer.tailLearned = true

	s.observeClientCallNumber(decodeHexString(t, "035e1100028021000101"))

	go func() { _ = s.writeTTCError(1031, "read-only grant") }()

	pkt, err := readTNSPacket(client)
	require.NoError(t, err)

	body := pkt.Payload[ttcDataFlagsSize:]

	_, _, callNumber, ok := skipOERFixedFields(s.oer, body, 0)
	require.True(t, ok)
	assert.Equal(t, byte(0x11), callNumber, "the refusal ends the call the client is waiting on")
}
