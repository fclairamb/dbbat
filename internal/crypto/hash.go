package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// DefaultArgon2Time is the default number of iterations.
const DefaultArgon2Time uint32 = 1

// DefaultArgon2Memory is the default memory in KB (64 MB).
const DefaultArgon2Memory uint32 = 64 * 1024

// DefaultArgon2Threads is the default parallelism factor.
const DefaultArgon2Threads uint8 = 4

const (
	argon2KeyLen = 32
	saltLength   = 16
)

// Hash errors.
var (
	ErrInvalidHashFormat   = errors.New("invalid hash format")
	ErrUnsupportedHashAlgo = errors.New("unsupported hash algorithm")
)

// HashParams holds configurable parameters for password hashing.
type HashParams struct {
	MemoryKB uint32 // Memory in KB
	Time     uint32 // Number of iterations
	Threads  uint8  // Parallelism factor
}

// DefaultHashParams returns the default hash parameters.
func DefaultHashParams() HashParams {
	return HashParams{
		MemoryKB: DefaultArgon2Memory,
		Time:     DefaultArgon2Time,
		Threads:  DefaultArgon2Threads,
	}
}

// HashPassword generates an Argon2id hash of the password using default parameters.
func HashPassword(password string) (string, error) {
	return HashPasswordWithParams(password, DefaultHashParams())
}

// HashPasswordWithParams generates an Argon2id hash of the password using provided parameters.
func HashPasswordWithParams(password string, params HashParams) (string, error) {
	// Generate a random salt
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("failed to generate salt: %w", err)
	}

	// Generate the hash
	hash := argon2.IDKey(
		[]byte(password),
		salt,
		params.Time,
		params.MemoryKB,
		params.Threads,
		argon2KeyLen,
	)

	// Encode as: $argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>
	encodedSalt := base64.RawStdEncoding.EncodeToString(salt)
	encodedHash := base64.RawStdEncoding.EncodeToString(hash)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		params.MemoryKB,
		params.Time,
		params.Threads,
		encodedSalt,
		encodedHash,
	), nil
}

// VerifyPassword verifies a password against an Argon2id hash.
func VerifyPassword(encodedHash, password string) (bool, error) {
	// Parse the encoded hash
	parts := strings.Split(encodedHash, "$")

	const expectedParts = 6
	if len(parts) != expectedParts {
		return false, ErrInvalidHashFormat
	}

	if parts[1] != "argon2id" {
		return false, ErrUnsupportedHashAlgo
	}

	// Parse parameters
	var version int

	var memory, time uint32

	var threads uint8

	_, err := fmt.Sscanf(parts[2], "v=%d", &version)
	if err != nil {
		return false, fmt.Errorf("failed to parse version: %w", err)
	}

	_, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads)
	if err != nil {
		return false, fmt.Errorf("failed to parse parameters: %w", err)
	}

	// Decode salt and hash
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("failed to decode salt: %w", err)
	}

	expectedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("failed to decode hash: %w", err)
	}

	// Compute hash with provided password
	computedHash := argon2.IDKey(
		[]byte(password),
		salt,
		time,
		memory,
		threads,
		uint32(len(expectedHash)),
	)

	// Constant-time comparison
	if subtle.ConstantTimeCompare(computedHash, expectedHash) == 1 {
		return true, nil
	}

	return false, nil
}
