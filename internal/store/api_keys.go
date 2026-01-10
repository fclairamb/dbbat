package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// API key constants
const (
	// APIKeyPrefix is the prefix for regular API keys
	APIKeyPrefix = "dbb_"
	// WebKeyPrefix is the prefix for web session keys
	WebKeyPrefix = "web_"
	// APIKeyRandomLength is the length of the random part of the key
	APIKeyRandomLength = 32
	// APIKeyPrefixLength is the length of the prefix stored for identification
	APIKeyPrefixLength = 8
	// WebSessionMaxDuration is the maximum duration for web sessions (1 hour)
	WebSessionMaxDuration = time.Hour
)

// API key errors
var (
	ErrAPIKeyNotFound = errors.New("API key not found")
	ErrAPIKeyRevoked  = errors.New("API key has been revoked")
	ErrAPIKeyExpired  = errors.New("API key has expired")
)

// generateKey generates a new random key with the given prefix
// Returns the full key (prefix_<32chars>) and the key prefix (first 8 chars for identification)
func generateKey(keyPrefix string) (string, string, error) {
	// Generate random bytes for the key
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	randomPart := make([]byte, APIKeyRandomLength)

	randomBytes := make([]byte, APIKeyRandomLength)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	for i := range randomPart {
		randomPart[i] = charset[randomBytes[i]%byte(len(charset))]
	}

	fullKey := keyPrefix + string(randomPart)
	prefix := fullKey[:APIKeyPrefixLength]

	return fullKey, prefix, nil
}

// generateAPIKey generates a new random API key (dbb_ prefix)
func generateAPIKey() (string, string, error) {
	return generateKey(APIKeyPrefix)
}

// generateWebKey generates a new random web session key (web_ prefix)
func generateWebKey() (string, string, error) {
	return generateKey(WebKeyPrefix)
}

// CreateAPIKey creates a new API key for a user
// Returns the created APIKey and the plain text key (only shown once)
func (s *Store) CreateAPIKey(ctx context.Context, userID uuid.UUID, name string, expiresAt *time.Time) (*APIKey, string, error) {
	// Generate the key
	plainKey, prefix, err := generateAPIKey()
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate API key: %w", err)
	}

	// Hash the key using the same method as passwords
	keyHash, err := crypto.HashPassword(plainKey)
	if err != nil {
		return nil, "", fmt.Errorf("failed to hash API key: %w", err)
	}

	apiKey := &APIKey{
		UserID:    userID,
		Name:      name,
		KeyHash:   keyHash,
		KeyPrefix: prefix,
		KeyType:   KeyTypeAPI,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
	}

	_, err = s.db.NewInsert().
		Model(apiKey).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create API key: %w", err)
	}

	return apiKey, plainKey, nil
}

// CreateWebSession creates a new web session key for a user
// Web sessions have a fixed 1-hour expiration and use the web_ prefix
// Returns the created APIKey and the plain text key (only shown once)
func (s *Store) CreateWebSession(ctx context.Context, userID uuid.UUID) (*APIKey, string, error) {
	// Generate the key
	plainKey, prefix, err := generateWebKey()
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate web session key: %w", err)
	}

	// Hash the key using the same method as passwords
	keyHash, err := crypto.HashPassword(plainKey)
	if err != nil {
		return nil, "", fmt.Errorf("failed to hash web session key: %w", err)
	}

	expiresAt := time.Now().Add(WebSessionMaxDuration)
	apiKey := &APIKey{
		UserID:    userID,
		Name:      "Web Session",
		KeyHash:   keyHash,
		KeyPrefix: prefix,
		KeyType:   KeyTypeWeb,
		ExpiresAt: &expiresAt,
		CreatedAt: time.Now(),
	}

	_, err = s.db.NewInsert().
		Model(apiKey).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create web session: %w", err)
	}

	return apiKey, plainKey, nil
}

// GetAPIKeyByPrefix retrieves all API keys with a given prefix
// Since prefix is unique, this returns at most one key
func (s *Store) GetAPIKeyByPrefix(ctx context.Context, prefix string) (*APIKey, error) {
	apiKey := new(APIKey)
	err := s.db.NewSelect().
		Model(apiKey).
		Where("key_prefix = ?", prefix).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAPIKeyNotFound
		}
		return nil, fmt.Errorf("failed to get API key: %w", err)
	}
	return apiKey, nil
}

// GetAPIKeyByID retrieves an API key by its ID
func (s *Store) GetAPIKeyByID(ctx context.Context, id uuid.UUID) (*APIKey, error) {
	apiKey := new(APIKey)
	err := s.db.NewSelect().
		Model(apiKey).
		Where("id = ?", id).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAPIKeyNotFound
		}
		return nil, fmt.Errorf("failed to get API key: %w", err)
	}
	return apiKey, nil
}

// VerifyAPIKey verifies a plain text API key and returns the associated key record
// It checks that the key exists, is not revoked, and is not expired
func (s *Store) VerifyAPIKey(ctx context.Context, plainKey string) (*APIKey, error) {
	// Validate key format
	if len(plainKey) < APIKeyPrefixLength {
		return nil, ErrAPIKeyNotFound
	}

	prefix := plainKey[:APIKeyPrefixLength]

	// Get the key by prefix
	apiKey, err := s.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}

	// Verify the key hash
	valid, err := crypto.VerifyPassword(apiKey.KeyHash, plainKey)
	if err != nil || !valid {
		return nil, ErrAPIKeyNotFound
	}

	// Check if revoked
	if apiKey.IsRevoked() {
		return nil, ErrAPIKeyRevoked
	}

	// Check if expired
	if apiKey.IsExpired() {
		return nil, ErrAPIKeyExpired
	}

	return apiKey, nil
}

// ListAPIKeys retrieves API keys with optional filters
func (s *Store) ListAPIKeys(ctx context.Context, filter APIKeyFilter) ([]APIKey, error) {
	var keys []APIKey
	q := s.db.NewSelect().Model(&keys)

	if filter.UserID != nil {
		q = q.Where("user_id = ?", *filter.UserID)
	}

	if filter.KeyType != nil {
		q = q.Where("key_type = ?", *filter.KeyType)
	}

	if !filter.IncludeAll {
		// Only include active keys (not revoked and not expired)
		q = q.Where("revoked_at IS NULL")
		q = q.Where("(expires_at IS NULL OR expires_at > ?)", time.Now())
	}

	q = q.Order("created_at DESC")

	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}

	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}

	err := q.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}

	if keys == nil {
		keys = []APIKey{}
	}
	return keys, nil
}

// RevokeAPIKey revokes an API key
func (s *Store) RevokeAPIKey(ctx context.Context, id uuid.UUID, revokedBy uuid.UUID) error {
	now := time.Now()
	result, err := s.db.NewUpdate().
		Model((*APIKey)(nil)).
		Set("revoked_at = ?", now).
		Set("revoked_by = ?", revokedBy).
		Where("id = ?", id).
		Where("revoked_at IS NULL"). // Only revoke if not already revoked
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to revoke API key: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrAPIKeyNotFound
	}

	return nil
}

// IncrementAPIKeyUsage updates the last_used_at and increments request_count
func (s *Store) IncrementAPIKeyUsage(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.NewUpdate().
		Model((*APIKey)(nil)).
		Set("last_used_at = ?", time.Now()).
		Set("request_count = request_count + 1").
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update API key usage: %w", err)
	}
	return nil
}
