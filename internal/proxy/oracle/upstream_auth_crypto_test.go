package oracle

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"strings"
	"testing"
)

// TestPBKDF2SpeedyKeyMatchesReference verifies our PBKDF2 implementation
// against an independently-computed reference for the algorithm documented
// in go-ora/v2/auth_object.go (HMAC-SHA512 chained `turns` times, XORed).
func TestPBKDF2SpeedyKeyMatchesReference(t *testing.T) {
	t.Parallel()

	password := []byte("p@ssw0rd")
	salt, _ := hex.DecodeString("11223344556677889900AABBCCDDEEFF")
	buffer := append([]byte{}, salt...)
	buffer = append(buffer, []byte(pbkdf2SpeedyKeyLabel)...)

	got := pbkdf2SpeedyKey(buffer, password, 4096)
	want := referenceSpeedyKey(buffer, password, 4096)

	if !bytes.Equal(got, want) {
		t.Fatalf("pbkdf2SpeedyKey mismatch:\n got=%x\nwant=%x", got, want)
	}

	if len(got) != 64 {
		t.Fatalf("pbkdf2SpeedyKey length = %d, want 64", len(got))
	}
}

// TestPBKDF2SpeedyKeyDifferentTurns confirms different turns produce different
// outputs (sanity check that the loop runs as documented).
func TestPBKDF2SpeedyKeyDifferentTurns(t *testing.T) {
	t.Parallel()

	salt, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	password := []byte("password")
	buffer := append([]byte{}, salt...)
	buffer = append(buffer, []byte(pbkdf2SpeedyKeyLabel)...)

	a := pbkdf2SpeedyKey(buffer, password, 100)
	b := pbkdf2SpeedyKey(buffer, password, 200)

	if bytes.Equal(a, b) {
		t.Fatalf("turns=100 and turns=200 produced the same key, loop is a no-op")
	}
}

// TestAESCBCRoundTripPaddingTrue verifies encrypt+decrypt with keepPadding=true
// preserves the padded blob.
func TestAESCBCRoundTripPaddingTrue(t *testing.T) {
	t.Parallel()

	key := mustHex(t, strings.Repeat("aa", 32))
	plaintext := []byte("oracle plaintext that needs padding")

	ct, err := aesCBCEncryptZeroIV(key, plaintext, true)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	if len(ct)%aes.BlockSize != 0 {
		t.Fatalf("padded ciphertext not block aligned: %d", len(ct))
	}

	pt, err := aesCBCDecryptZeroIV(key, ct, true)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("round trip mismatch:\n got=%q\nwant=%q", pt, plaintext)
	}
}

// TestAESCBCRoundTripPaddingFalseTruncates verifies encrypt with
// keepPadding=false truncates to the original length.
func TestAESCBCRoundTripPaddingFalseTruncates(t *testing.T) {
	t.Parallel()

	key := mustHex(t, strings.Repeat("bb", 32))
	plaintext := bytes.Repeat([]byte{0x42}, aes.BlockSize)

	ct, err := aesCBCEncryptZeroIV(key, plaintext, false)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	if len(ct) != len(plaintext) {
		t.Fatalf("truncated ciphertext = %d, want %d", len(ct), len(plaintext))
	}
}

// TestComputeUpstreamAuthSecretsRoundTripPBKDF2 simulates a complete
// handshake: a synthetic upstream picks a known server session key and
// encrypts it under the verifier-derived AES key. Our client decrypts it,
// generates a fresh client session key, encrypts the password, and we
// verify the encrypted blobs round-trip back correctly.
func TestComputeUpstreamAuthSecretsRoundTripPBKDF2(t *testing.T) {
	t.Parallel()

	password := "supersecret"
	salt, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	pbkdf2ChkSalt, _ := hex.DecodeString("aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899")
	vgenCount := 4096
	sderCount := 100

	speedyKey := pbkdf2SpeedyKey(append(append([]byte{}, salt...), []byte(pbkdf2SpeedyKeyLabel)...), []byte(password), vgenCount)

	hash := sha512.New()
	hash.Write(append(speedyKey, salt...))
	verifierKey := hash.Sum(nil)[:32]

	serverSessKey := bytes.Repeat([]byte{0x77}, 32)

	encServerBytes := mustEncryptZeroIV(t, verifierKey, serverSessKey, false)
	encServerHex := strings.ToUpper(hex.EncodeToString(encServerBytes))

	sec := &upstreamAuthSecrets{
		verifierType:    VerifierType18453,
		customHash:      true,
		salt:            salt,
		pbkdf2ChkSalt:   pbkdf2ChkSalt,
		pbkdf2VgenCount: vgenCount,
		pbkdf2SderCount: sderCount,
	}

	if err := computeUpstreamAuthSecrets(password, encServerHex, sec); err != nil {
		t.Fatalf("computeUpstreamAuthSecrets: %v", err)
	}

	if !bytes.Equal(sec.serverSessionKey, serverSessKey) {
		t.Fatalf("server session key not recovered:\n got=%x\nwant=%x", sec.serverSessionKey, serverSessKey)
	}

	if len(sec.clientSessionKey) != 32 {
		t.Fatalf("client session key length = %d, want 32", len(sec.clientSessionKey))
	}

	encClientBytes, err := hex.DecodeString(sec.encClientSessKey)
	if err != nil {
		t.Fatalf("decode encClientSessKey hex: %v", err)
	}

	gotClientKey := mustDecryptZeroIV(t, verifierKey, encClientBytes, false)
	if !bytes.Equal(gotClientKey, sec.clientSessionKey) {
		t.Fatalf("client session key mismatch:\n got=%x\nwant=%x", gotClientKey, sec.clientSessionKey)
	}

	combined := referencePasswordEncKey(serverSessKey, sec.clientSessionKey, sec, true)

	encPwBytes, err := hex.DecodeString(sec.encPassword)
	if err != nil {
		t.Fatalf("decode encPassword hex: %v", err)
	}

	gotPwBuf := mustDecryptZeroIV(t, combined, encPwBytes, true)
	if len(gotPwBuf) <= 16 {
		t.Fatalf("decrypted password buffer too short: %d", len(gotPwBuf))
	}

	if got := string(gotPwBuf[16:]); got != password {
		t.Fatalf("password round-trip mismatch:\n got=%q\nwant=%q", got, password)
	}

	if sec.eSpeedyKey == "" {
		t.Fatalf("eSpeedyKey empty for verifier 18453")
	}

	speedyBytes, err := hex.DecodeString(sec.eSpeedyKey)
	if err != nil {
		t.Fatalf("decode eSpeedyKey: %v", err)
	}

	gotSpeedy := mustDecryptZeroIV(t, combined, speedyBytes, false)
	if len(gotSpeedy) <= 16 {
		t.Fatalf("decrypted speedy key buffer too short: %d", len(gotSpeedy))
	}

	if !bytes.Equal(gotSpeedy[16:16+len(sec.speedyKey)], sec.speedyKey) {
		t.Fatalf("speedy key round trip mismatch")
	}
}

// TestComputeUpstreamAuthSecretsRoundTrip6949 mirrors the PBKDF2 case for the
// legacy SHA-1 verifier with customHash=false (MD5/XOR derivation).
func TestComputeUpstreamAuthSecretsRoundTrip6949(t *testing.T) {
	t.Parallel()

	password := "legacypw"
	salt, _ := hex.DecodeString("0102030405060708090a")

	verifierKey := referenceVerifierKey6949([]byte(password), salt)

	serverSessKey := bytes.Repeat([]byte{0x33}, 48)

	encServerBytes := mustEncryptZeroIV(t, verifierKey, serverSessKey, false)
	encServerHex := strings.ToUpper(hex.EncodeToString(encServerBytes))

	sec := &upstreamAuthSecrets{
		verifierType: VerifierType6949,
		customHash:   false,
		salt:         salt,
	}

	if err := computeUpstreamAuthSecrets(password, encServerHex, sec); err != nil {
		t.Fatalf("computeUpstreamAuthSecrets: %v", err)
	}

	if !bytes.Equal(sec.serverSessionKey, serverSessKey) {
		t.Fatalf("server session key not recovered:\n got=%x\nwant=%x", sec.serverSessionKey, serverSessKey)
	}

	if sec.eSpeedyKey != "" {
		t.Fatalf("eSpeedyKey should be empty for verifier 6949")
	}

	combined := referencePasswordEncKey(serverSessKey, sec.clientSessionKey, sec, false)

	encPwBytes, err := hex.DecodeString(sec.encPassword)
	if err != nil {
		t.Fatalf("decode encPassword: %v", err)
	}

	gotPwBuf := mustDecryptZeroIV(t, combined, encPwBytes, true)
	if got := string(gotPwBuf[16:]); got != password {
		t.Fatalf("password round-trip mismatch:\n got=%q\nwant=%q", got, password)
	}
}

func TestBuildSecretsFromPhase1ResponseRequiresFields(t *testing.T) {
	t.Parallel()

	if _, err := buildSecretsFromPhase1Response(&upstreamAuthResponse{salt: "ab"}, true); err == nil {
		t.Fatalf("expected error when AUTH_SESSKEY is missing")
	}

	if _, err := buildSecretsFromPhase1Response(&upstreamAuthResponse{encServerSessKey: "ab"}, true); err == nil {
		t.Fatalf("expected error when AUTH_VFR_DATA is missing")
	}
}

func TestBuildSecretsClampsCounts(t *testing.T) {
	t.Parallel()

	resp := &upstreamAuthResponse{
		encServerSessKey: "AA",
		salt:             "00",
		verifierType:     VerifierType18453,
		pbkdf2VgenCount:  -1,
		pbkdf2SderCount:  0,
	}

	sec, err := buildSecretsFromPhase1Response(resp, true)
	if err != nil {
		t.Fatalf("buildSecretsFromPhase1Response: %v", err)
	}

	if sec.pbkdf2VgenCount != 4096 {
		t.Fatalf("pbkdf2VgenCount not clamped: %d", sec.pbkdf2VgenCount)
	}

	if sec.pbkdf2SderCount != 3 {
		t.Fatalf("pbkdf2SderCount not clamped: %d", sec.pbkdf2SderCount)
	}
}

func TestHexDecodeUpper(t *testing.T) {
	t.Parallel()

	got, err := hexDecodeUpper("DeAdBeEf")
	if err != nil {
		t.Fatalf("hexDecodeUpper: %v", err)
	}

	want := []byte{0xde, 0xad, 0xbe, 0xef}
	if !bytes.Equal(got, want) {
		t.Fatalf("hexDecodeUpper got=%x want=%x", got, want)
	}

	if _, err := hexDecodeUpper("xx"); err == nil {
		t.Fatalf("expected error on invalid hex")
	}

	if _, err := hexDecodeUpper("a"); err == nil {
		t.Fatalf("expected error on odd-length hex")
	}
}

// referenceSpeedyKey is an independent re-implementation of the algorithm
// for cross-checking pbkdf2SpeedyKey.
func referenceSpeedyKey(buffer, key []byte, turns int) []byte {
	mac := hmac.New(sha512.New, key)
	mac.Write(append(append([]byte{}, buffer...), 0, 0, 0, 1))
	first := mac.Sum(nil)

	temp := append([]byte{}, first...)
	out := append([]byte{}, first...)

	for i := 2; i <= turns; i++ {
		mac.Reset()
		mac.Write(temp)
		temp = mac.Sum(nil)

		for j := 0; j < 64; j++ {
			out[j] ^= temp[j]
		}
	}

	return out
}

func mustEncryptZeroIV(t *testing.T, key, plaintext []byte, keepPadding bool) []byte {
	t.Helper()

	out, err := aesCBCEncryptZeroIV(key, plaintext, keepPadding)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	return out
}

func mustDecryptZeroIV(t *testing.T, key, ciphertext []byte, stripPadding bool) []byte {
	t.Helper()

	out, err := aesCBCDecryptZeroIV(key, ciphertext, stripPadding)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	return out
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex.DecodeString: %v", err)
	}

	return b
}

// referenceVerifierKey6949 reproduces the SHA-1 path used by
// computeUpstreamAuthSecrets so the test asserts independently of that code.
func referenceVerifierKey6949(password, salt []byte) []byte {
	h := sha1.New()
	h.Write(append(password, salt...))
	out := h.Sum(nil)

	return append(out, 0, 0, 0, 0)
}

// referencePasswordEncKey re-derives the password encryption key without
// going through derivePasswordEncKey, for cross-validation.
func referencePasswordEncKey(serverKey, clientKey []byte, sec *upstreamAuthSecrets, customHash bool) []byte {
	if customHash {
		var (
			joined []byte
			retLen int
		)

		switch sec.verifierType {
		case VerifierType6949:
			joined = append(append([]byte{}, clientKey[:24]...), serverKey[:24]...)
			retLen = 24
		case VerifierType18453:
			joined = append(append([]byte{}, clientKey...), serverKey...)
			retLen = 32
		}

		hexBuf := strings.ToUpper(hex.EncodeToString(joined))
		full := pbkdf2SpeedyKey(sec.pbkdf2ChkSalt, []byte(hexBuf), sec.pbkdf2SderCount)

		return full[:retLen]
	}

	const start = 16

	buf := make([]byte, 24)
	for i := 0; i < 24; i++ {
		buf[i] = serverKey[i+start] ^ clientKey[i+start]
	}

	h1 := md5.Sum(buf[:16])
	h2 := md5.Sum(buf[16:])

	out := make([]byte, 0, 32)
	out = append(out, h1[:]...)
	out = append(out, h2[:]...)

	return out[:24]
}
