package oracle

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Everything a **pre-v315** session decides differently, at the level of the
// individual decision rather than of a live ojdbc6 login.
//
// `TestIntegration_OJDBC6ThroughTheProxy` is the end-to-end proof, and it is
// the one that would have caught these bugs in the first place — but it needs
// Docker, an Oracle container and a 2013 jar, so it runs on CI's Oracle leg and
// nowhere else. This file is what runs on every `go test`: one case per branch,
// both sides of each, so a regression in the plumbing is a fast red rather than
// a slow one.
//
// The functions below are what "pre-v315" actually consists of. Each was a bug:
//
//	acceptUsesLegacyLength        the packet-length field's width
//	encodeDataPacketForSession    …applied to every packet dbbat frames itself
//	buildMarker                   …including the break/reset markers
//	clientSupportsVerifier18453   the O5LOGON challenge generation
//	observeBigClrChunks           the CLR long form
//	clientChallengeTrailer        the end-of-call summary's width
//	upstreamCustomHashApplies     the upstream password-key derivation
//
// See docs/oracle.md, "Pre-v315 clients".

// legacyAcceptVersion is what ojdbc6 11.2.0.4 negotiates (`01 36`), and
// modernAcceptVersion a representative v315+ one. The boundary itself —
// tnsExtendedLengthVersion — is covered exactly, below.
const (
	legacyAcceptVersion = 310
	modernAcceptVersion = 319
)

// buildAccept frames a TNS Accept announcing the given version. Only the
// version field matters to acceptUsesLegacyLength; the rest is padding, sized
// to the 32 bytes a real v310 Accept occupies end to end.
func buildAccept(t *testing.T, version uint16, legacyLength bool) []byte {
	t.Helper()

	const acceptLen = 32

	raw := make([]byte, acceptLen)
	raw[4] = byte(TNSPacketTypeAccept)

	if legacyLength {
		binary.BigEndian.PutUint16(raw[0:2], acceptLen)
	} else {
		binary.BigEndian.PutUint32(raw[0:4], acceptLen)
	}

	binary.BigEndian.PutUint16(raw[acceptTNSVersionOffset:acceptTNSVersionOffset+2], version)

	return raw
}

// TestAcceptUsesLegacyLength_RealAccepts reads the verdict off the Accepts in
// the corpus rather than off synthetic bytes: the one client that must answer
// "legacy" is the one whose recording exists precisely because it is different.
func TestAcceptUsesLegacyLength_RealAccepts(t *testing.T) {
	t.Parallel()

	for name, wantLegacy := range map[string]bool{
		ojdbc6LegacyDump:                 true,
		"go_ora_dml.pcapng":              false,
		"jdbc_thin_cursor_reexec.pcapng": false,
		"python_thin.pcapng":             false,
		"sqlplus_cursor_reexec.pcapng":   false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw := firstAcceptPacket(t, name)

			legacy, ok := acceptUsesLegacyLength(raw)
			require.True(t, ok, "a recorded Accept must be readable")
			assert.Equal(t, wantLegacy, legacy)

			version := binary.BigEndian.Uint16(raw[acceptTNSVersionOffset : acceptTNSVersionOffset+2])
			assert.Equal(t, wantLegacy, version < tnsExtendedLengthVersion,
				"the verdict is the version and nothing else (this one negotiated %d)", version)

			// And the version field is the *only* thing that can decide it. The
			// Accept is the packet that establishes the version, so it is itself
			// framed the legacy way in both kinds of session — measured here
			// across every recorded client, modern ones included. Only the
			// packets *after* it follow what it announced, which is why
			// acceptUsesLegacyLength reads [8:10] and not [0:2].
			assert.NotZero(t, binary.BigEndian.Uint16(raw[0:2]),
				"every Accept carries its own length in the legacy field, whatever it announces")
		})
	}
}

// TestAcceptUsesLegacyLength_Boundary pins 315 exactly. Off by one here means
// every 12.1 client is framed for a 11.2 one, or the reverse.
func TestAcceptUsesLegacyLength_Boundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version    uint16
		wantLegacy bool
	}{
		{1, true},
		{legacyAcceptVersion, true},
		{313, true},
		{tnsExtendedLengthVersion - 1, true},
		{tnsExtendedLengthVersion, false},
		{modernAcceptVersion, false},
		{65535, false},
	} {
		legacy, ok := acceptUsesLegacyLength(buildAccept(t, tc.version, tc.wantLegacy))
		require.Truef(t, ok, "version %d", tc.version)
		assert.Equalf(t, tc.wantLegacy, legacy, "version %d", tc.version)
	}
}

// TestAcceptUsesLegacyLength_Rejects covers everything that is not a readable
// Accept. All of it must report ok=false, which leaves the session on the v315+
// form — what every client had before the distinction existed.
func TestAcceptUsesLegacyLength_Rejects(t *testing.T) {
	t.Parallel()

	data := buildAccept(t, legacyAcceptVersion, true)
	data[4] = byte(TNSPacketTypeData)

	for name, raw := range map[string][]byte{
		"nil":               nil,
		"empty":             {},
		"header only":       make([]byte, tnsHeaderSize),
		"one byte short":    make([]byte, acceptTNSVersionOffset+1),
		"not an Accept":     data,
		"a Data packet":     {0x00, 0x00, 0x00, 0x0a, byte(TNSPacketTypeData), 0x00, 0x00, 0x00, 0x00, 0x00},
		"a Refuse packet":   {0x00, 0x20, 0x00, 0x00, byte(TNSPacketTypeRefuse), 0x00, 0x00, 0x00, 0x01, 0x36},
		"a Redirect packet": {0x00, 0x20, 0x00, 0x00, byte(TNSPacketTypeRedirect), 0x00, 0x00, 0x00, 0x01, 0x36},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			legacy, ok := acceptUsesLegacyLength(raw)
			assert.False(t, ok, "an unreadable Accept must decide nothing")
			assert.False(t, legacy)
		})
	}
}

// TestEncodeDataPacketForSession_BothForms is the fix itself, in miniature: the
// same payload, framed for each kind of peer, read back by the same reader both
// peers use.
//
// The legacy assertion is the one that matters. A v310 client reads the length
// out of [0:2]; the v315+ form leaves those two bytes zero, so it sees a
// zero-length packet and refuses it before a single TTC byte is looked at
// (`Invalid Packet Lenght`).
func TestEncodeDataPacketForSession_BothForms(t *testing.T) {
	t.Parallel()

	payload := []byte{0x00, 0x00, byte(TTCFuncResponse), 0x01, 0x02}
	total := tnsHeaderSize + len(payload)

	t.Run("legacy", func(t *testing.T) {
		t.Parallel()

		raw := encodeDataPacketForSession(payload, true)

		require.Len(t, raw, total)
		assert.Equal(t, uint16(total), binary.BigEndian.Uint16(raw[0:2]),
			"the length lives at [0:2] — the only place a v310 peer looks")
		assert.Equal(t, byte(TNSPacketTypeData), raw[4])
		assert.Equal(t, payload, raw[tnsHeaderSize:])

		assertReadsBackAsData(t, raw, payload)
	})

	t.Run("v315+", func(t *testing.T) {
		t.Parallel()

		raw := encodeDataPacketForSession(payload, false)

		require.Len(t, raw, total)
		assert.Zero(t, binary.BigEndian.Uint16(raw[0:2]),
			"the legacy field reads zero, which is what says to look at [0:4]")
		assert.Equal(t, uint32(total), binary.BigEndian.Uint32(raw[0:4]))
		assert.Equal(t, byte(TNSPacketTypeData), raw[4])
		assert.Equal(t, payload, raw[tnsHeaderSize:])

		assertReadsBackAsData(t, raw, payload)

		// The v315+ branch is the pre-existing encoder, unchanged.
		assert.Equal(t, encodeV315DataPacket(payload), raw)
	})
}

// TestSessionEncodesInItsNegotiatedForm is the same claim one level up: the
// session method dispatches on the flag the Accept set, so every caller — the
// O5LOGON challenge, the upstream AUTH writes, the merged AUTH OK — follows it
// without having to know the rule.
func TestSessionEncodesInItsNegotiatedForm(t *testing.T) {
	t.Parallel()

	payload := []byte{0x00, 0x00, 0x08, 0x01}

	s := newTestSession(nil)
	assert.False(t, s.tnsLegacyLength, "a session decides v315+ until an Accept says otherwise")
	assert.Equal(t, encodeDataPacketForSession(payload, false), s.encodeSessionDataPacket(payload))

	s.tnsLegacyLength = true
	assert.Equal(t, encodeDataPacketForSession(payload, true), s.encodeSessionDataPacket(payload))
}

// TestBuildMarker_BothForms covers the break/reset markers, which are the one
// thing dbbat writes that is neither a Data packet nor built from a payload —
// and which used to be a hard-coded 11-byte literal in the v315+ form only.
func TestBuildMarker_BothForms(t *testing.T) {
	t.Parallel()

	const markerLen = 11

	for name, tc := range map[string]struct {
		marker func(bool) []byte
		want   byte
		isKind func(*TNSPacket) bool
	}{
		"reset": {buildResetMarker, markerTypeReset, isResetMarker},
		"break": {buildBreakMarker, markerTypeBreak, isBreakMarker},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			legacy := tc.marker(true)
			modern := tc.marker(false)

			require.Len(t, legacy, markerLen)
			require.Len(t, modern, markerLen)

			assert.Equal(t, uint16(markerLen), binary.BigEndian.Uint16(legacy[0:2]))
			assert.Zero(t, binary.BigEndian.Uint16(modern[0:2]))
			assert.Equal(t, uint32(markerLen), binary.BigEndian.Uint32(modern[0:4]))

			// The v315+ form is byte-for-byte the literal that was there before
			// the two forms existed, which is what makes this change a no-op for
			// every client that already worked.
			assert.Equal(t,
				[]byte{0x00, 0x00, 0x00, 0x0B, 0x0C, 0x00, 0x00, 0x00, 0x01, 0x00, tc.want},
				modern)

			// Everything past the length field is identical, and both still read
			// back as the marker they are.
			assert.Equal(t, legacy[4:], modern[4:])

			for _, raw := range [][]byte{legacy, modern} {
				pkt := readPacketFromBytes(t, raw)
				assert.Equal(t, TNSPacketTypeControl, pkt.Type)
				assert.True(t, tc.isKind(pkt), "the marker must still be recognized as a %s", name)
			}
		})
	}
}

// TestClientSupportsVerifier18453 is the verifier half: the server's customHash
// bit is necessary and, on its own, was wrongly taken as sufficient. 23ai
// advertises it to everyone, so a pre-v315 client was handed a challenge four
// years its junior and closed the socket rather than answer it.
func TestClientSupportsVerifier18453(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		customHash bool
		legacy     bool
		want       bool
	}{
		"modern client, server offers customHash": {true, false, true},
		"modern client, server does not":          {false, false, false},
		"pre-v315 client, server offers it":       {true, true, false},
		"pre-v315 client, server does not":        {false, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(nil)
			s.upstreamCustomHash = tc.customHash
			s.tnsLegacyLength = tc.legacy

			assert.Equal(t, tc.want, s.clientSupportsVerifier18453())
		})
	}
}

// TestObserveBigClrChunks_SkipsPreV315 is the same shape a third time, on the
// capability that decides how a long CLR value is framed. The reply below is a
// real one, with the bit genuinely set, so the pre-v315 case is the code
// declining it rather than the fixture failing to offer it.
func TestObserveBigClrChunks_SkipsPreV315(t *testing.T) {
	t.Parallel()

	raw, _ := firstSetProtocolReply(t, "go_ora_dml.pcapng")
	require.True(t, observeBigClrChunksFlag(raw), "the fixture must actually advertise the capability")

	t.Run("a modern session records it", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(nil)
		s.observeBigClrChunks(raw)

		assert.True(t, s.clientBigClrChunks)
	})

	t.Run("a pre-v315 session does not", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(nil)
		s.tnsLegacyLength = true
		s.observeBigClrChunks(raw)

		assert.False(t, s.clientBigClrChunks,
			"ojdbc6 writes a 96-byte value as fe 40 <64> 20 <32> 00 — single-byte chunk "+
				"lengths — and reading those as compressed ints walks off the end of it")
	})

	t.Run("a reply that advertises nothing records nothing", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(nil)
		s.observeBigClrChunks(buildSetProtocolReply(t, []byte{0x06, 0x01, 0x01, 0x01, 0xef}))

		assert.False(t, s.clientBigClrChunks)
	})
}

// TestClientChallengeTrailer_BorrowsTheUpstreamSummary covers the one branch
// that is not a refusal but a substitution: which sessions reuse the live
// upstream's end-of-call summary instead of a hand-built capture.
//
// ojdbc6 parses 30 bytes of tail where buildAuthChallengeEndMarker writes 32,
// leaves the surplus in its TTC read buffer, and reads the first stale zero as
// the *next* call's message code.
func TestClientChallengeTrailer_BorrowsTheUpstreamSummary(t *testing.T) {
	t.Parallel()

	// A summary of a width no hand-built capture has, so "borrowed" and "built"
	// can never be confused for one another.
	borrowed := append([]byte{byte(TTCFuncOERR)}, make([]byte, 30)...)

	withResp := func(trailer []byte) *upstreamAuthResponse {
		return &upstreamAuthResponse{challengeTrailer: trailer}
	}

	for name, tc := range map[string]struct {
		wide       bool
		legacy     bool
		resp       *upstreamAuthResponse
		wantBorrow bool
	}{
		"OCI, upstream challenge captured":      {true, false, withResp(borrowed), true},
		"pre-v315, upstream challenge captured": {false, true, withResp(borrowed), true},
		"modern thin keeps the built summary":   {false, false, withResp(borrowed), false},
		"pre-v315 with no upstream challenge":   {false, true, nil, false},
		"pre-v315 with an empty trailer":        {false, true, withResp(nil), false},
		"pre-v315 with a trailer that is not an OER": {
			false, true, withResp([]byte{0x08, 0x01}), false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(nil)
			s.clientWideEncoding = tc.wide
			s.tnsLegacyLength = tc.legacy
			s.upstreamAuthResp = tc.resp

			got := s.clientChallengeTrailer(VerifierType6949)

			if tc.wantBorrow {
				assert.Equal(t, borrowed, got, "the real server sized this for the caps this client negotiated")

				return
			}

			assert.Equal(t, buildAuthChallengeEndMarker(VerifierType6949, tc.wide), got,
				"without a usable upstream summary the hand-built one still stands")
		})
	}
}

// TestUpstreamCustomHashApplies is the last of the four: the *upstream* leg's
// password-key derivation, which took the capability bit's word for it and ran
// PBKDF2 over an empty AUTH_PBKDF2_CSK_SALT. The upstream answered ORA-01017 for
// a password that was perfectly correct.
//
// The gate can only ever turn customHash off, and only when the material it
// needs is absent — which is what the "server offers it and sent the salt" case
// is here to keep true.
func TestUpstreamCustomHashApplies(t *testing.T) {
	t.Parallel()

	const chkSalt = "0123456789ABCDEF"

	for name, tc := range map[string]struct {
		customHash bool
		resp       *upstreamAuthResponse
		want       bool
	}{
		"server offers it and sent the salt": {true, &upstreamAuthResponse{pbkdf2ChkSalt: chkSalt}, true},
		"server offers it but sent no salt":  {true, &upstreamAuthResponse{}, false},
		"server does not offer it":           {false, &upstreamAuthResponse{pbkdf2ChkSalt: chkSalt}, false},
		"no challenge at all":                {true, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(nil)
			s.upstreamCustomHash = tc.customHash

			assert.Equal(t, tc.want, s.upstreamCustomHashApplies(tc.resp))
		})
	}
}

// TestReframeAuthOK_FollowsTheSessionsLengthForm pins the length form through
// the AUTH OK re-fragmentation, the one place a *multi*-packet client message is
// rebuilt. upstream_auth_client_test.go covers the fragmentation arithmetic; this
// covers the envelope each fragment goes out in.
func TestReframeAuthOK_FollowsTheSessionsLengthForm(t *testing.T) {
	t.Parallel()

	dataFlags := []byte{0x00, 0x00}
	fragLens := []int{4, 6}
	ttc := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	for name, legacy := range map[string]bool{"legacy": true, "v315+": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			merged := encodeDataPacketForSession(append(append([]byte{}, dataFlags...), ttc...), legacy)

			out := reframeAuthOK(merged, dataFlags, fragLens, legacy)
			require.NotEqual(t, merged, out, "a two-fragment AUTH OK must be re-cut")

			// Walk the result as the client would: two packets, each declaring
			// its length in the session's own field.
			pos := 0

			for _, n := range fragLens {
				want := tnsHeaderSize + ttcDataFlagsSize + n

				require.LessOrEqual(t, pos+want, len(out))

				frag := out[pos : pos+want]

				if legacy {
					assert.Equal(t, uint16(want), binary.BigEndian.Uint16(frag[0:2]))
				} else {
					assert.Zero(t, binary.BigEndian.Uint16(frag[0:2]))
					assert.Equal(t, uint32(want), binary.BigEndian.Uint32(frag[0:4]))
				}

				assert.Equal(t, byte(TNSPacketTypeData), frag[4])

				pos += want
			}

			assert.Equal(t, len(out), pos, "the fragments must account for every byte")
		})
	}
}

// TestEncodeOERPacket_FollowsTheShapesLengthForm covers refusals — the AUTH
// reject and every mid-session one — which are framed from the shape rather than
// from the session directly.
func TestEncodeOERPacket_FollowsTheShapesLengthForm(t *testing.T) {
	t.Parallel()

	sum := oerSummary{CallStatus: 1, SeqNumber: 1, ErrorCode: 1017, ErrorMessage: "ORA-01017: nope"}

	modern := encodeOERPacket(oerShape{}, sum)
	legacy := encodeOERPacket(oerShape{legacyLength: true}, sum)

	require.Len(t, legacy, len(modern), "only the header's length field differs")
	assert.Equal(t, modern[4:], legacy[4:], "the OER body is the same either way")

	assert.Zero(t, binary.BigEndian.Uint16(modern[0:2]))
	assert.Equal(t, uint32(len(modern)), binary.BigEndian.Uint32(modern[0:4]))
	assert.Equal(t, uint16(len(legacy)), binary.BigEndian.Uint16(legacy[0:2]))

	// Both read back as one complete Data packet, which is the whole point: a
	// refusal the client cannot frame is a hang, not an error message.
	for _, raw := range [][]byte{modern, legacy} {
		pkt := readPacketFromBytes(t, raw)
		assert.Equal(t, TNSPacketTypeData, pkt.Type)
		assert.Len(t, pkt.Raw, len(raw))
	}
}

// TestNextOERFrame_StampsTheSessionsLengthForm is the wiring between the two:
// the shape every refusal is built from carries the session's envelope, and
// carries it whether or not an upstream OER has taught the session its body
// layout — the length form is the TNS envelope, not something an OER could
// teach.
func TestNextOERFrame_StampsTheSessionsLengthForm(t *testing.T) {
	t.Parallel()

	for name, legacy := range map[string]bool{"legacy": true, "v315+": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, learned := range []bool{false, true} {
				s := newTestSession(nil)
				s.tnsLegacyLength = legacy
				s.oer = oerShape{tailLearned: learned}

				shape, _, _ := s.nextOERFrame()

				assert.Equalf(t, legacy, shape.legacyLength,
					"tailLearned=%v must not change the envelope", learned)
			}
		})
	}
}

// firstAcceptPacket returns the raw bytes of the first TNS Accept in a capture.
func firstAcceptPacket(t *testing.T, dumpName string) []byte {
	t.Helper()

	td := loadTestDump(t, dumpName)

	for i := range td.Packets {
		pkt, err := parseTNSFromDumpPacket(td.Packets[i].Data)
		if err != nil {
			continue
		}

		if pkt.Type == TNSPacketTypeAccept {
			return pkt.Raw
		}
	}

	t.Fatalf("%s contains no Accept packet", dumpName)

	return nil
}

// readPacketFromBytes runs raw back through readTNSPacket — the reader both
// peers use — so a framing assertion is about what a peer sees rather than about
// what the encoder meant.
func readPacketFromBytes(t *testing.T, raw []byte) *TNSPacket {
	t.Helper()

	client, server := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	go func() { _, _ = client.Write(raw) }()

	pkt, err := readTNSPacket(server)
	require.NoError(t, err)

	return pkt
}

// assertReadsBackAsData is readPacketFromBytes plus the payload check both
// encoder branches owe.
func assertReadsBackAsData(t *testing.T, raw, payload []byte) {
	t.Helper()

	pkt := readPacketFromBytes(t, raw)

	assert.Equal(t, TNSPacketTypeData, pkt.Type)
	assert.Equal(t, payload, pkt.Payload)
	assert.Len(t, pkt.Raw, len(raw), "the reader must consume exactly the bytes the encoder wrote")
}
