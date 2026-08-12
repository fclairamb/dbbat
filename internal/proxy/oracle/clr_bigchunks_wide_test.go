package oracle

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bigChunkKeyValWide is the wide (OCI) counterpart of bigChunkKeyVal: one
// [keyLen:4 LE][CLR key][valLen:4 LE][CLR val][flag:4 LE] pair whose value uses
// the UseBigClrChunks long form. Built out of encodeBigChunkCLRSplit rather than
// the wide encoder under test, so a read-side assertion is not just the write
// side restated.
func bigChunkKeyValWide(key, value string, flag int, bufferSized bool) []byte {
	buf := binary.LittleEndian.AppendUint32(nil, wideAuthSizeField(len(key), bufferSized))
	buf = append(buf, ttcClr([]byte(key))...)
	buf = binary.LittleEndian.AppendUint32(buf, wideAuthSizeField(len(value), bufferSized))
	buf = append(buf, encodeBigChunkCLRSplit([]byte(value), ttcClrChunkSize)...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(flag))

	return buf
}

// walkWideAuthPairs returns every KV pair of a captured wide AUTH body, in wire
// order, together with the raw first byte of each value's CLR — the byte that
// says short form (a length) or long form (0xFE).
func walkWideAuthPairs(t *testing.T, body []byte, bigChunks bool) []struct {
	pair     authKVPairResult
	valFirst byte
} {
	t.Helper()

	f, ok := detectWideAuthFraming(body)
	require.True(t, ok, "the fixture must be a wide AUTH body")

	anchor := len(f.prefix)
	pairCount := int(binary.LittleEndian.Uint32(body[anchor+24 : anchor+28]))

	// <ptr:8><userLen:4><mode:4><ptr:8><pairCount:4>[pad:4]<ptr:8><ptr:8>, then
	// the CLR username, then the pairs.
	pos := anchor + 8 + 4 + 4 + 8 + 4
	if f.pairCountPad {
		pos += 4
	}

	pos += 8 + 8
	pos += 1 + int(body[pos])

	out := make([]struct {
		pair     authKVPairResult
		valFirst byte
	}, 0, pairCount)

	for i := range pairCount {
		start := pos

		pair, ok := readAuthKVPairWide(body[start:], bigChunks)
		require.Truef(t, ok, "pair %d/%d unreadable", i, pairCount)

		// The value CLR sits after [keyLen:4][CLR key][valLen:4].
		_, kn := readCLR(body[start+4:])
		valLenPos := start + 4 + kn

		var valFirst byte
		if binary.LittleEndian.Uint32(body[valLenPos:valLenPos+4]) > 0 {
			valFirst = body[valLenPos+4]
		}

		out = append(out, struct {
			pair     authKVPairResult
			valFirst byte
		}{pair, valFirst})

		pos += pair.Consumed
	}

	require.Equal(t, len(body), pos, "the pair walk must consume the whole body")

	return out
}

// TestSqlplusCapture_NegotiatesBigClrChunksAndCarriesNoLongValue is the
// measurement that decided how the wide/OCI leg treats UseBigClrChunks. The
// leg used to drop the flag on the asserted claim that "OCI does not use it";
// this is what testdata/sqlplus_cursor_reexec.pcapng (macOS Instant Client 23.3
// → Oracle 23ai Free) actually establishes:
//
//   - the OCI session DOES negotiate the capability. It comes from the *server*
//     (ServerCompileTimeCaps[37]&0x20 in the Set Protocol reply), so
//     session.clientBigClrChunks is true here exactly as on a go-ora session.
//     The wide leg is therefore genuinely reachable with the flag set — being
//     wide is not what keeps it away;
//   - and no AUTH value in the login reaches the 0xFE long form: the longest is
//     AUTH_CONNECT_STRING at 172 bytes, well under the 252-byte short-form
//     limit, where the two encodings are byte-identical.
//
// So the capture cannot say which chunk-length encoding OCI writes past the
// limit — the claim is unverifiable from this evidence rather than confirmed by
// it, which is why the leg now follows the negotiated capability instead.
// Should a re-capture ever contain a long value, this test is where the real
// answer would show up: the assertion below would fail with a 0xFE first byte,
// and the pair walk would then say which reader keeps its alignment.
func TestSqlplusCapture_NegotiatesBigClrChunksAndCarriesNoLongValue(t *testing.T) {
	t.Parallel()

	const capture = "sqlplus_cursor_reexec.pcapng"

	raw, caps := firstSetProtocolReply(t, capture)

	require.Greater(t, len(caps), bigClrChunksCapIndex)
	assert.Equal(t, byte(0x7f), caps[bigClrChunksCapIndex],
		"the OCI session's ServerCompileTimeCaps[37], as measured")
	assert.True(t, observeBigClrChunksFlag(raw),
		"an OCI login negotiates UseBigClrChunks like any other: the capability is the server's")

	longest := 0

	for _, sub := range []byte{PiggybackSubAuth1, PiggybackSubAuth2} {
		body := captureAuthPacket(t, capture, sub).Payload[ttcDataFlagsSize:]

		for _, got := range walkWideAuthPairs(t, body, false) {
			key := string(got.pair.Key)

			assert.Lessf(t, len(got.pair.Value), 0xFC,
				"%s is %d bytes: the capture would then hold a long-form value", key, len(got.pair.Value))
			assert.NotEqualf(t, byte(0xFE), got.valFirst,
				"%s uses the CLR long form, which the capture was read as not containing", key)

			longest = max(longest, len(got.pair.Value))
		}
	}

	assert.Equal(t, 172, longest,
		"the longest captured AUTH value (AUTH_CONNECT_STRING) — a change here changes what this capture proves")

	// The same body read with the opposite flag decodes identically, which is
	// the mechanical statement of "this capture cannot tell the two apart".
	for _, sub := range []byte{PiggybackSubAuth1, PiggybackSubAuth2} {
		body := captureAuthPacket(t, capture, sub).Payload[ttcDataFlagsSize:]
		assert.Equal(t, walkWideAuthPairs(t, body, false), walkWideAuthPairs(t, body, true),
			"every value being short, both readers must agree on the whole captured body")
	}
}

// TestWideAuthLeg_BigChunkLongValue_RoundTrip is the wide counterpart of
// TestTTCKeyValChunked_LongValue_RoundTrip: a value over the short-form limit,
// written in the negotiated long form, must come back out of the wide reader
// intact and byte-aligned — and must not be what single-byte chunk lengths
// would have produced.
func TestWideAuthLeg_BigChunkLongValue_RoundTrip(t *testing.T) {
	t.Parallel()

	val := strings.Repeat("DEADBEEF", 60) // 480 bytes: two chunks at 252
	require.Greater(t, len(val), 0xFC,
		"the fixture must exceed the short-form limit or nothing is being tested")

	for name, bufferSized := range map[string]bool{"instantclient 3x": true, "db-bundled plain": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			buf := ttcKeyValWideSized(authKeyPassword, val, 0, bufferSized, true)

			// The encoder agrees with an independently built fixture.
			assert.Equal(t, bigChunkKeyValWide(authKeyPassword, val, 0, bufferSized), buf)

			pair, ok := readAuthKVPairWide(buf, true)
			require.True(t, ok, "a big-chunk wide pair must parse with bigChunks=true")
			assert.Equal(t, authKeyPassword, string(pair.Key))
			assert.Equal(t, val, string(pair.Value))
			assert.Equal(t, len(buf), pair.Consumed,
				"misalignment here would destroy every pair that follows")

			// Chunked, so the value is not contiguous on the wire.
			assert.NotContains(t, string(buf), val)

			// The single-byte-chunk reader must NOT recover it: that divergence is
			// the whole reason the flag has to reach this leg. It happens to fail
			// outright on this fixture, but "decoded something else" would be just
			// as much of a desync, so the assertion covers both.
			wrong, wrongOK := readAuthKVPairWide(buf, false)
			assert.False(t, wrongOK && string(wrong.Value) == val,
				"a single-byte-chunk reader must not decode a big-chunk value")

			// And the reverse: the legacy single-byte-chunk writer produces bytes a
			// big-chunk reader cannot recover, which is what the leg used to send.
			legacy := ttcKeyValWideSized(authKeyPassword, val, 0, bufferSized, false)
			require.NotEqual(t, legacy, buf,
				"past the short-form limit the two encodings must differ or the fix is inert")

			stale, staleOK := readAuthKVPairWide(legacy, true)
			assert.False(t, staleOK && string(stale.Value) == val,
				"a big-chunk reader must not recover the single-byte-chunk form")
		})
	}
}

// TestReplaceAuthKVValueWide_BigChunkLongValue drives the same value through the
// wide splice — the path that reads the value it is replacing and writes the new
// one — over a client body whose pairs continue after the spliced one.
func TestReplaceAuthKVValueWide_BigChunkLongValue(t *testing.T) {
	t.Parallel()

	oldVal := strings.Repeat("OLD_", 100) // 400 bytes
	trailer := ttcKeyValWideSized("AUTH_ACL", "4400", 0, true, true)

	body := bigChunkKeyValWide(authKeySessKey, oldVal, 1, true)
	body = append(body, trailer...)

	out := replaceAuthKVValueWide(body, authKeySessKey, longSpeedyKey, true)
	require.NotEqual(t, body, out, "the splice must not decline on a big-chunk value")

	pair, ok := readAuthKVPairWide(out, true)
	require.True(t, ok)
	assert.Equal(t, longSpeedyKey, string(pair.Value),
		"a substituted value over the short-form limit must re-encode in the negotiated form")
	assert.Equal(t, 1, pair.Flag, "the flag is preserved across the splice")

	// The 3x buffer-size convention survives a long value (ORA-28041 guard).
	valLenPos := 4 + 1 + len(authKeySessKey)
	assert.Equal(t, uint32(3*len(longSpeedyKey)),
		binary.LittleEndian.Uint32(out[valLenPos:valLenPos+4]))

	// The pair after the splice is untouched and still parses.
	assert.Contains(t, string(out), string(trailer))

	next, ok := readAuthKVPairWide(out[pair.Consumed:], true)
	require.True(t, ok, "the pair after the splice must survive")
	assert.Equal(t, "AUTH_ACL", string(next.Key))
	assert.Equal(t, "4400", string(next.Value))

	// Told the wrong form, the splice cannot even find the value's end: it reads
	// a compressed-int chunk length as a single-byte one.
	assert.NotEqual(t, out, replaceAuthKVValueWide(body, authKeySessKey, longSpeedyKey, false))
}

// TestWideAuthLeg_ShortValues_ByteIdentical is the regression that makes
// threading the flag through this leg safe to ship: every value dbbat writes
// wide today is under the short-form limit, and for those the negotiated
// encoding must not move a single byte — on any of the four write sites.
func TestWideAuthLeg_ShortValues_ByteIdentical(t *testing.T) {
	t.Parallel()

	// 0xFB is the last length that still fits the short form. The AUTH_* values
	// dbbat substitutes are 64 / 96 / 160.
	for _, n := range []int{0, 1, 64, 96, 160, 0xFB} {
		val := strings.Repeat("x", n)

		assert.Equalf(t,
			ttcKeyValWide(authKeySessKey, val, 1),
			ttcKeyValWideChunked(authKeySessKey, val, 1, true),
			"a %d-byte wide KV value must encode identically in both CLR forms", n)

		for _, bufferSized := range []bool{true, false} {
			assert.Equalf(t,
				ttcKeyValWideSized(authKeySessKey, val, 1, bufferSized, false),
				ttcKeyValWideSized(authKeySessKey, val, 1, bufferSized, true),
				"a %d-byte sized wide KV value must encode identically in both CLR forms", n)
		}

		// The splice, on a short value in either length convention.
		for _, bufferSized := range []bool{true, false} {
			body := append(bigChunkKeyValWideShort(authKeySessKey, "OLDVALUE", 1, bufferSized),
				ttcKeyValWideSized("AUTH_ACL", "4400", 0, bufferSized, false)...)

			assert.Equalf(t,
				replaceAuthKVValueWide(body, authKeySessKey, val, false),
				replaceAuthKVValueWide(body, authKeySessKey, val, true),
				"a %d-byte wide splice must produce identical bytes in both CLR forms", n)
		}
	}

	// The synthetic wide bodies, with dbbat's real pair sets.
	ident := driverIdentity{ProgramName: "dbbat for sqlplus", HostName: "workstation", OSUser: "connector", PID: 42, DriverName: "dbbat"}
	sec := &upstreamAuthSecrets{
		encClientSessKey: strings.Repeat("A", 64),
		encPassword:      strings.Repeat("B", 96),
		eSpeedyKey:       strings.Repeat("C", 160),
	}

	f1 := defaultWideAuthFraming(PiggybackSubAuth1, authPhase1FuncSeq, logonModeNoNewPass)
	assert.Equal(t,
		buildClientAuthPhase1Wide(f1, "system", ident, false),
		buildClientAuthPhase1Wide(f1, "system", ident, true),
		"today's wide Phase 1 must be byte-identical in both CLR forms")

	f2 := defaultWideAuthFraming(PiggybackSubAuth2, authPhase2FuncSeq,
		logonModeNoNewPass|logonModeUserAndPass)
	assert.Equal(t,
		buildClientAuthPhase2Wide(f2, "system", ident, sec, false),
		buildClientAuthPhase2Wide(f2, "system", ident, sec, true),
		"today's wide Phase 2 must be byte-identical in both CLR forms")

	// And the client-facing challenge, which 2026-08-12-07 pinned the same way.
	assert.Equal(t,
		buildAuthChallenge(sec.encClientSessKey, "0A1B2C3D", "1122334455667788", 4096, 3, VerifierType18453, true, false),
		buildAuthChallenge(sec.encClientSessKey, "0A1B2C3D", "1122334455667788", 4096, 3, VerifierType18453, true, true),
		"today's wide challenge must be byte-identical in both CLR forms")

	// A challenge value past the limit is where the flag has to bite, on the wide
	// leg as much as the thin one.
	longSalt := strings.Repeat("A1B2C3D4", 40) // 320 chars
	assert.NotEqual(t,
		buildAuthChallenge(sec.encClientSessKey, "0A1B2C3D", longSalt, 4096, 3, VerifierType18453, true, false),
		buildAuthChallenge(sec.encClientSessKey, "0A1B2C3D", longSalt, 4096, 3, VerifierType18453, true, true),
		"past the short-form limit the negotiated encoding must change the wide challenge too")
}

// bigChunkKeyValWideShort builds a wide pair with a SHORT value, written by hand
// rather than through the encoder under test.
func bigChunkKeyValWideShort(key, value string, flag int, bufferSized bool) []byte {
	buf := binary.LittleEndian.AppendUint32(nil, wideAuthSizeField(len(key), bufferSized))
	buf = append(buf, byte(len(key)))
	buf = append(buf, key...)
	buf = binary.LittleEndian.AppendUint32(buf, wideAuthSizeField(len(value), bufferSized))
	buf = append(buf, byte(len(value)))
	buf = append(buf, value...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(flag))

	return buf
}

// TestParseAuthKVDictionaryWide_BigChunkValue closes the loop on the read side:
// the upstream server's own AUTH dictionary is where readAuthKVPairWide is
// actually reached from, and it is the server that advertised the capability in
// the first place. A long value in its reply must be recovered, and the pairs
// after it must stay aligned.
func TestParseAuthKVDictionaryWide_BigChunkValue(t *testing.T) {
	t.Parallel()

	longVfr := strings.Repeat("F0E1D2C3", 50) // 400 bytes

	dict := binary.LittleEndian.AppendUint16(nil, 2)
	dict = append(dict, bigChunkKeyValWide(authKeyVfrData, longVfr, VerifierType18453, false)...)
	dict = append(dict, ttcKeyValWideSized(authKeySessKey, "DEADBEEF", 0, false, true)...)

	resp := &upstreamAuthResponse{properties: map[string]string{}}

	consumed, ok := parseAuthKVDictionary(dict, resp, true, true)
	require.True(t, ok, "a wide dictionary carrying a long value must parse with bigChunks=true")
	assert.Equal(t, len(dict), consumed)
	assert.Equal(t, longVfr, resp.properties[authKeyVfrData])
	assert.Equal(t, "DEADBEEF", resp.properties[authKeySessKey],
		"the pair after the long value must still be found")

	// The pre-fix behaviour on the same bytes: told single-byte chunks, the walk
	// loses the pair boundary rather than reading the value.
	legacy := &upstreamAuthResponse{properties: map[string]string{}}

	_, legacyOK := parseAuthKVDictionary(dict, legacy, true, false)
	assert.False(t, legacyOK && legacy.properties[authKeyVfrData] == longVfr,
		"a single-byte-chunk walk must not decode a big-chunk dictionary")

	// Sanity: the fixture really is in the long form.
	assert.True(t, bytes.Contains(dict, encodeBigChunkCLRSplit([]byte(longVfr), ttcClrChunkSize)))
}
