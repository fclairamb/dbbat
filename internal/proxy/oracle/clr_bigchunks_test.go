package oracle

import (
	"bytes"
	"testing"
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
