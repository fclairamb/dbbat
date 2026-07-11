package oracle

import (
	"bytes"
	"testing"
)

// encodeBigChunkCLR encodes data in go-ora's UseBigClrChunks long form:
// 0xfe + [compressed-int chunkLen][chunk]... + 0x00. Single chunk for the test.
func encodeBigChunkCLR(data []byte) []byte {
	out := []byte{0xFE}
	out = append(out, ttcCompressedUint(uint64(len(data)))...)
	out = append(out, data...)
	out = append(out, 0x00)

	return out
}

func TestReadCLRVariant_BigChunks_LongValue(t *testing.T) {
	t.Parallel()

	// A 350-byte value (like AUTH_CONNECT_STRING with a long host) — longer than
	// the 252-byte short-form limit, so it uses the 0xfe long form.
	val := bytes.Repeat([]byte("A"), 350)
	buf := encodeBigChunkCLR(val)

	got, n := readCLRVariant(buf, true)
	if n != len(buf) {
		t.Fatalf("consumed %d, want %d", n, len(buf))
	}

	if !bytes.Equal(got, val) {
		t.Fatalf("value mismatch: got %d bytes, want %d", len(got), len(val))
	}

	// The 1-byte-chunk reader (bigChunks=false) must NOT decode it correctly —
	// it interprets the compressed-int length prefix as a 1-byte chunk length.
	if wrong, _ := readCLRVariant(buf, false); bytes.Equal(wrong, val) {
		t.Fatal("1-byte-chunk reader unexpectedly decoded a big-chunk value")
	}
}

func TestReadAuthKVPair_BigChunkConnectString(t *testing.T) {
	t.Parallel()

	// Build a KV pair: AUTH_CONNECT_STRING = long descriptor, big-chunk encoded.
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

	pair, ok := readAuthKVPair(buf, true)
	if !ok {
		t.Fatal("readAuthKVPair(bigChunks=true) failed on a long connect string")
	}

	if string(pair.Key) != key {
		t.Errorf("key = %q", pair.Key)
	}

	if !bytes.Equal(pair.Value, desc) {
		t.Errorf("value mismatch: got %d bytes, want %d", len(pair.Value), len(desc))
	}
}
