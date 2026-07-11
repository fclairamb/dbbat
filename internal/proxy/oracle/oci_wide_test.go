package oracle

import (
	"encoding/binary"
	"testing"
)

func TestWideKVRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key, value string
		flag       int
	}{
		{"AUTH_SESSKEY", "40414243", 1},
		{"AUTH_VFR_DATA", "DEADBEEF", 18453},
		{"AUTH_TERMINAL", "", 0},
		// Value longer than one CLR chunk (exercises the 0xfe long form).
		{"AUTH_PASSWORD", string(make([]byte, 300)), 0},
	}

	for _, tc := range cases {
		buf := writeWideKV(nil, tc.key, tc.value, tc.flag)

		pair, ok := readWideKVPair(buf)
		if !ok {
			t.Fatalf("%s: readWideKVPair failed", tc.key)
		}

		if string(pair.Key) != tc.key {
			t.Errorf("%s: key = %q", tc.key, pair.Key)
		}

		if string(pair.Value) != tc.value {
			t.Errorf("%s: value len = %d, want %d", tc.key, len(pair.Value), len(tc.value))
		}

		if pair.Flag != tc.flag {
			t.Errorf("%s: flag = %d, want %d", tc.key, pair.Flag, tc.flag)
		}

		if pair.Consumed != len(buf) {
			t.Errorf("%s: consumed %d, want %d", tc.key, pair.Consumed, len(buf))
		}
	}
}

func TestParseWideAuthKVDictionary(t *testing.T) {
	t.Parallel()

	// Build a 2-pair wide dictionary (count uint16-LE) and parse it back.
	buf := binary.LittleEndian.AppendUint16(nil, 2)
	buf = writeWideKV(buf, "AUTH_VFR_DATA", "0011AABB", 18453)
	buf = writeWideKV(buf, "AUTH_PBKDF2_VGEN_COUNT", "4096", 0)

	resp := &upstreamAuthResponse{properties: map[string]string{}}
	if _, ok := parseWideAuthKVDictionary(buf, resp); !ok {
		t.Fatal("parseWideAuthKVDictionary failed")
	}

	if resp.verifierType != 18453 {
		t.Errorf("verifierType = %d, want 18453", resp.verifierType)
	}

	if resp.pbkdf2VgenCount != 4096 {
		t.Errorf("pbkdf2VgenCount = %d, want 4096", resp.pbkdf2VgenCount)
	}
}

func TestIsOCIWideAuthPhase1(t *testing.T) {
	t.Parallel()

	oci := []byte{0x03, 0x76, 0x02, 0x01, 0x03, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	if !isOCIWideAuthPhase1(oci) {
		t.Error("expected OCI wide detection on sentinel body")
	}

	thin := []byte{0x03, 0x76, 0x00, 0x01, 0x01, 0x09}
	if isOCIWideAuthPhase1(thin) {
		t.Error("thin body misdetected as OCI wide")
	}
}
