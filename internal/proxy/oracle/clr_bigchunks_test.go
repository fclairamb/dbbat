package oracle

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodeBigChunkCLR encodes data in the UseBigClrChunks long form:
// 0xFE + (compressed-int chunkLen + chunk)* + 0x00. Uses a single chunk.
func encodeBigChunkCLR(data []byte) []byte {
	out := make([]byte, 0, len(data)+8)
	out = append(out, 0xFE)
	out = append(out, ttcCompressedUint(uint64(len(data)))...)
	out = append(out, data...)
	out = append(out, 0x00)

	return out
}

// bigChunkKeyVal encodes one TTC AUTH KV pair whose value uses the
// UseBigClrChunks long form — what a client sends once the server advertises
// ServerCompileTimeCaps[37]&0x20. Keys are always short, so they keep the
// encoding-agnostic short form.
func bigChunkKeyVal(key, value string, flag int, chunkSize int) []byte {
	buf := ttcCompressedUint(uint64(len(key)))
	buf = append(buf, ttcClr([]byte(key))...)
	buf = append(buf, ttcCompressedUint(uint64(len(value)))...)
	buf = append(buf, encodeBigChunkCLRSplit([]byte(value), chunkSize)...)
	buf = append(buf, ttcCompressedUint(uint64(flag))...)

	return buf
}

// longConnectString is a realistic AUTH_CONNECT_STRING over the 252-byte
// short-form limit: the descriptor a client builds when the host is a
// load-balancer DNS name. This is the value the UseBigClrChunks hardening
// exists for — and, until observeBigClrChunksFlag was reading caps[41] instead
// of caps[37], the one the fallback rewrite never actually saw in the encoding
// the client was really using.
const longConnectString = "(DESCRIPTION=(ADDRESS_LIST=(LOAD_BALANCE=on)(FAILOVER=on)" +
	"(ADDRESS=(PROTOCOL=TCP)(HOST=k8s-tooling-dbbatpro-c33c5852d8-69841f65b5cd36e2.elb.eu-west-3.amazonaws.com)(PORT=1522))" +
	"(ADDRESS=(PROTOCOL=TCP)(HOST=k8s-tooling-dbbatpro-c33c5852d8-69841f65b5cd36e2.elb.eu-west-1.amazonaws.com)(PORT=1522)))" +
	"(CONNECT_DATA=(SERVICE_NAME=FREEPDB1)(CID=(PROGRAM=sqlplus)(HOST=workstation)(USER=connector))))"

func TestReadCLRVariant_BigChunks_LongValue(t *testing.T) {
	t.Parallel()

	// A 350-byte value (like AUTH_CONNECT_STRING with a long load-balancer host)
	// exceeds the 252-byte short-form limit, so it uses the 0xFE long form.
	val := bytes.Repeat([]byte("A"), 350)
	buf := encodeBigChunkCLR(val)

	got, n := readCLRVariant(buf, true)
	if n != len(buf) {
		t.Fatalf("consumed %d, want %d", n, len(buf))
	}

	if !bytes.Equal(got, val) {
		t.Fatalf("value mismatch: got %d bytes, want %d", len(got), len(val))
	}

	// The single-byte-chunk reader (bigChunks=false) must NOT decode it
	// correctly — it reads the compressed-int length prefix as a 1-byte chunk
	// length. This is exactly the desync the hardening guards against.
	if wrong, _ := readCLRVariant(buf, false); bytes.Equal(wrong, val) {
		t.Fatal("single-byte-chunk reader unexpectedly decoded a big-chunk value")
	}
}

func TestReadCLRVariant_ShortValue_EncodingAgnostic(t *testing.T) {
	t.Parallel()

	// Short values are byte-identical in both encodings: readCLRVariant must
	// return the same result regardless of the bigChunks flag.
	val := []byte("AUTH_CONNECT_STRING")
	buf := ttcClr(val)

	a, an := readCLRVariant(buf, false)
	b, bn := readCLRVariant(buf, true)

	if an != bn || !bytes.Equal(a, b) || !bytes.Equal(a, val) {
		t.Fatalf("short-value decode differs by flag: (%d,%q) vs (%d,%q)", an, a, bn, b)
	}
}

func TestReadAuthKVPair_BigChunkConnectString(t *testing.T) {
	t.Parallel()

	// Build a KV pair: AUTH_CONNECT_STRING = long descriptor, big-chunk encoded,
	// then a compressed-int flag. readAuthKVPair must stay aligned (correct
	// Consumed) only when told the value uses big chunks.
	desc := []byte("(DESCRIPTION=(ADDRESS=(PROTOCOL=tcp)(HOST=" +
		"k8s-tooling-dbbatpro-c33c5852d8-69841f65b5cd36e2.elb.eu-west-3.amazonaws.com)(PORT=1522))" +
		"(CONNECT_DATA=(SERVICE_NAME=TEST01)(CID=(PROGRAM=x)(HOST=y)(USER=z))))")

	key := "AUTH_CONNECT_STRING"

	buf := ttcCompressedUint(uint64(len(key)))
	buf = append(buf, byte(len(key)))
	buf = append(buf, key...)
	buf = append(buf, ttcCompressedUint(uint64(len(desc)))...)
	buf = append(buf, encodeBigChunkCLR(desc)...)
	buf = append(buf, ttcCompressedUint(0)...) // flag

	pair, ok := readAuthKVPair(buf, false, true)
	if !ok {
		t.Fatal("readAuthKVPair(bigChunks=true) failed on a long connect string")
	}

	if string(pair.Key) != key {
		t.Errorf("key = %q, want %q", pair.Key, key)
	}

	if !bytes.Equal(pair.Value, desc) {
		t.Errorf("value mismatch: got %d bytes, want %d", len(pair.Value), len(desc))
	}

	if pair.Consumed != len(buf) {
		t.Errorf("consumed %d, want %d (misalignment would break the next pair)", pair.Consumed, len(buf))
	}
}

// longSpeedyKey stands in for an AUTH_PBKDF2_SPEEDY_KEY that has outgrown the
// 252-byte short form. Today's is 160 hex chars, comfortably short — this is
// the substituted value the write side has to frame in the negotiated
// encoding, and the only reason the write-side bug is not live.
var longSpeedyKey = strings.Repeat("A1B2C3D4", 50) // 400 chars

// decodeKVPairs walks pairCount TTC KV pairs and returns them keyed by name,
// failing the test on any misparse or leftover bytes. Reading the rewrite's
// output back is the only way to assert on a long value: past 252 bytes it is
// split across chunks, so its bytes are no longer contiguous in the wire form.
func decodeKVPairs(t *testing.T, buf []byte, pairCount int, bigChunks bool) map[string]string {
	t.Helper()

	out := make(map[string]string, pairCount)
	pos := 0

	for i := range pairCount {
		pair, ok := readAuthKVPair(buf[pos:], false, bigChunks)
		require.True(t, ok, "pair %d/%d failed to parse", i, pairCount)

		out[string(pair.Key)] = string(pair.Value)
		pos += pair.Consumed
	}

	require.Equal(t, len(buf), pos, "pair walk did not consume the whole buffer")

	return out
}

// TestRewritePhase2KVPairs_BigChunkConnectString is the point of the
// capability-offset fix: with UseBigClrChunks correctly observed as set, the
// Phase 2 fallback rewrite must walk a client's big-chunk-encoded long
// AUTH_CONNECT_STRING, swap the AUTH_* secrets around it, and stay byte-aligned
// for every pair that follows.
//
// The bigChunks=false leg is the state dbbat shipped in: the flag was read off
// caps[41] and answered false on every 19c/23ai session, so the walk decoded a
// compressed-int chunk length as a single-byte one and lost the pair boundary.
//
// The speedy key is deliberately over the short-form limit, which covers the
// *write* side too: the substituted value has to go back out with compressed-int
// chunk lengths, not ttcClr's single-byte ones.
func TestRewritePhase2KVPairs_BigChunkConnectString(t *testing.T) {
	t.Parallel()

	require.Greater(t, len(longConnectString), 0xFC,
		"the fixture must exceed the short-form limit or nothing is being tested")
	require.Greater(t, len(longSpeedyKey), 0xFC,
		"the substituted value must exceed the short-form limit or the write side is untested")

	sec := &upstreamAuthSecrets{
		encClientSessKey: "NEWSESS_HEX",
		encPassword:      "NEWPWD_HEX",
		eSpeedyKey:       longSpeedyKey,
	}

	// A client's pair set, in wire order, with the long connect string in the
	// middle so a misparse also destroys the pairs after it.
	connectPair := bigChunkKeyVal("AUTH_CONNECT_STRING", longConnectString, 0, 252)

	buf := ttcKeyVal("AUTH_SESSKEY", "OLDSESS_HEX", 1)
	buf = append(buf, ttcKeyVal("AUTH_PASSWORD", "OLDPWD_HEX", 0)...)
	buf = append(buf, ttcKeyVal("AUTH_PBKDF2_SPEEDY_KEY", "OLDSPEEDY_HEX", 0)...)
	buf = append(buf, connectPair...)
	buf = append(buf, ttcKeyVal("AUTH_TERMINAL", "unknown", 0)...)
	buf = append(buf, ttcKeyVal("AUTH_ACL", "4400", 0)...)

	const pairCount = 6

	out, err := rewritePhase2KVPairs(buf, pairCount, sec, true)
	require.NoError(t, err)

	// The three AUTH_* secrets are the upstream's now. The long one is read
	// back rather than searched for: it is chunked on the wire.
	assert.Contains(t, string(out), sec.encClientSessKey)
	assert.Contains(t, string(out), sec.encPassword)
	assert.NotContains(t, string(out), "OLDSESS_HEX")
	assert.NotContains(t, string(out), "OLDSPEEDY_HEX")

	decoded := decodeKVPairs(t, out, pairCount, true)
	assert.Equal(t, sec.eSpeedyKey, decoded["AUTH_PBKDF2_SPEEDY_KEY"],
		"a substituted value over the short-form limit must re-encode in the negotiated form")
	assert.Equal(t, sec.encClientSessKey, decoded["AUTH_SESSKEY"])
	assert.Equal(t, sec.encPassword, decoded["AUTH_PASSWORD"])
	assert.Equal(t, longConnectString, decoded["AUTH_CONNECT_STRING"])

	// The long connect string is not rewritten, so it must come through
	// byte-for-byte — chunk framing included, since the upstream leg parses it
	// with the same capability the client encoded it with.
	assert.Contains(t, string(out), string(connectPair),
		"the big-chunk AUTH_CONNECT_STRING pair must pass through verbatim")

	// Alignment survived: the pairs after the long value are still there.
	assert.Contains(t, string(out), "AUTH_TERMINAL")
	assert.Contains(t, string(out), "AUTH_ACL")

	// Expected output = the same pair list with the three secrets swapped, the
	// long one framed the way the session negotiated.
	want := ttcKeyValChunked("AUTH_SESSKEY", sec.encClientSessKey, 1, true)
	want = append(want, ttcKeyValChunked("AUTH_PASSWORD", sec.encPassword, 0, true)...)
	want = append(want, ttcKeyValChunked("AUTH_PBKDF2_SPEEDY_KEY", sec.eSpeedyKey, 0, true)...)
	want = append(want, connectPair...)
	want = append(want, ttcKeyValChunked("AUTH_TERMINAL", "unknown", 0, true)...)
	want = append(want, ttcKeyValChunked("AUTH_ACL", "4400", 0, true)...)

	assert.Equal(t, want, out)

	// The pre-fix write side, on the same substituted value: single-byte chunk
	// lengths, which a big-chunk upstream cannot parse back.
	legacy := ttcKeyVal("AUTH_PBKDF2_SPEEDY_KEY", sec.eSpeedyKey, 0)
	assert.NotEqual(t, legacy, ttcKeyValChunked("AUTH_PBKDF2_SPEEDY_KEY", sec.eSpeedyKey, 0, true),
		"the two encodings must differ past the short-form limit or the fix is inert")

	if pair, ok := readAuthKVPair(legacy, false, true); ok {
		assert.NotEqual(t, sec.eSpeedyKey, string(pair.Value),
			"a big-chunk reader must not decode the single-byte-chunk form correctly")
	}

	// And the regression the capability-offset fix removes: told the wrong
	// capability, the walk desyncs on the very same bytes.
	_, err = rewritePhase2KVPairs(buf, pairCount, sec, false)
	require.Error(t, err, "a single-byte-chunk walk cannot parse a big-chunk value")
	require.ErrorIs(t, err, ErrAuthPhase2Rewrite)
}

// TestTTCKeyValChunked_LongValue_RoundTrip is the write-side round trip the
// read-side tests above mirror: a value over the short-form limit encoded with
// bigChunks=true must come back out of readCLRVariant(_, true) intact, and must
// NOT be what ttcClr would have written.
func TestTTCKeyValChunked_LongValue_RoundTrip(t *testing.T) {
	t.Parallel()

	val := strings.Repeat("DEADBEEF", 60) // 480 bytes: two chunks at 252

	buf := ttcKeyValChunked("AUTH_PASSWORD", val, 0, true)

	pair, ok := readAuthKVPair(buf, false, true)
	require.True(t, ok, "a big-chunk-encoded pair must parse with bigChunks=true")
	assert.Equal(t, "AUTH_PASSWORD", string(pair.Key))
	assert.Equal(t, val, string(pair.Value))
	assert.Equal(t, len(buf), pair.Consumed,
		"misalignment here would destroy every pair that follows")

	// The CLR body alone also round-trips through readCLRVariant, which is the
	// function this encoder is the counterpart of.
	clr := encodeBigChunkCLRSplit([]byte(val), ttcClrChunkSize)

	got, n := readCLRVariant(clr, true)
	assert.Equal(t, len(clr), n)
	assert.Equal(t, val, string(got))

	// Two chunks, so the wire form is not the contiguous value.
	assert.NotContains(t, string(clr), val)
}

// TestTTCClrVariant_ShortValue_NoOp pins the property that makes this hardening
// safe to ship: every value dbbat substitutes today is under the short-form
// limit, and for those the negotiated encoding must not change a single byte.
func TestTTCClrVariant_ShortValue_NoOp(t *testing.T) {
	t.Parallel()

	// 0xFB is the last length that still fits the short form; 0xFC is the first
	// that does not, and is where the two encodings must diverge.
	for _, n := range []int{0, 1, 64, 96, 160, 0xFB} {
		val := bytes.Repeat([]byte("x"), n)

		assert.Equal(t, ttcClr(val), ttcClrVariant(val, true),
			"a %d-byte value must encode identically in both CLR forms", n)
		assert.Equal(t, ttcClrVariant(val, false), ttcClrVariant(val, true),
			"a %d-byte value must encode identically in both CLR forms", n)

		assert.Equal(t,
			ttcKeyVal("AUTH_SESSKEY", string(val), 1),
			ttcKeyValChunked("AUTH_SESSKEY", string(val), 1, true),
			"a %d-byte KV value must encode identically in both CLR forms", n)
	}

	long := bytes.Repeat([]byte("x"), 0xFC)
	assert.NotEqual(t, ttcClr(long), ttcClrVariant(long, true),
		"at the short-form limit the encodings must diverge, or the flag does nothing")
}

// TestBuildAuthChallenge_HonoursBigClrChunks is the same property on the other
// direction: the AUTH challenge dbbat synthesizes *for the client* in
// terminated-auth mode must frame its values in the CLR long form the session
// negotiated, not always in single-byte chunks.
//
// Today every value in that challenge is short — AUTH_SESSKEY (64 hex chars),
// AUTH_VFR_DATA, AUTH_PBKDF2_CSK_SALT, the two PBKDF2 counts, the DB id — so the
// flag must not move a single byte. It becomes live the moment a salt or
// verifier grows past the 252-byte short-form limit, which the second half
// forces with a synthetic salt.
func TestBuildAuthChallenge_HonoursBigClrChunks(t *testing.T) {
	t.Parallel()

	const (
		sessKey = "A1B2C3D4E5F60718293A4B5C6D7E8F90A1B2C3D4E5F60718293A4B5C6D7E8F90" // 64 hex
		vfrData = "0A1B2C3D4E5F60718293A4B5C6D7E8F9"
		salt    = "1122334455667788"
	)

	// Both the customHash (6-pair) and legacy (2-pair) shapes.
	for _, saltHex := range []string{salt, ""} {
		off := buildAuthChallenge(sessKey, vfrData, saltHex, 4096, 3, VerifierType18453, false, false)
		on := buildAuthChallenge(sessKey, vfrData, saltHex, 4096, 3, VerifierType18453, false, true)

		assert.Equal(t, off, on,
			"today's short challenge values must encode identically in both CLR forms")
	}

	// A salt past the short-form limit is where the flag has to bite.
	longSalt := strings.Repeat("A1B2C3D4", 40) // 320 chars
	require.Greater(t, len(longSalt), 0xFC,
		"the fixture must exceed the short-form limit or nothing is being tested")

	big := buildAuthChallenge(sessKey, vfrData, longSalt, 4096, 3, VerifierType18453, false, true)
	single := buildAuthChallenge(sessKey, vfrData, longSalt, 4096, 3, VerifierType18453, false, false)
	require.NotEqual(t, single, big,
		"past the short-form limit the negotiated encoding must change the bytes")

	// The long value round-trips through readCLRVariant(_, true) — the reader
	// this encoder is the counterpart of — and is chunked, not contiguous.
	clr := encodeBigChunkCLRSplit([]byte(longSalt), ttcClrChunkSize)
	assert.Contains(t, string(big), string(clr),
		"the long salt must be written in the big-chunk long form")
	assert.NotContains(t, string(big), longSalt, "a chunked value is not contiguous on the wire")

	got, n := readCLRVariant(clr, true)
	assert.Equal(t, len(clr), n)
	assert.Equal(t, longSalt, string(got))

	// And the whole challenge still walks pair-by-pair with the same capability:
	// header is data flags (2) + func (1) + a compressed pair count.
	pairs, hn := readCompressedInt(big[3:])
	require.Equal(t, 6, pairs)

	decoded := decodeKVPairs(t, big[3+hn:], pairs, true)
	assert.Equal(t, longSalt, decoded[authKeyPbkdf2CskSalt])
	assert.Equal(t, sessKey, decoded[authKeySessKey])
	assert.Equal(t, vfrData, decoded[authKeyVfrData])
	assert.Equal(t, dbbatGloballyUniqueDBID, decoded[authKeyGloballyUniqueDBID+"\x00"])
}

// TestReplaceAuthKVValue_BigChunkLongValue covers the anchored splice — the
// path that both read the replaced value with plain readCLR and wrote the new
// one with single-byte chunks.
func TestReplaceAuthKVValue_BigChunkLongValue(t *testing.T) {
	t.Parallel()

	oldVal := strings.Repeat("OLD_", 100) // 400 bytes
	newVal := strings.Repeat("NEW1", 120) // 480 bytes
	trailer := ttcKeyVal("AUTH_ACL", "4400", 0)

	body := bigChunkKeyVal(authKeySessKey, oldVal, 1, 252)
	body = append(body, trailer...)

	out := replaceAuthKVValue(body, authKeySessKey, newVal, false, true)
	require.NotEqual(t, body, out, "the splice must not decline on a big-chunk value")

	decoded := decodeKVPairs(t, out, 2, true)
	assert.Equal(t, newVal, decoded[authKeySessKey])
	assert.Equal(t, "4400", decoded["AUTH_ACL"], "the pair after the splice must survive")

	// The flag is preserved across the splice.
	pair, ok := readAuthKVPair(out, false, true)
	require.True(t, ok)
	assert.Equal(t, 1, pair.Flag)

	// The trailing pair comes through byte-for-byte.
	assert.Contains(t, string(out), string(trailer))
}

// TestRewriteAuthPhase2_FallbackBigChunkConnectString drives the same value
// through rewriteAuthPhase2's fallback branch end to end — preamble, username
// splice and KV walk — rather than the KV walk alone.
//
// The body carries no username, which is what makes rewriteAuthPhase2Anchored
// decline (it locates the username by the run of name bytes in front of the
// first AUTH_ key, and there is none); the test asserts that, so it cannot
// silently start exercising the anchored path instead.
func TestRewriteAuthPhase2_FallbackBigChunkConnectString(t *testing.T) {
	t.Parallel()

	sec := &upstreamAuthSecrets{
		encClientSessKey: "NEWSESS_HEX",
		encPassword:      "NEWPWD_HEX",
	}

	connectPair := bigChunkKeyVal("AUTH_CONNECT_STRING", longConnectString, 0, 252)

	body := make([]byte, 0, len(connectPair)+128)
	body = append(body, byte(TTCFuncPiggyback), PiggybackSubAuth2, 0x00)
	body = append(body, 0x00, 0x00)                  // no username
	body = append(body, ttcCompressedUint(0x101)...) // mode
	body = append(body, 0x01)                        // padding
	body = append(body, ttcCompressedUint(4)...)     // pair count
	body = append(body, 0x01, 0x01)                  // marker
	body = append(body, ttcKeyVal("AUTH_SESSKEY", "OLDSESS_HEX", 1)...)
	body = append(body, connectPair...)
	body = append(body, ttcKeyVal("AUTH_PASSWORD", "OLDPWD_HEX", 0)...)
	body = append(body, ttcKeyVal("AUTH_ACL", "4400", 0)...)

	_, anchored := rewriteAuthPhase2Anchored(body, "CONNECTOR", sec, true)
	require.False(t, anchored, "this body must exercise the KV-walk fallback, not the anchor")

	out, err := rewriteAuthPhase2(body, "CONNECTOR", sec, true)
	require.NoError(t, err)

	assert.Contains(t, string(out), string(connectPair),
		"the big-chunk AUTH_CONNECT_STRING must survive the fallback rewrite verbatim")
	assert.Contains(t, string(out), sec.encClientSessKey)
	assert.Contains(t, string(out), sec.encPassword)
	assert.Contains(t, string(out), "AUTH_ACL")

	// The rewritten body must still parse as a Phase 2 body (parseAuthPhase2
	// takes the 2-byte data flags in front).
	gotSess, gotPwd, err := parseAuthPhase2(append([]byte{0x00, 0x00}, out...))
	require.NoError(t, err)
	assert.Equal(t, sec.encClientSessKey, gotSess)
	assert.Equal(t, sec.encPassword, gotPwd)

	// Same body, the flag dbbat used to compute: the fallback fails outright.
	_, err = rewriteAuthPhase2(body, "CONNECTOR", sec, false)
	require.Error(t, err, "the pre-fix flag value breaks this rewrite")
}
