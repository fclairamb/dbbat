package oracle

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readAuthRejectFrame reads one whole frame written by sendAuthFailed off the
// wire and asserts the TNS framing around it: v315+ (4-byte length header, the
// 2-byte length field left 0x0000), Data type, empty data flags.
//
// The framing is read here rather than through readTNSPacket so it is itself
// under test — the regression it guards is a legacy 2-byte-framed reject, which
// a modern client misreads as an oversized packet and surfaces as ORA-12566 with
// no useful reason.
func readAuthRejectFrame(t *testing.T, conn net.Conn) []byte {
	t.Helper()

	header := make([]byte, 8)
	_, err := io.ReadFull(conn, header)
	require.NoError(t, err)

	total := int(binary.BigEndian.Uint32(header[0:4]))
	require.GreaterOrEqual(t, total, 8)

	frame := make([]byte, total)
	copy(frame, header)
	_, err = io.ReadFull(conn, frame[8:])
	require.NoError(t, err)

	assert.Equal(t, uint16(0), binary.BigEndian.Uint16(frame[0:2]),
		"2-byte length field must be 0x0000 for v315+ framing (else client reads ORA-12566)")
	assert.Equal(t, uint32(len(frame)), binary.BigEndian.Uint32(frame[0:4]),
		"4-byte length header must equal the total frame length")
	assert.Equal(t, byte(TNSPacketTypeData), frame[4], "packet type must be Data (0x06)")

	payload := frame[8:]
	require.GreaterOrEqual(t, len(payload), 3, "payload shorter than TTC header")
	assert.Equal(t, []byte{0x00, 0x00}, payload[:ttcDataFlagsSize], "data flags must be empty")

	return payload[ttcDataFlagsSize:]
}

// decodeAuthRejectOER reads the ORA code, message and call number out of an
// AUTH-reject body the way a *client* does: walk the summary object's fixed
// fields, step over exactly the number of trailing fields this client's parser
// expects, and take the message CLR from there.
//
// The strictness is the point, and it is what a tolerant "scan for ORA-" decode
// cannot say. dbbat used to answer an AUTH refusal with a TTC Response (0x08)
// carrying a compressed ORA code and a bare CLR; python-oracledb thin read that
// as return parameters, hit the CLR's length byte where it wanted a ub4, and
// reported `DPY-5002: internal error: read integer of length N when expecting
// integer of no more than length 4` — with N the byte length of dbbat's own
// message. A decode that finds the text by searching for it would have passed on
// that frame.
func decodeAuthRejectOER(t *testing.T, shape oerShape, body []byte) (int, string, byte) {
	t.Helper()

	require.NotEmpty(t, body)
	require.NotEqual(t, byte(TTCFuncResponse), body[0],
		"an AUTH refusal must be an OER, not a TTC Response: 0x08 is what python-oracledb "+
			"thin read as return parameters and reported as DPY-5002")
	require.Equal(t, byte(TTCFuncOERR), body[0], "TTC message type must be OER (0x04)")

	pos, errCode, callNumber, ok := skipOERFixedFields(shape, body, 0)
	require.True(t, ok, "the frame must walk as a summary object")

	for range shape.extraTailFields {
		_, n := readCompressedInt(body[pos:])
		require.NotZero(t, n, "truncated trailing field")

		pos += n
	}

	msg, n := readCLR(body[pos:])
	require.NotZero(t, n, "no message CLR where a client with this tail count looks for one")

	return errCode, string(msg), callNumber
}

// authRejectMessages are the three refusals a client can meet at AUTH Phase 2,
// with the ORA codes authRejectFor assigns them. The message lengths are carried
// along because they are the evidence in the bug report: python-oracledb thin
// reported "read integer of length 39" for the 39-byte message, "length 92" for
// the 92-byte one — the length byte of dbbat's own CLR, read as a ub4.
var authRejectMessages = []struct {
	name    string
	code    uint16
	message string
	msgLen  int
}{
	{"invalid credentials", ORA01017, "invalid username/password; logon denied", 39},
	{"no active grant", ORA01045, "no active grant for this database; request access via dbbat", 59},
	{
		"ambiguous service name", ORA01045,
		"service name matches multiple dbbat databases; connect using the dbbat database name instead", 92,
	},
}

// TestSendAuthFailed_EmitsAnOERAClientCanParse is the fix: each of the three
// AUTH-phase refusals arrives as a real end-of-call OER carrying the ORA code and
// the actionable text, in the layout the client on the other end parses.
func TestSendAuthFailed_EmitsAnOERAClientCanParse(t *testing.T) {
	t.Parallel()

	for _, tt := range authRejectMessages {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Len(t, tt.message, tt.msgLen,
				"the message length is the DPY-5002 evidence; a reworded message needs its length updated")

			client, server := newPipeConns(t)

			s := newTestErrorSession(t, server)

			go s.sendAuthFailed(tt.code, tt.message)

			body := readAuthRejectFrame(t, client)

			shape, _, _ := s.authRefusalOERShape()
			code, message, _ := decodeAuthRejectOER(t, shape, body)

			assert.Equal(t, int(tt.code), code,
				"reject frame must carry the chosen ORA code, not an abrupt EOF")
			assert.Equal(t, fmt.Sprintf("ORA-%05d: %s", tt.code, tt.message), message,
				"the text a client renders must name the code and the reason")
		})
	}
}

// TestSendAuthFailed_TailFollowsTheClientsChallenge pins the one field an
// AUTH-phase refusal cannot leave at its default, and the line it is decided on.
//
// A modern (customHash / 12c-18453) client's parser reads the two "fields added
// in Oracle Database 20c" between the wide RetCode pair and the message; a legacy
// one (go-ora, verifier 6949) does not. Send the wrong count and the client lands
// on the message's own length byte — which is DPY-5002 all over again, this time
// inside a well-formed OER.
//
// It is asserted through learnOERShape, i.e. through the same walk that learns a
// tail off a real server frame: only one field count puts an "ORA-" CLR where the
// client looks.
func TestSendAuthFailed_TailFollowsTheClientsChallenge(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		customHash   bool
		verifierType int
		want         int
	}{
		// go-ora: no customHash, legacy challenge, and a real 23ai server ends
		// its AUTH response with no extra fields (oerFixtureGoOraOK).
		"legacy client": {want: 0},

		// python-oracledb thin / JDBC thin / OCI: customHash, 18453 challenge,
		// and the same server sends two (oerFixturePythonOK).
		"modern client": {customHash: true, want: oerModernExtraTailFields},

		// The challenge that actually went out wins over the negotiation: a key
		// with no 18453 verifier is answered with the legacy challenge however
		// modern the server is.
		"modern server, legacy challenge": {customHash: true, verifierType: VerifierType6949, want: 0},
		"legacy server, modern challenge": {verifierType: VerifierType18453, want: oerModernExtraTailFields},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, server := newPipeConns(t)

			s := newTestErrorSession(t, server)
			s.upstreamCustomHash = tc.customHash
			s.clientVerifierType = tc.verifierType

			go s.sendAuthFailed(ORA01045, "no active grant for this database; request access via dbbat")

			body := readAuthRejectFrame(t, client)

			shape, _, _ := s.authRefusalOERShape()
			assert.Equal(t, tc.want, shape.extraTailFields)

			learned := defaultOERShape()
			require.True(t, learnOERShape(&learned, body),
				"the frame must decode as a summary object with a message at its tail")
			assert.Equal(t, tc.want, learned.extraTailFields,
				"the message CLR must sit exactly where this client's parser looks for it")

			_, message, _ := decodeAuthRejectOER(t, shape, body)
			assert.Contains(t, message, "ORA-01045: no active grant")
		})
	}
}

// TestSendAuthFailed_LearnedShapeWins keeps the guess above from overriding a
// tail read off a real upstream OER. An OCI session has one before it is
// challenged at all: beginUpstreamAuth runs first and learnOERTail reads the
// upstream's own AUTH summary out of the response.
func TestSendAuthFailed_LearnedShapeWins(t *testing.T) {
	t.Parallel()

	s := newTestErrorSession(t, nil)
	s.upstreamCustomHash = true
	s.learnOERTail(oerFixture(t, oerFixtureGoOraOK, ""))

	require.True(t, s.oer.tailLearned)

	shape, _, _ := s.authRefusalOERShape()
	assert.Equal(t, 0, shape.extraTailFields,
		"a tail measured off the upstream must beat the challenge-shaped guess")
}

// TestSendAuthFailed_CarriesTheAuthCallNumber: the client is parked in the
// receive for its own AUTH call, so the refusal has to end *that* call. ojdbc
// checks the number before it will read an OER at all, and a wrong one demotes
// the real ORA code to the cause of an ORA-18745.
func TestSendAuthFailed_CarriesTheAuthCallNumber(t *testing.T) {
	t.Parallel()

	authPkt := func(subOp, seq byte) *TNSPacket {
		return &TNSPacket{
			Type:    TNSPacketTypeData,
			Payload: []byte{0x00, 0x00, byte(TTCFuncPiggyback), subOp, seq, 0x00, 0x01},
		}
	}

	for name, tc := range map[string]struct {
		phase1 *TNSPacket
		phase2 *TNSPacket
		want   byte
	}{
		"refused before the challenge": {phase1: authPkt(PiggybackSubAuth1, 1), want: 1},
		"refused on the key":           {phase1: authPkt(PiggybackSubAuth1, 1), phase2: authPkt(PiggybackSubAuth2, 2), want: 2},
		"no AUTH packet recorded":      {want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, server := newPipeConns(t)

			s := newTestErrorSession(t, server)
			s.clientAuthPhase1Pkt = tc.phase1
			s.clientAuthPhase2Pkt = tc.phase2

			go s.sendAuthFailed(ORA01017, "invalid username/password; logon denied")

			body := readAuthRejectFrame(t, client)

			shape, _, _ := s.authRefusalOERShape()
			_, _, callNumber := decodeAuthRejectOER(t, shape, body)
			assert.Equal(t, tc.want, callNumber,
				"the refusal must end the AUTH call the client is waiting on")
		})
	}
}

// TestSendAuthFailed_OCIDialect keeps an OCI client's refusal in the encoding it
// parses. It is the AUTH-phase half of
// TestWriteTTCError_SeedsOCIEncodingFromAuthFraming, and it matters more here:
// this frame goes out *before* any statement, which is the moment nothing has
// been learned and the client's own AUTH framing is the only evidence there is.
func TestSendAuthFailed_OCIDialect(t *testing.T) {
	t.Parallel()

	client, server := newPipeConns(t)

	s := newTestErrorSession(t, server)
	s.clientWideEncoding = true
	s.upstreamCustomHash = true

	go s.sendAuthFailed(ORA01045, "no active grant for this database; request access via dbbat")

	body := readAuthRejectFrame(t, client)

	require.Equal(t, byte(TTCFuncOERR), body[0])
	assert.Equal(t, ORA01045, binary.LittleEndian.Uint16(body[oerFixedErrNumOffset:]),
		"an OCI client must get the fixed-width encoding even with nothing learned")
	assert.Equal(t, uint32(ORA01045), binary.LittleEndian.Uint32(body[oerFixedRetCodeOffset:]))
	assert.Equal(t, byte(ttcEndOfResponse), body[len(body)-1],
		"sqlplus waits for the end-of-response marker it negotiated")

	tail, ok := oerFixedWidthTailFieldsAt(body, 0)
	require.True(t, ok, "the frame must decode as a fixed-width summary object")
	assert.Equal(t, oerModernExtraTailFields, tail.extraFields)
}

func TestAuthRejectFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		wantCode     uint16
		wantFragment string
	}{
		{"no active grant is actionable", ErrNoActiveGrant, ORA01045, "grant"},
		{"unknown user is generic", ErrUserNotFound, ORA01017, "username/password"},
		{
			"wrapped no-grant still routes to 1045",
			fmt.Errorf("%w: user=x database=y", ErrNoActiveGrant), ORA01045, "grant",
		},
		{"other failures are generic", ErrDecryptedPasswordTooShort, ORA01017, "username/password"},
		{"ambiguous service name is actionable", ErrAmbiguousServiceName, ORA01045, "dbbat database name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, message := authRejectFor(tt.err)
			assert.Equal(t, tt.wantCode, code)
			assert.Contains(t, message, tt.wantFragment)
		})
	}
}
