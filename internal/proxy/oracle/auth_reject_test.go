package oracle

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
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

// mutualizedServiceName is a service name shared by several dbbat databases and
// held by none of them as its own name, which is what makes resolveDatabase fall
// through to the candidate list — the mutualized-instance case (a real one: the
// report behind this frame was against a service called MUTU01).
const mutualizedServiceName = "MUTU01"

// TestDisambiguateDatabase_RefusesWithAReadableORAError closes the one gap the
// tests above leave. They assert the frame for all three refusal messages, but
// two of the three *paths* that produce one live in disambiguateDatabase, and
// nothing exercised it: the integration cases go through authenticateClient's
// grant check against a single-database fixture, where disambiguateDatabase
// no-ops (`len(candidates) < 2`).
//
// So this drives the real branch, against a real store, and follows the same
// three steps run() does — disambiguateDatabase → authRejectFor → sendAuthFailed
// — decoding what lands on the socket. A wrong ORA code, a wrong message, or a
// frame no client can read would fail here.
//
// The store is dbbat's own PostgreSQL and nothing else: the branch is decided by
// which candidates the user holds grants on, so no Oracle is involved and this
// stays out of the integration suite.
func TestDisambiguateDatabase_RefusesWithAReadableORAError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataStore := newOracleTestStore(t)

	// The server rows' password is never decrypted on this path, so any 32-byte
	// key encrypts it.
	serverKey := make([]byte, 32)
	for i := range serverKey {
		serverKey[i] = byte(0x5A + i)
	}

	// Three databases behind one upstream service name — the shape the branch
	// exists for. Which of them the user can reach is what each subtest sets.
	candidates := make([]store.Server, 0, 3)

	for _, name := range []string{"mutu_a", "mutu_b", "mutu_c"} {
		service := mutualizedServiceName

		db, err := dataStore.CreateServer(ctx, &store.Server{
			Name:              name,
			Host:              "127.0.0.1",
			Port:              1521,
			DatabaseName:      name,
			OracleServiceName: &service,
			Username:          "system",
			Password:          "oracle",
			Protocol:          store.ProtocolOracle,
		}, serverKey)
		require.NoError(t, err)

		candidates = append(candidates, *db)
	}

	for name, tc := range map[string]struct {
		// username is this subtest's own user, so the subtests can run in
		// parallel: the branch is decided by grants, which are per user.
		username string

		// grantOn is how many of the candidates that user holds a grant on.
		grantOn int

		wantErr     error
		wantCode    uint16
		wantMessage string
	}{
		// The reported case: grants on several databases behind one service name.
		// Nobody can pick for the user, so they are told to name the database.
		"several candidates": {
			username: "mutuseveral",
			grantOn:  2,
			wantErr:  ErrAmbiguousServiceName,
			wantCode: ORA01045,
			wantMessage: "ORA-01045: service name matches multiple dbbat databases; " +
				"connect using the dbbat database name instead",
		},

		// No grant on any of them is the ordinary no-access answer, reached here
		// rather than in authenticateClient.
		"no candidate": {
			username:    "mutunone",
			grantOn:     0,
			wantErr:     ErrNoActiveGrant,
			wantCode:    ORA01045,
			wantMessage: "ORA-01045: no active grant for this database; request access via dbbat",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			phase1Pkt := newMutuProbeUser(t, dataStore, tc.username, candidates[:tc.grantOn])

			client, server := newPipeConns(t)

			s := newTestErrorSession(t, server)
			s.store = dataStore
			s.serviceName = mutualizedServiceName
			s.database = &candidates[0]
			s.databaseCandidates = candidates
			s.clientAuthPhase1Pkt = phase1Pkt

			// The three steps run() performs on this branch, in order.
			err := s.disambiguateDatabase(phase1Pkt)
			require.ErrorIs(t, err, tc.wantErr)

			code, message := authRejectFor(err)
			assert.Equal(t, tc.wantCode, code)

			go s.sendAuthFailed(code, message)

			body := readAuthRejectFrame(t, client)

			shape, _, _ := s.authRefusalOERShape()
			gotCode, gotMessage, callNumber := decodeAuthRejectOER(t, shape, body)

			assert.Equal(t, int(tc.wantCode), gotCode)
			assert.Equal(t, tc.wantMessage, gotMessage,
				"the client has to read the actionable sentence for this branch")
			assert.Equal(t, authPhase1FuncSeq, callNumber,
				"refused before the challenge, the client is parked on AUTH Phase 1")
		})
	}

	// The branch that is not a refusal, for the same reason the two above are
	// tested: it is the one that has to keep working when they fire.
	t.Run("exactly one candidate selects it", func(t *testing.T) {
		t.Parallel()

		phase1Pkt := newMutuProbeUser(t, dataStore, "mutuone", candidates[1:2])

		s := newTestErrorSession(t, nil)
		s.store = dataStore
		s.serviceName = mutualizedServiceName
		s.database = &candidates[0]
		s.databaseCandidates = candidates

		require.NoError(t, s.disambiguateDatabase(phase1Pkt))
		assert.Equal(t, candidates[1].UID, s.database.UID,
			"the one database the user can reach must be the one selected, not the relay's placeholder")
	})
}

// newMutuProbeUser creates a user holding a grant on each of grantOn and returns
// the AUTH Phase 1 packet a thin client would send for it.
//
// One user per subtest is what lets them run in parallel: the branch
// disambiguateDatabase takes is decided by *this user's* grants, so nothing is
// shared but the candidate rows, which are read-only.
//
// The packet comes out of the same builder dbbat uses to drive a Phase 1 upstream
// rather than a hand-written body, and the username is read back through the
// production parser before it is handed over — a probe whose name cannot be
// parsed would send every branch to ErrUserNotFound and prove nothing.
func newMutuProbeUser(t *testing.T, dataStore *store.Store, username string, grantOn []store.Server) *TNSPacket {
	t.Helper()

	ctx := context.Background()

	user, err := dataStore.CreateUser(ctx, username,
		"$argon2id$v=19$m=4096,t=3,p=1$salt$hash", []string{"connector"})
	require.NoError(t, err)

	for i := range grantOn {
		_, err := testsupport.CreateGrantWithControls(ctx, t, dataStore, user.UID, grantOn[i].UID, nil)
		require.NoError(t, err)
	}

	body := buildClientAuthPhase1((&session{}).syntheticAuthHeader(PiggybackSubAuth1, authPhase1FuncSeq),
		strings.ToUpper(username), defaultDriverIdentity(), logonModeNoNewPass, false)

	pkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, body...),
	}

	parsed, err := parseAuthPhase1(pkt.Payload)
	require.NoError(t, err)
	require.Equal(t, strings.ToUpper(username), parsed,
		"the probe packet has to carry a username the production parser recovers")

	return pkt
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
