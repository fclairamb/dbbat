package oracle

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Oracle verifier types observed in AUTH_VFR_DATA's flag field.
const (
	// VerifierType6949 selects the SHA-1 / MD5-XOR (or SHA-1 / PBKDF2 customHash)
	// derivation paths.
	VerifierType6949 = 6949
	// VerifierType18453 selects the modern PBKDF2 / HMAC-SHA512 path.
	VerifierType18453 = 18453
)

const pbkdf2SpeedyKeyLabel = "AUTH_PBKDF2_SPEEDY_KEY"

// pbkdf2SpeedyKey computes the speedy key used by Oracle's PBKDF2 verifier 18453.
// It mirrors generateSpeedyKey in go-ora/v2/auth_object.go: HMAC-SHA512 chained
// `turns` times, XORing each intermediate hash into the running accumulator.
func pbkdf2SpeedyKey(buffer, key []byte, turns int) []byte {
	mac := hmac.New(sha512.New, key)
	mac.Write(append(buffer, 0, 0, 0, 1))

	firstHash := mac.Sum(nil)
	tempHash := make([]byte, len(firstHash))
	copy(tempHash, firstHash)

	for index1 := 2; index1 <= turns; index1++ {
		mac.Reset()
		mac.Write(tempHash)
		tempHash = mac.Sum(nil)

		for index2 := 0; index2 < 64; index2++ {
			firstHash[index2] ^= tempHash[index2]
		}
	}

	return firstHash
}

// pkcs5Pad applies PKCS#5 / PKCS#7 padding to the data.
func pkcs5Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+padding)
	copy(out, data)

	for i := len(data); i < len(out); i++ {
		out[i] = byte(padding)
	}

	return out
}

// aesCBCEncryptZeroIV encrypts data with AES in CBC mode and a zero IV. The
// caller decides whether to keep the trailing pad block via the keepPadding
// flag. When keepPadding is false, only the original (pre-pad) bytes are
// retained, matching go-ora's encryptSessionKey when padding=false.
func aesCBCEncryptZeroIV(key, plaintext []byte, keepPadding bool) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}

	originalLen := len(plaintext)
	plaintext = pkcs5Pad(plaintext, blk.BlockSize())

	enc := cipher.NewCBCEncrypter(blk, make([]byte, blk.BlockSize()))
	output := make([]byte, len(plaintext))
	enc.CryptBlocks(output, plaintext)

	if !keepPadding {
		return output[:originalLen], nil
	}

	return output, nil
}

// aesCBCDecryptZeroIV decrypts AES-CBC ciphertext encrypted with a zero IV.
// Trailing PKCS#5 padding is removed only when stripPadding is true and the
// final byte is a valid padding length; otherwise the full plaintext is
// returned. This mirrors go-ora's decryptSessionKey.
func aesCBCDecryptZeroIV(key, ciphertext []byte, stripPadding bool) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}

	if len(ciphertext)%blk.BlockSize() != 0 {
		return nil, ErrCiphertextNotAligned
	}

	dec := cipher.NewCBCDecrypter(blk, make([]byte, blk.BlockSize()))
	output := make([]byte, len(ciphertext))
	dec.CryptBlocks(output, ciphertext)

	if stripPadding && len(output) > 0 {
		num := int(output[len(output)-1])
		if num > 0 && num <= blk.BlockSize() {
			ok := true

			for x := len(output) - num; x < len(output); x++ {
				if output[x] != byte(num) {
					ok = false

					break
				}
			}

			if ok {
				return output[:len(output)-num], nil
			}
		}
	}

	return output, nil
}

// upstreamAuthSecrets is the negotiated state from AUTH Phase 1.
type upstreamAuthSecrets struct {
	verifierType    int
	customHash      bool
	salt            []byte
	pbkdf2ChkSalt   []byte
	pbkdf2VgenCount int
	pbkdf2SderCount int

	verifierKey      []byte // key used to encrypt session keys (16/24/32 bytes)
	speedyKey        []byte // 64-byte intermediate, only present for verifier 18453
	serverSessionKey []byte
	clientSessionKey []byte
	encClientSessKey string // hex
	encPassword      string // hex
	eSpeedyKey       string // hex (only for verifier 18453)
}

// computeUpstreamAuthSecrets runs the password-side derivation: it builds the
// verifier key, decrypts the server session key, generates a fresh client
// session key, encrypts it, derives a combined key, then encrypts the
// password (and optionally the speedy key for verifier 18453).
//
// Mirrors newAuthObject in go-ora/v2/auth_object.go for the post-AUTH-Phase-1
// portion. The portion that reads server-supplied fields off the wire is
// handled separately in parseAuthPhase1Response so this function can be
// tested with synthetic inputs.
func computeUpstreamAuthSecrets(password, encServerSessKey string, sec *upstreamAuthSecrets) error {
	padding := false

	switch sec.verifierType {
	case VerifierType6949:
		// caps[4]&2 is normally set on real 19c servers; when it is, padding=false.
		// We don't track that bit here — the value of padding for 6949 only affects
		// the wire-format trailer (encryptSessionKey output length). Real 19c
		// servers we tested set the bit, so default to padding=false.
		result, err := hex.DecodeString(strings.ToUpper(sec.encodedSalt()))
		if err != nil {
			return fmt.Errorf("decode salt: %w", err)
		}

		buf := append([]byte(password), result...)

		hash := sha1.New()
		hash.Write(buf)
		key := hash.Sum(nil)
		sec.verifierKey = append(key, 0, 0, 0, 0)

	case VerifierType18453:
		message := append(sec.salt, []byte(pbkdf2SpeedyKeyLabel)...)
		sec.speedyKey = pbkdf2SpeedyKey(message, []byte(password), sec.pbkdf2VgenCount)

		buf := append(sec.speedyKey, sec.salt...)

		hash := sha512.New()
		hash.Write(buf)

		full := hash.Sum(nil)
		sec.verifierKey = full[:32]

	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedVerifier, sec.verifierType)
	}

	encServerBytes, err := hex.DecodeString(encServerSessKey)
	if err != nil {
		return fmt.Errorf("decode server session key: %w", err)
	}

	serverKey, err := aesCBCDecryptZeroIV(sec.verifierKey, encServerBytes, padding)
	if err != nil {
		return fmt.Errorf("decrypt server session key: %w", err)
	}

	sec.serverSessionKey = serverKey

	clientKey := make([]byte, len(serverKey))
	for {
		if _, err := rand.Read(clientKey); err != nil {
			return fmt.Errorf("rand client session key: %w", err)
		}

		if !bytesEqual(clientKey, serverKey) {
			break
		}
	}

	sec.clientSessionKey = clientKey

	encClientBytes, err := aesCBCEncryptZeroIV(sec.verifierKey, clientKey, padding)
	if err != nil {
		return fmt.Errorf("encrypt client session key: %w", err)
	}

	sec.encClientSessKey = strings.ToUpper(hex.EncodeToString(encClientBytes))

	combined, err := derivePasswordEncKey(sec)
	if err != nil {
		return fmt.Errorf("derive password enc key: %w", err)
	}

	// AUTH_PASSWORD always uses padding=true on the wire (the encrypted blob
	// keeps its trailing pad block). go-ora's encryptPassword call passes true
	// regardless of the verifier type.
	encPwBytes, err := encryptOraclePassword([]byte(password), combined, true)
	if err != nil {
		return fmt.Errorf("encrypt password: %w", err)
	}

	sec.encPassword = strings.ToUpper(hex.EncodeToString(encPwBytes))

	if sec.verifierType == VerifierType18453 {
		// AUTH_PBKDF2_SPEEDY_KEY uses padding=false (truncate to original length).
		eSpeedyBytes, err := encryptOraclePassword(sec.speedyKey, combined, false)
		if err != nil {
			return fmt.Errorf("encrypt speedy key: %w", err)
		}

		sec.eSpeedyKey = strings.ToUpper(hex.EncodeToString(eSpeedyBytes))
	}

	return nil
}

// derivePasswordEncKey computes the AES key used to encrypt AUTH_PASSWORD.
// Mirrors AuthObject.generatePasswordEncKey for the customHash and non-customHash
// paths. dbbat only authenticates against modern Oracle (19c+), so customHash
// is the typical path; the non-customHash branch is included for completeness
// and to keep behavior aligned with go-ora.
func derivePasswordEncKey(sec *upstreamAuthSecrets) ([]byte, error) {
	if sec.customHash {
		var (
			joined []byte
			retLen int
		)

		switch sec.verifierType {
		case VerifierType6949:
			joined = append(append([]byte{}, sec.clientSessionKey[:24]...), sec.serverSessionKey[:24]...)
			retLen = 24
		case VerifierType18453:
			joined = append(append([]byte{}, sec.clientSessionKey...), sec.serverSessionKey...)
			retLen = 32
		default:
			return nil, fmt.Errorf("%w: %d", ErrUnsupportedVerifier, sec.verifierType)
		}

		hexBuf := strings.ToUpper(hex.EncodeToString(joined))

		full := pbkdf2SpeedyKey(sec.pbkdf2ChkSalt, []byte(hexBuf), sec.pbkdf2SderCount)

		return full[:retLen], nil
	}

	switch sec.verifierType {
	case VerifierType6949:
		const start = 16

		buf := make([]byte, 24)
		for i := 0; i < 24; i++ {
			buf[i] = sec.serverSessionKey[i+start] ^ sec.clientSessionKey[i+start]
		}

		h1 := md5.New()
		h1.Write(buf[:16])
		ret := h1.Sum(nil)

		h2 := md5.New()
		h2.Write(buf[16:])
		ret = append(ret, h2.Sum(nil)...)

		return ret[:24], nil
	default:
		return nil, fmt.Errorf("%w: %d (without customHash)", ErrUnsupportedVerifier, sec.verifierType)
	}
}

// encryptOraclePassword prepends 16 random bytes to the data, then encrypts.
func encryptOraclePassword(data, key []byte, keepPadding bool) ([]byte, error) {
	prefix := make([]byte, 16)
	if _, err := rand.Read(prefix); err != nil {
		return nil, fmt.Errorf("rand password prefix: %w", err)
	}

	return aesCBCEncryptZeroIV(key, append(prefix, data...), keepPadding)
}

// encodedSalt returns the salt as an uppercase hex string without re-decoding
// (used by the 6949 path which originally received the hex-encoded value).
func (s *upstreamAuthSecrets) encodedSalt() string {
	return strings.ToUpper(hex.EncodeToString(s.salt))
}

// bytesEqual reports whether a == b.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// ErrUnsupportedVerifier indicates an Oracle verifier type dbbat does not implement.
var ErrUnsupportedVerifier = errors.New("unsupported Oracle verifier type")
