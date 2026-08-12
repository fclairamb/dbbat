package oracle

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixtures below are the AUTH packets a macOS Instant Client 23.3 (sqlplus
// 23.3.0.23.09) put on the wire against Oracle 23ai Free, read straight out of
// testdata/sqlplus_cursor_reexec.pcapng. They are what the wide synthetic
// builders were measured against, so the tests read them from the capture
// rather than restating them — a re-captured fixture cannot silently drift away
// from what the builders assume.
//
// Capture facts these tests pin (the five deltas from the thin body):
//
//	Phase 1  data flags 20 00  prefix 03 76 02 01 03  mode 0x00000001  5 pairs
//	Phase 2  data flags 20 00  prefix 03 73 03 01 04  mode 0x40000101  21 pairs
//
// both buffer-sized (user-len 0x12 = 3*len("system")) and neither pair-count
// padded.

// captureAuthPacket returns the first AUTH packet of one phase from a capture.
func captureAuthPacket(t *testing.T, dumpName string, sub byte) *TNSPacket {
	t.Helper()

	td := loadTestDump(t, dumpName)

	for i := range td.Packets {
		pkt, err := parseTNSFromDumpPacket(td.Packets[i].Data)
		if err != nil || pkt.Type != TNSPacketTypeData ||
			len(pkt.Payload) < ttcDataFlagsSize+2 {
			continue
		}

		if body := pkt.Payload[ttcDataFlagsSize:]; body[0] == byte(TTCFuncPiggyback) && body[1] == sub {
			return pkt
		}
	}

	t.Fatalf("%s contains no AUTH packet with sub %#x", dumpName, sub)

	return nil
}

// TestDetectWideAuthFraming_SqlplusCapture reads the framing conventions off
// the two real sqlplus AUTH packets. Everything asserted here is a value dbbat
// cannot derive and must borrow, so a change in what detectWideAuthFraming
// concludes is a change in what the synthetic body puts on the wire.
func TestDetectWideAuthFraming_SqlplusCapture(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		sub           byte
		wantPrefix    []byte
		wantMode      uint32
		wantPairCount uint32
	}{
		// Delta 5: Phase 2's mode carries the client-specific 0x40000000 high
		// bit on top of logonModeNoNewPass|logonModeUserAndPass (0x101). It is
		// also why Phase 2 must be built from Phase 2's own captured packet:
		// Phase 1 declared a bare 0x1 and a different lead byte pair.
		"phase 1": {PiggybackSubAuth1, []byte{0x03, 0x76, 0x02, 0x01, 0x03}, 0x00000001, 5},
		"phase 2": {PiggybackSubAuth2, []byte{0x03, 0x73, 0x03, 0x01, 0x04}, 0x40000101, 21},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pkt := captureAuthPacket(t, "sqlplus_cursor_reexec.pcapng", tc.sub)

			// Delta 1: OCI puts 0x2000 in the TNS data flags, thin clients 0x0000.
			assert.Equal(t, wideAuthDataFlags, pkt.Payload[:ttcDataFlagsSize],
				"captured OCI AUTH data flags must be what the synthetic path sends")

			body := pkt.Payload[ttcDataFlagsSize:]

			f, ok := detectWideAuthFraming(body)
			require.True(t, ok, "a real OCI AUTH body must parse as a wide preamble")

			assert.Equal(t, tc.wantPrefix, f.prefix)
			assert.Equal(t, tc.wantMode, f.mode)

			// Delta 3: the Instant Client's 4-byte length fields are 3x UTF-8
			// max-expansion buffer sizes, not plain byte lengths.
			assert.True(t, f.bufferSized, "instantclient flavor is buffer-sized")
			assert.False(t, f.pairCountPad, "only the DB-bundled client pads the pair count")

			anchor := len(f.prefix)
			assert.Equal(t, uint32(3*len("system")),
				binary.LittleEndian.Uint32(body[anchor+8:anchor+12]),
				"user-len field is the 3x buffer size of the captured login")
			assert.Equal(t, tc.wantPairCount,
				binary.LittleEndian.Uint32(body[anchor+24:anchor+28]))
		})
	}
}

// TestBuildWideAuthBody_PreambleMatchesCapture is the strongest statement these
// tests make: given the framing read off sqlplus's own packet, the same
// username and the same number of pairs, buildWideAuthBody reproduces sqlplus's
// preamble byte for byte — pointer runs, buffer-sized user length, logon mode,
// pair count and CLR username included. Delta 2 (pointer runs and 4-byte
// little-endian fields rather than compressed integers) is what this pins.
func TestBuildWideAuthBody_PreambleMatchesCapture(t *testing.T) {
	t.Parallel()

	for name, sub := range map[string]byte{
		"phase 1": PiggybackSubAuth1,
		"phase 2": PiggybackSubAuth2,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := captureAuthPacket(t, "sqlplus_cursor_reexec.pcapng", sub).
				Payload[ttcDataFlagsSize:]

			f, ok := detectWideAuthFraming(body)
			require.True(t, ok)

			anchor := len(f.prefix)
			pairCount := int(binary.LittleEndian.Uint32(body[anchor+24 : anchor+28]))

			// Only the pair COUNT reaches the preamble, so placeholder pairs are
			// enough to reproduce it; the pairs themselves are covered below.
			got := buildWideAuthBody(f, "system", make([]authKV, pairCount), false)

			// prefix + ptr + userLen + mode + ptr + pairCount + ptr + ptr + CLR user.
			preambleLen := anchor + 8 + 4 + 4 + 8 + 4 + 8 + 8 + 1 + len("system")

			require.GreaterOrEqual(t, len(got), preambleLen)
			assert.Equal(t, body[:preambleLen], got[:preambleLen],
				"synthetic preamble must be byte-identical to the captured sqlplus preamble")
		})
	}
}

// TestWideAuthFramingFor_UsesTheClientPacket covers the borrow-and-OR rule: the
// framing comes from the client's own packet for that same phase, while the
// caller's mode bits are OR-ed in. A Phase 2 that lost logonModeUserAndPass
// offers the upstream no password at all, so the OR is load-bearing.
func TestWideAuthFramingFor_UsesTheClientPacket(t *testing.T) {
	t.Parallel()

	phase2 := captureAuthPacket(t, "sqlplus_cursor_reexec.pcapng", PiggybackSubAuth2)

	s := &session{}
	f := s.wideAuthFramingFor(phase2, PiggybackSubAuth2, authPhase2FuncSeq,
		logonModeNoNewPass|logonModeUserAndPass)

	assert.Equal(t, []byte{0x03, 0x73, 0x03, 0x01, 0x04}, f.prefix,
		"the client's own lead bytes and sequence are copied verbatim")
	assert.Equal(t, uint32(0x40000101), f.mode,
		"client high bits are preserved and the caller's mode is OR-ed in")
	assert.True(t, f.bufferSized)
}

// TestWideAuthFramingFor_FallsBackToInstantClient covers the two ways the
// client packet can fail to inform the framing — absent entirely (the legacy
// non-relay path) and present but unparseable. Both must land on the Instant
// Client shape, the flavor in testdata and the more widely deployed one.
func TestWideAuthFramingFor_FallsBackToInstantClient(t *testing.T) {
	t.Parallel()

	for name, pkt := range map[string]*TNSPacket{
		"no client packet": nil,
		"empty payload":    {Payload: []byte{0x00, 0x00}},
		"thin client body": {Payload: append([]byte{0x00, 0x00}, thinPhase1WideProbeBody()...)},
		"truncated preamble": {Payload: append([]byte{0x20, 0x00},
			append([]byte{0x03, 0x76, 0x02, 0x01, 0x03}, wideAuthPointerRun...)...)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := &session{}
			f := s.wideAuthFramingFor(pkt, PiggybackSubAuth1, authPhase1FuncSeq, logonModeNoNewPass)

			assert.Equal(t, []byte{byte(TTCFuncPiggyback), PiggybackSubAuth1, authPhase1FuncSeq, 0x01, 0x03},
				f.prefix)
			assert.Equal(t, uint32(logonModeNoNewPass), f.mode)
			assert.True(t, f.bufferSized, "the fallback is the Instant Client convention")
			assert.False(t, f.pairCountPad)
		})
	}
}

// thinPhase1WideProbeBody is a compressed (thin) AUTH Phase 1 body, used to
// confirm the wide detector rejects a body it must not claim.
func thinPhase1WideProbeBody() []byte {
	body := make([]byte, 0, 64)
	body = append(body, byte(TTCFuncPiggyback), PiggybackSubAuth1, 0x01, 0x01, 0x01, 0x05, 0x01, 0x01)
	body = append(body, ttcClr([]byte("system"))...)
	body = append(body, ttcKeyVal("AUTH_TERMINAL", "", 0)...)

	return body
}

// TestDetectWideAuthFraming_Rejects pins what must NOT be read as an OCI
// preamble. Claiming one of these would hand a wide body to a thin upstream,
// which is the mirror image of the bug this whole path exists to fix.
func TestDetectWideAuthFraming_Rejects(t *testing.T) {
	t.Parallel()

	farPointer := append([]byte{byte(TTCFuncPiggyback), PiggybackSubAuth1, 0x01},
		bytes.Repeat([]byte{0xaa}, maxWideAuthLead+1)...)
	farPointer = append(farPointer, wideAuthPointerRun...)
	farPointer = append(farPointer, make([]byte, 32)...)

	for name, body := range map[string][]byte{
		"nil":                 nil,
		"too short":           {0x03, 0x76},
		"not a piggyback":     append([]byte{0x11, 0x76, 0x01}, wideAuthPointerRun...),
		"no pointer run":      thinPhase1WideProbeBody(),
		"pointer run too far": farPointer,
		"truncated after anchor": append([]byte{0x03, 0x76, 0x02, 0x01, 0x03},
			wideAuthPointerRun...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, ok := detectWideAuthFraming(body)
			assert.False(t, ok)
		})
	}
}

// TestDetectWideAuthFraming_BundledFlavor covers the other OCI flavor: the
// DB-bundled 23.26 client sends PLAIN byte lengths and an extra zero dword
// after the pair count. Mixing its convention with the Instant Client's draws
// ORA-28041, so the two must be told apart from the packet alone.
func TestDetectWideAuthFraming_BundledFlavor(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		body            []byte
		wantBufferSized bool
		wantPad         bool
		wantPrefixLen   int
	}{
		"instantclient": {instantclientPhase1Body("system"), true, false, 5},
		"bundled":       {bundledPhase1Body("system"), false, true, 11},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f, ok := detectWideAuthFraming(tc.body)
			require.True(t, ok)

			assert.Equal(t, tc.wantBufferSized, f.bufferSized)
			assert.Equal(t, tc.wantPad, f.pairCountPad)
			assert.Len(t, f.prefix, tc.wantPrefixLen)

			// A body built back in the detected flavor must round-trip through
			// the same detector, which is what keeps the two apart end to end.
			rebuilt := buildWideAuthBody(f, "system", authPhase1Pairs(driverIdentity{}), false)

			again, ok := detectWideAuthFraming(rebuilt)
			require.True(t, ok)
			assert.Equal(t, tc.wantBufferSized, again.bufferSized)
			assert.Equal(t, tc.wantPad, again.pairCountPad)
		})
	}
}

// TestTtcKeyValWideSized_EmptyValue is delta 4. OCI writes an empty value as a
// zero length field followed straight by the flag, with no CLR byte at all —
// captured verbatim as AUTH_TERMINAL in sqlplus's Phase 1. ttcKeyValWide's
// unconditional CLR byte is one the reader then eats as part of the flag, which
// desynchronises every pair after it.
func TestTtcKeyValWideSized_EmptyValue(t *testing.T) {
	t.Parallel()

	got := ttcKeyValWideSized("AUTH_TERMINAL", "", 0, true, false)

	// 27 00 00 00 | 0d "AUTH_TERMINAL" | 00 00 00 00 | 00 00 00 00
	want := make([]byte, 0, 26)
	want = append(want, 0x27, 0x00, 0x00, 0x00, 0x0d)
	want = append(want, []byte("AUTH_TERMINAL")...)
	want = append(want, make([]byte, 8)...)

	assert.Equal(t, want, got, "empty value must carry no CLR byte")

	// The bug this guards: ttcKeyValWide writes a 0x00 CLR that readAuthKVPairWide
	// never consumes, so it is exactly one byte longer and misreads the flag.
	assert.Len(t, ttcKeyValWide("AUTH_TERMINAL", "", 0), len(got)+1,
		"ttcKeyValWide is the encoding that must NOT be used for empty values")

	kv, ok := readAuthKVPairWide(got, false)
	require.True(t, ok)
	assert.Equal(t, "AUTH_TERMINAL", string(kv.Key))
	assert.Empty(t, kv.Value)
	assert.Equal(t, 0, kv.Flag)
	assert.Len(t, got, kv.Consumed, "the reader must consume the pair exactly")
}

// TestWideAuthBody_ParsesBackThroughTheReaders walks a synthetic Phase 1 body
// back out through the readers the upstream's parser mirrors. The empty
// AUTH_TERMINAL sits first, so a stray CLR byte would desynchronise everything
// behind it — this is the test that would have caught delta 4.
func TestWideAuthBody_ParsesBackThroughTheReaders(t *testing.T) {
	t.Parallel()

	ident := driverIdentity{
		HostName:    "", // empty on purpose: drives the no-CLR empty-value framing
		ProgramName: "dbbat for sqlplus",
		PID:         1958,
		OSUser:      "florent",
		DriverName:  "dbbat",
	}

	for name, bufferSized := range map[string]bool{"instantclient": true, "bundled": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := defaultWideAuthFraming(PiggybackSubAuth1, authPhase1FuncSeq, logonModeNoNewPass)
			f.bufferSized = bufferSized

			pairs := authPhase1Pairs(ident)
			body := buildClientAuthPhase1Wide(f, "system", ident, false)

			require.True(t, payloadUsesWideKVEncoding(body),
				"a wide body must be recognized as wide, or the upstream reads it as thin")

			// Walk to the KV region and read every pair back.
			pos := len(f.prefix) + 28 + 16
			pos += 1 + int(body[pos]) // CLR username

			for i, want := range pairs {
				kv, ok := readAuthKVPairWide(body[pos:], false)
				require.Truef(t, ok, "pair %d (%s) unreadable", i, want.key)
				assert.Equal(t, want.key, string(kv.Key))
				assert.Equal(t, want.value, string(kv.Value))
				assert.Equal(t, want.flag, kv.Flag)

				pos += kv.Consumed
			}

			assert.Len(t, body, pos, "the pairs must account for the whole body")

			// The username locators the rewrite path uses must agree with what
			// the builder wrote, including the buffer-sized user-len field.
			start, end, ok := locateAnchoredUsername(body)
			require.True(t, ok)
			assert.Equal(t, "system", string(body[start:end]))

			lenPos, newVal := findUserIDLenPos(body, start, len("system"), len("SYSTEM"))
			require.NotEqual(t, -1, lenPos, "the user-len field must be locatable")
			assert.Equal(t, len(f.prefix)+8, lenPos, "it sits right after the first pointer run")

			if bufferSized {
				assert.Equal(t, byte(3*len("SYSTEM")), newVal)
			} else {
				assert.Equal(t, byte(len("SYSTEM")), newVal)
			}
		})
	}
}

// TestWideAuthPhase2_ParsesBackThroughParseAuthPhase2 checks the credentials
// survive the wide encoding through the same entry point dbbat uses to read a
// client's Phase 2 — the one function that has to find AUTH_SESSKEY and
// AUTH_PASSWORD in either dialect.
func TestWideAuthPhase2_ParsesBackThroughParseAuthPhase2(t *testing.T) {
	t.Parallel()

	sec := &upstreamAuthSecrets{
		encClientSessKey: "1E92DE1CF308A2D4154A07A9B038950B775276B11CF141E5FC23116D98AF3AA8",
		encPassword:      "FF5911DEB7CAF42F624F5DD90D1E4C2A",
		eSpeedyKey:       "098B86786ED67A4352661DB2",
	}

	ident := driverIdentity{ProgramName: "dbbat", OSUser: "florent", PID: 1, DriverName: "dbbat"}

	f := defaultWideAuthFraming(PiggybackSubAuth2, authPhase2FuncSeq,
		logonModeNoNewPass|logonModeUserAndPass)

	body := buildClientAuthPhase2Wide(f, "system", ident, sec, false)

	payload := append(append([]byte(nil), wideAuthDataFlags...), body...)

	gotKey, gotPwd, err := parseAuthPhase2(payload)
	require.NoError(t, err)
	assert.Equal(t, sec.encClientSessKey, gotKey)
	assert.Equal(t, sec.encPassword, gotPwd)

	// The OCI-only keys and the OCI library type are what make this a wide body
	// rather than a thin one wearing wide framing.
	assert.Equal(t, "2", findKVByKeyBytesWide(body, []byte("SESSION_CLIENT_LIB_TYPE")))

	for _, key := range []string{"AUTH_RTT", "AUTH_CLNT_MEM", "AUTH_ACL", "AUTH_FLAGS"} {
		assert.NotEmpty(t, findKVByKeyBytesWide(body, []byte(key)), "missing OCI key %s", key)
	}

	assert.Equal(t, sec.eSpeedyKey, findKVByKeyBytesWide(body, []byte("AUTH_PBKDF2_SPEEDY_KEY")))
}

// TestSyntheticAuthDispatchesOnClientEncoding is the dispatch guard. With no
// client packet to rewrite, the synthetic path runs; which body it builds must
// follow clientWideEncoding, and the TNS data flags must follow with it. A wide
// body under 0x0000 flags (or a thin body under 0x2000) is the failure this
// whole change is about, and it is invisible to any test that only inspects the
// KV pairs.
func TestSyntheticAuthDispatchesOnClientEncoding(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		wide      bool
		wantFlags []byte
	}{
		"thin client": {false, thinAuthDataFlags},
		"wide client": {true, wideAuthDataFlags},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ident := driverIdentity{ProgramName: "dbbat", OSUser: "florent", PID: 7, DriverName: "dbbat"}
			sec := &upstreamAuthSecrets{encClientSessKey: "AABB", encPassword: "CCDD"}

			for phase, send := range map[string]func(*session) error{
				"phase 1": func(s *session) error {
					return s.sendUpstreamAuthPhase1("system", ident, logonModeNoNewPass)
				},
				"phase 2": func(s *session) error {
					return s.sendUpstreamAuthPhase2("system", ident, sec, logonModeNoNewPass|logonModeUserAndPass)
				},
			} {
				t.Run(phase, func(t *testing.T) {
					t.Parallel()

					pkt := sendSyntheticAuth(t, tc.wide, send)

					assert.Equal(t, tc.wantFlags, pkt.Payload[:ttcDataFlagsSize],
						"data flags must follow the client's dialect")

					body := pkt.Payload[ttcDataFlagsSize:]
					assert.Equal(t, tc.wide, payloadUsesWideKVEncoding(body),
						"body encoding must follow the client's dialect")

					// Either way the upstream must still find the login name.
					// parseAuthPhase1 normalises to upper case as Oracle does;
					// the body itself carries the name as written.
					gotUser, err := parseAuthPhase1(pkt.Payload)
					require.NoError(t, err)
					assert.Equal(t, "SYSTEM", gotUser)
					assert.Contains(t, string(body), "system")
				})
			}
		})
	}
}

// TestSyntheticWideAuthDeclaresTheCallersLogonMode pins that the mode the
// caller asks for is the mode that reaches the wire, at the offset the upstream
// reads it from. Phase 1 and Phase 2 differ only in this field and the lead
// bytes, and a Phase 2 that lost logonModeUserAndPass authenticates against no
// password at all — the failure would look like a wrong credential, not a
// framing bug, so it is worth pinning by offset rather than by round-trip.
func TestSyntheticWideAuthDeclaresTheCallersLogonMode(t *testing.T) {
	t.Parallel()

	ident := driverIdentity{ProgramName: "dbbat", OSUser: "florent", PID: 7, DriverName: "dbbat"}

	for name, mode := range map[string]uint32{
		"phase 1 mode":            logonModeNoNewPass,
		"phase 2 mode":            logonModeNoNewPass | logonModeUserAndPass,
		"mode with OCI high bits": logonModeNoNewPass | logonModeUserAndPass | 0x40000000,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pkt := sendSyntheticAuth(t, true, func(s *session) error {
				return s.sendUpstreamAuthPhase1("system", ident, mode)
			})

			body := pkt.Payload[ttcDataFlagsSize:]

			f, ok := detectWideAuthFraming(body)
			require.True(t, ok, "the synthetic body must be readable as a wide preamble")

			anchor := len(f.prefix)
			assert.Equal(t, mode, binary.LittleEndian.Uint32(body[anchor+12:anchor+16]),
				"logon mode must land in the 4-byte field after the user length")
			assert.Equal(t, mode, f.mode, "and must read back through the detector")
		})
	}
}

// TestCountingHandlerCapturesTheNegotiatedEncoding covers the accessor the
// sqlplus integration test leans on to prove it exercised the wide path. That
// assertion is the only thing stopping the integration test from passing on a
// thin session, so an accessor that silently returned nothing would make it
// vacuous — and only the integration build calls it, where a mistake surfaces
// slowly and behind a container.
func TestCountingHandlerCapturesTheNegotiatedEncoding(t *testing.T) {
	t.Parallel()

	h := newCountingHandler()
	logger := slog.New(h)

	logger.DebugContext(t.Context(), "sending AUTH challenge", slog.Bool("wide_encoding", false))
	logger.DebugContext(t.Context(), "sending AUTH challenge", slog.Bool("wide_encoding", true))

	assert.Equal(t, []bool{false, true}, h.boolsFor("sending AUTH challenge", "wide_encoding"),
		"every value must be recorded, in order")
	assert.Empty(t, h.boolsFor("sending AUTH challenge", "no_such_attr"))
	assert.Empty(t, h.boolsFor("no such message", "wide_encoding"))
}

// sendSyntheticAuth drives one synthetic AUTH send over a pipe and returns the
// packet that reached the upstream. clientAuthPhase1Pkt/Phase2Pkt are left nil,
// which is what selects the synthetic path without touching the
// forceSyntheticUpstreamAuth global — mutating that from a parallel test would
// be a data race under the detector.
func sendSyntheticAuth(t *testing.T, wide bool, send func(*session) error) *TNSPacket {
	t.Helper()

	upstreamSide, serverSide := net.Pipe()

	t.Cleanup(func() {
		_ = upstreamSide.Close()
		_ = serverSide.Close()
	})

	deadline := time.Now().Add(5 * time.Second)
	require.NoError(t, upstreamSide.SetDeadline(deadline))
	require.NoError(t, serverSide.SetDeadline(deadline))

	s := &session{
		ctx:                context.Background(),
		logger:             slog.New(slog.DiscardHandler),
		upstreamConn:       upstreamSide,
		clientWideEncoding: wide,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- send(s) }()

	pkt, err := readTNSPacket(serverSide)
	require.NoError(t, err)
	require.NoError(t, <-errCh)

	return pkt
}
