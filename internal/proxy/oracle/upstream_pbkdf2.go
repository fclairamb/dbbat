package oracle

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// upstreamAuthChallenge holds parsed AUTH challenge KV pairs from upstream Oracle.
type upstreamAuthChallenge struct {
	sessKey         string
	salt            string
	pbkdf2CskSalt   string
	pbkdf2VgenCount int
	pbkdf2SderCount int
}

// pbkdf2AuthResponse holds the encrypted values for AUTH Phase 2.
type pbkdf2AuthResponse struct {
	encClientSessKey string
	encPassword      string
	encSpeedyKey     string
}

// parseUpstreamAuthKVPairs extracts all AUTH_ fields from an upstream challenge response.
func parseUpstreamAuthKVPairs(tnsDataPayload []byte) upstreamAuthChallenge {
	var c upstreamAuthChallenge

	if len(tnsDataPayload) < ttcDataFlagsSize+3 {
		return c
	}

	pairs := scanTTCKeyValPairs(tnsDataPayload[ttcDataFlagsSize+2:])

	for _, p := range pairs {
		switch strings.ToUpper(p.Key) {
		case "AUTH_SESSKEY":
			c.sessKey = p.Value
		case "AUTH_VFR_DATA":
			c.salt = p.Value
		case "AUTH_PBKDF2_CSK_SALT":
			c.pbkdf2CskSalt = p.Value
		case "AUTH_PBKDF2_VGEN_COUNT":
			c.pbkdf2VgenCount, _ = strconv.Atoi(p.Value)
		case "AUTH_PBKDF2_SDER_COUNT":
			c.pbkdf2SderCount, _ = strconv.Atoi(p.Value)
		}
	}

	if c.pbkdf2VgenCount < 4096 {
		c.pbkdf2VgenCount = 4096
	}

	if c.pbkdf2SderCount < 3 {
		c.pbkdf2SderCount = 3
	}

	return c
}

// generatePBKDF2ClientResponse implements the Oracle 12c/18453 PBKDF2 auth client-side.
// This matches go-ora's auth_object.go for verifierType == 18453.
func generatePBKDF2ClientResponse(password string, ch *upstreamAuthChallenge) (*pbkdf2AuthResponse, error) {
	// 1. Derive verifier key from password + salt using PBKDF2-HMAC-SHA512
	salt, err := hex.DecodeString(ch.salt)
	if err != nil {
		return nil, fmt.Errorf("failed to decode salt: %w", err)
	}

	message := append(salt, []byte("AUTH_PBKDF2_SPEEDY_KEY")...)
	speedyKey := generateSpeedyKey(message, []byte(password), ch.pbkdf2VgenCount)

	// key = SHA512(speedyKey + salt)[:32]
	h := sha512.New()
	h.Write(append(speedyKey, salt...))
	key := h.Sum(nil)[:32]

	// 2. Decrypt server session key (AES-256-CBC, no padding for 18453)
	serverSessKey, err := aes256CBCDecrypt(key, ch.sessKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt server session key: %w", err)
	}

	// 3. Generate client session key (same length as server)
	clientSessKey := make([]byte, len(serverSessKey))
	if _, err := rand.Read(clientSessKey); err != nil {
		return nil, fmt.Errorf("failed to generate client session key: %w", err)
	}

	// 4. Encrypt client session key
	encClientSessKey, err := aes256CBCEncrypt(key, clientSessKey)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt client session key: %w", err)
	}

	// 5. Derive combined key (customHash path for 18453)
	// buffer = client_key + server_key, keyBuffer = hex(buffer)
	buffer := make([]byte, 0, len(clientSessKey)+len(serverSessKey))
	buffer = append(buffer, clientSessKey...)
	buffer = append(buffer, serverSessKey...)
	keyBuffer := fmt.Sprintf("%X", buffer)

	// Combined key via PBKDF2 with pbkdf2_csk_salt
	df2key, err := hex.DecodeString(ch.pbkdf2CskSalt)
	if err != nil {
		return nil, fmt.Errorf("failed to decode PBKDF2 CSK salt: %w", err)
	}

	combinedKey := generateSpeedyKey(df2key, []byte(keyBuffer), ch.pbkdf2SderCount)[:32]

	// 6. Encrypt password (with random prefix, no padding for 18453)
	encPassword, err := encryptOraclePassword([]byte(password), combinedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt password: %w", err)
	}

	// 7. Encrypt speedy key (no padding for 18453)
	encSpeedyKey, err := encryptOraclePassword(speedyKey, combinedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt speedy key: %w", err)
	}

	return &pbkdf2AuthResponse{
		encClientSessKey: strings.ToUpper(hex.EncodeToString(encClientSessKey)),
		encPassword:      strings.ToUpper(hex.EncodeToString(encPassword)),
		encSpeedyKey:     strings.ToUpper(hex.EncodeToString(encSpeedyKey)),
	}, nil
}

// generateSpeedyKey implements PBKDF2-HMAC-SHA512 matching go-ora's generateSpeedyKey.
func generateSpeedyKey(buffer, key []byte, turns int) []byte {
	mac := hmac.New(sha512.New, key)
	mac.Write(append(buffer, 0, 0, 0, 1))
	firstHash := mac.Sum(nil)

	tempHash := make([]byte, len(firstHash))
	copy(tempHash, firstHash)

	for i := 2; i <= turns; i++ {
		mac.Reset()
		mac.Write(tempHash)
		tempHash = mac.Sum(nil)

		for j := range 64 {
			firstHash[j] ^= tempHash[j]
		}
	}

	return firstHash
}

// aes256CBCDecrypt decrypts hex-encoded data using AES-256-CBC with zero IV.
func aes256CBCDecrypt(key []byte, hexData string) ([]byte, error) {
	data, err := hex.DecodeString(hexData)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	if len(data)%aes.BlockSize != 0 {
		return nil, ErrCiphertextNotAligned
	}

	iv := make([]byte, aes.BlockSize)
	mode := cipher.NewCBCDecrypter(block, iv)
	plaintext := make([]byte, len(data))
	mode.CryptBlocks(plaintext, data)

	return plaintext, nil
}

// aes256CBCEncrypt encrypts data using AES-256-CBC with zero IV, returns raw ciphertext.
func aes256CBCEncrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// Pad to block size
	padded := pkcs7Pad(plaintext, aes.BlockSize)

	iv := make([]byte, aes.BlockSize)
	mode := cipher.NewCBCEncrypter(block, iv)
	ciphertext := make([]byte, len(padded))
	mode.CryptBlocks(ciphertext, padded)

	// For 18453, return originalLen bytes (truncated like go-ora)
	return ciphertext[:len(plaintext)], nil
}

// encryptOraclePassword encrypts a password/key with random prefix for Oracle AUTH.
func encryptOraclePassword(data, key []byte) ([]byte, error) {
	prefix := make([]byte, 16)
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}

	payload := make([]byte, 0, len(prefix)+len(data))
	payload = append(payload, prefix...)
	payload = append(payload, data...)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	padded := pkcs7Pad(payload, aes.BlockSize)

	iv := make([]byte, aes.BlockSize)
	mode := cipher.NewCBCEncrypter(block, iv)
	ciphertext := make([]byte, len(padded))
	mode.CryptBlocks(ciphertext, padded)

	return ciphertext, nil
}

// checkUpstreamAuthResponse checks if the upstream AUTH response indicates success.
func checkUpstreamAuthResponse(payload []byte) error {
	if len(payload) <= ttcDataFlagsSize+2 {
		return nil
	}

	funcCode := payload[ttcDataFlagsSize]
	if funcCode != byte(TTCFuncResponse) {
		return nil
	}

	if len(payload) <= ttcDataFlagsSize+3 {
		return nil
	}

	retCode := payload[ttcDataFlagsSize+2]
	if retCode != 0 {
		return ErrAuthFailed
	}

	return nil
}

// buildUpstreamAuthPhase2 builds the AUTH Phase 2 message for PBKDF2 auth.
func buildUpstreamAuthPhase2(username string, resp *pbkdf2AuthResponse) []byte {
	u := []byte(username)
	buf := make([]byte, 0, 512)

	// Header: data_flags(2) + func(0x03) + sub(0x73=AUTH Phase 2)
	buf = append(buf, 0x00, 0x00, 0x03, 0x73)

	// Preamble (same structure as Phase 1 but with sub=0x73)
	buf = append(buf, 0x02, 0x01, 0x01, 0x01, 0x03, 0x02, 0x01, 0x01)
	buf = append(buf, 0x01, 0x01, 0x0e, 0x01, 0x01)
	buf = append(buf, byte(len(u)))
	buf = append(buf, u...)

	// KV pairs: AUTH_SESSKEY, AUTH_PASSWORD, AUTH_PBKDF2_SPEEDY_KEY
	kvs := []struct{ k, v string }{
		{"AUTH_SESSKEY", resp.encClientSessKey},
		{"AUTH_PASSWORD", resp.encPassword},
		{"AUTH_PBKDF2_SPEEDY_KEY", resp.encSpeedyKey},
	}

	for _, kv := range kvs {
		buf = append(buf, ttcCompressedUint(uint64(len(kv.k)))...)
		buf = append(buf, ttcClr([]byte(kv.k))...)
		buf = append(buf, ttcCompressedUint(uint64(len(kv.v)))...)
		buf = append(buf, ttcClr([]byte(kv.v))...)
		buf = append(buf, 0x00) // flag
	}

	return buf
}
