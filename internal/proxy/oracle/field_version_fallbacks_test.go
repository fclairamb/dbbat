package oracle

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// These tests pin the fallbacks that answer a client before anything was
// learned off an upstream OER, and that were measured against 23ai only: the
// thin 18453 challenge summary, the AUTH-refusal tail, and the OCI
// end-of-response marker. Each one depends on the *server's* TTC field version
// (ServerCompileTimeCaps[7]), and Oracle 18c XE advertises one below 20c, where
// none of the three applies. See docs/oracle.md, "Oracle 18c XE".

// setProtocolReplyWithFieldVersion is a Set Protocol reply advertising
// fieldVersion, long enough to carry the end-of-call and ECID flags.
func setProtocolReplyWithFieldVersion(t *testing.T, fieldVersion byte) []byte {
	t.Helper()

	caps := make([]byte, 42)
	caps[oerCapFieldVersionIndex] = fieldVersion
	caps[oerCapEndOfCallIndex] = 0x01
	caps[oerCapECIDIndex] = 0x01

	return buildSetProtocolReply(t, caps)
}

// summaryBytesAfterSeq is the width of an end-of-call summary past its
// compressed call status and end-to-end sequence number, the two fields whose
// width depends on their value. What is left is one byte per remaining field,
// all zero in a challenge summary, so it counts the fields the client parses.
func summaryBytesAfterSeq(t *testing.T, trailer []byte) int {
	t.Helper()

	require.GreaterOrEqual(t, len(trailer), 3)
	require.Equal(t, byte(TTCFuncOERR), trailer[0])

	pos := 1
	pos += 1 + int(trailer[pos]) // call status
	pos += 1 + int(trailer[pos]) // sequence number

	return len(trailer) - pos
}

func TestObserveOERCapabilities_RecordsTheServersOwnFieldVersion(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]int{
		goOra19cDump:        12, // Oracle 19c SE2
		"go_ora_dml.pcapng": 27, // Oracle 23ai Free
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(nil)

			found := false

			for _, pkt := range loadTestDump(t, name).Packets {
				tns, err := parseTNSFromDumpPacket(pkt.Data)
				if err != nil || pkt.Direction == dump.DirClientToServer {
					continue
				}

				s.observeOERServerCaps(tns.Raw)

				if s.oerShapeSnapshot().serverFieldVersion != 0 {
					found = true

					break
				}
			}

			require.True(t, found, "the capture must carry a Set Protocol reply")
			assert.Equal(t, want, s.oerShapeSnapshot().serverFieldVersion,
				"the server's own version, neither floored nor lowered to the client's")
		})
	}
}

// TestClientChallengeTrailer_JDBCThinOnAPre20cServer is the 18c XE login
// failure: JDBC thin threw ORA-17401 `Protocol violation [0]` from doOAUTH
// because the hand-built 18453 summary carried the two 20c fields the server's
// negotiation does not have.
//
// The expected width is a measurement, not a derivation: dbeaver_init.pcapng is
// JDBC thin against a real Oracle 19c, and the AUTH Phase 1 response that
// server sent carries a summary two fields shorter than the 23ai capture.
func TestClientChallengeTrailer_JDBCThinOnAPre20cServer(t *testing.T) {
	t.Parallel()

	var real19c []byte

	s := newTestSession(nil)

	for _, pkt := range loadTestDump(t, dbeaver19cDump).Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || pkt.Direction == dump.DirClientToServer {
			continue
		}

		s.observeOERServerCaps(tns.Raw)

		if bytes.Contains(tns.Raw, []byte("AUTH_SESSKEY")) {
			resp := &upstreamAuthResponse{properties: map[string]string{}}
			require.True(t, parseAuthMessageStream(extractTTCPayload(tns.Payload), resp, false, false))

			real19c = resp.challengeTrailer
		}
	}

	require.NotEmpty(t, real19c, "the 19c capture must carry its AUTH Phase 1 challenge")

	got := s.clientChallengeTrailer(VerifierType18453)
	modern := buildAuthChallengeEndMarker(VerifierType18453, false)

	assert.Equal(t, summaryBytesAfterSeq(t, real19c), summaryBytesAfterSeq(t, got),
		"dbbat's challenge must carry exactly the fields the 19c server itself sends JDBC thin")
	assert.Equal(t, modern[:len(modern)-oerModernExtraTailFields], got,
		"it is the 23ai summary without its two 20c fields")
}

func TestClientChallengeTrailer_KeepsThe20cFieldsWhereTheyExist(t *testing.T) {
	t.Parallel()

	modern := buildAuthChallengeEndMarker(VerifierType18453, false)

	t.Run("23ai server", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(nil)
		s.observeOERServerCaps(setProtocolReplyWithFieldVersion(t, 27))

		assert.Equal(t, modern, s.clientChallengeTrailer(VerifierType18453))
	})

	t.Run("no Set Protocol reply read", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, modern, newTestSession(nil).clientChallengeTrailer(VerifierType18453),
			"unknown is not older: the 23ai-measured summary stands")
	})

	t.Run("6949 challenge on 18c", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(nil)
		s.observeOERServerCaps(setProtocolReplyWithFieldVersion(t, 11))

		assert.Equal(t, buildAuthChallengeEndMarker(VerifierType6949, false), s.clientChallengeTrailer(VerifierType6949),
			"the legacy summary never carried the pair")
	})

	t.Run("OCI fallback on 18c", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(nil)
		s.clientWideEncoding = true
		s.observeOERServerCaps(setProtocolReplyWithFieldVersion(t, 11))

		assert.Equal(t, buildAuthChallengeEndMarker(VerifierType18453, true), s.clientChallengeTrailer(VerifierType18453),
			"the fixed-width summary is a different object, left alone")
	})
}

func TestAuthRefusalOERShape_NoModernTailOnAPre20cServer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		fieldVersion byte
		want         int
	}{
		{"18c XE", 11, 0},
		{"19c", 12, 0},
		{"23ai", 27, oerModernExtraTailFields},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(nil)
			s.upstreamCustomHash = true
			s.clientVerifierType = VerifierType18453
			s.observeOERServerCaps(setProtocolReplyWithFieldVersion(t, tc.fieldVersion))

			shape, _, _ := s.authRefusalOERShape()
			assert.Equal(t, tc.want, shape.extraTailFields)
		})
	}
}

// TestNextOERFrame_NoEndOfResponseBelow23_4 is the sqlplus 18c refusal: the
// unlearned OCI fallback appended the end-of-response marker, which no server
// below 23.4 can negotiate, and the 18c client dropped the session on it
// (ORA-03113 then ORA-03114) instead of reporting the refusal.
func TestNextOERFrame_NoEndOfResponseBelow23_4(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		fieldVersion byte
		want         bool
	}{
		{"18c XE", 11, false},
		{"23ai", 27, true},
		{"no Set Protocol reply read", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(nil)
			s.clientWideEncoding = true

			if tc.fieldVersion != 0 {
				s.observeOERServerCaps(setProtocolReplyWithFieldVersion(t, tc.fieldVersion))
			}

			shape, seq, call := s.nextOERFrame()
			assert.Equal(t, tc.want, shape.endOfResponse)

			frame := encodeOERPacket(shape, oerSummary{CallStatus: 1, SeqNumber: seq, ErrorCode: 1031, ErrorMessage: "ORA-01031: refused", CallNumber: call})
			assert.Equal(t, tc.want, frame[len(frame)-1] == ttcEndOfResponse,
				"the marker is on the wire exactly when the shape says so")
		})
	}
}

// oci64_18cServerReplies are the responses the 18c XE server sent sqlplus 18.4
// on the nightly leg, every one of which ends in that server's own OER.
var oci64_18cServerReplies = []string{
	"testdata/oci64_18c_describe.hex",
	"testdata/oci64_18c_lob.hex",
	"testdata/oci64_18c_long.hex",
	"testdata/oci64_18c_sevencols.hex",
	"testdata/oci64_18c_refcursor_bind_output.hex",
}

// oci64_18cRefusalSession is an 18c sqlplus session at the point it is
// refused: the 64-bit OCI client, the 18453 challenge, and a server below 20c.
func oci64_18cRefusalSession(t *testing.T) *session {
	t.Helper()

	s := newTestSession(nil)
	s.clientWideEncoding = true
	s.clientWide64Encoding = true
	s.upstreamCustomHash = true
	s.clientVerifierType = VerifierType18453
	s.observeOERServerCaps(setProtocolReplyWithFieldVersion(t, 11))

	return s
}

// TestRefusalOn18cIsShapedLikeThe18cServersOwnOER takes the server shape off
// the 18c XE server's own replies rather than off a hand-built one, and checks
// the refusal bytes dbbat sends an 18c sqlplus against it: the AUTH refusal and
// the mid-session one, learned and unlearned, all carry the tail that server
// writes and no end-of-response marker.
func TestRefusalOn18cIsShapedLikeThe18cServersOwnOER(t *testing.T) {
	t.Parallel()

	learned := oci64_18cRefusalSession(t)
	samples := 0

	for _, path := range oci64_18cServerReplies {
		for i, frame := range recordedFrames(t, path) {
			ttc := extractTTCPayload(frame)
			require.NotEmptyf(t, ttc, "%s[%d]", path, i)

			var server oerShape

			require.Truef(t, learnOERShape(&server, ttc), "%s[%d] ends in the server's own OER", path, i)
			assert.Equalf(t, 0, server.extraTailFields, "%s[%d]: no 20c tail fields", path, i)
			assert.Truef(t, server.fixedWidth64, "%s[%d]", path, i)
			assert.Falsef(t, server.endOfResponse, "%s[%d]: 18c ends no message with 0x1d", path, i)

			learned.learnOERTail(ttc)

			samples++
		}
	}

	require.Positive(t, samples)
	require.True(t, learned.oerShapeSnapshot().tailLearned, "the 18c replies must teach the shape")

	for name, refuse := range map[string]func(*session) (oerShape, int, byte){
		"auth refusal":    (*session).authRefusalOERShape,
		"session refusal": (*session).nextOERFrame,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			encode := func(s *session) []byte {
				shape, _, _ := refuse(s)

				return encodeOERPacket(shape, oerSummary{
					CallStatus: 1, SeqNumber: 7, ErrorCode: 1031,
					ErrorMessage: "ORA-01031: refused", CallNumber: 3,
				})
			}

			fromServer := encode(learned)
			fallback := encode(oci64_18cRefusalSession(t))

			assert.Equal(t, fromServer, fallback,
				"before any 18c OER was read, the refusal is the one the 18c server's own replies teach")

			var sent oerShape

			ttc := extractTTCPayload(fromServer[tnsHeaderSize:])
			require.True(t, learnOERShape(&sent, ttc), "dbbat's refusal must read as an OER")
			assert.Equal(t, 0, sent.extraTailFields, "the tail the 18c server writes")
			assert.True(t, sent.fixedWidth64)
			assert.False(t, sent.endOfResponse)
			assert.NotEqual(t, byte(ttcEndOfResponse), fromServer[len(fromServer)-1],
				"no end-of-response marker on the wire")
		})
	}
}
