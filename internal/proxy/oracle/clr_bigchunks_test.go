package oracle

import (
	"bytes"
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

// TestRewritePhase2KVPairs_BigChunkConnectString is the point of the
// capability-offset fix: with UseBigClrChunks correctly observed as set, the
// Phase 2 fallback rewrite must walk a client's big-chunk-encoded long
// AUTH_CONNECT_STRING, swap the AUTH_* secrets around it, and stay byte-aligned
// for every pair that follows.
//
// The bigChunks=false leg is the state dbbat shipped in: the flag was read off
// caps[41] and answered false on every 19c/23ai session, so the walk decoded a
// compressed-int chunk length as a single-byte one and lost the pair boundary.
func TestRewritePhase2KVPairs_BigChunkConnectString(t *testing.T) {
	t.Parallel()

	require.Greater(t, len(longConnectString), 0xFC,
		"the fixture must exceed the short-form limit or nothing is being tested")

	sec := &upstreamAuthSecrets{
		encClientSessKey: "NEWSESS_HEX",
		encPassword:      "NEWPWD_HEX",
		eSpeedyKey:       "NEWSPEEDY_HEX",
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

	// The three AUTH_* secrets are the upstream's now.
	assert.Contains(t, string(out), sec.encClientSessKey)
	assert.Contains(t, string(out), sec.encPassword)
	assert.Contains(t, string(out), sec.eSpeedyKey)
	assert.NotContains(t, string(out), "OLDSESS_HEX")

	// The long connect string is not rewritten, so it must come through
	// byte-for-byte — chunk framing included, since the upstream leg parses it
	// with the same capability the client encoded it with.
	assert.Contains(t, string(out), string(connectPair),
		"the big-chunk AUTH_CONNECT_STRING pair must pass through verbatim")

	// Alignment survived: the pairs after the long value are still there.
	assert.Contains(t, string(out), "AUTH_TERMINAL")
	assert.Contains(t, string(out), "AUTH_ACL")

	// Expected output = the same pair list with the three secrets swapped.
	want := ttcKeyVal("AUTH_SESSKEY", sec.encClientSessKey, 1)
	want = append(want, ttcKeyVal("AUTH_PASSWORD", sec.encPassword, 0)...)
	want = append(want, ttcKeyVal("AUTH_PBKDF2_SPEEDY_KEY", sec.eSpeedyKey, 0)...)
	want = append(want, connectPair...)
	want = append(want, ttcKeyVal("AUTH_TERMINAL", "unknown", 0)...)
	want = append(want, ttcKeyVal("AUTH_ACL", "4400", 0)...)

	assert.Equal(t, want, out)

	// And the regression this fix removes: told the wrong capability, the walk
	// desyncs on the very same bytes.
	_, err = rewritePhase2KVPairs(buf, pairCount, sec, false)
	require.Error(t, err, "a single-byte-chunk walk cannot parse a big-chunk value")
	require.ErrorIs(t, err, ErrAuthPhase2Rewrite)
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
