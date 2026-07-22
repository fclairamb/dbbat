package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CLIAuthProvider namespaces oauth_states rows used for the generic CLI
// authorization (device-flow style) handshake, so they can share the table
// with real OAuth CSRF states without ever colliding: /auth/:provider routes
// are only registered for configured providers, never "cli".
const CLIAuthProvider = "cli"

// CLIAuthRequestTTL bounds how long a CLI authorization request stays valid.
const CLIAuthRequestTTL = 10 * time.Minute

// CLI authorization status values, stored inside OAuthState.Metadata.
const (
	CLIAuthStatusPending  = "pending"
	CLIAuthStatusApproved = "approved"
	CLIAuthStatusDenied   = "denied"
)

// CLI authorization errors.
var (
	ErrCLIAuthNotFound        = errors.New("CLI authorization request not found or expired")
	ErrCLIAuthAlreadyResolved = errors.New("CLI authorization request already responded to")
)

// cliAuthMetadata is the JSON shape stored in OAuthState.Metadata for
// provider=CLIAuthProvider rows.
type cliAuthMetadata struct {
	Name         string     `json:"name"`
	UserCode     string     `json:"user_code"`
	Status       string     `json:"status"`
	UserID       *uuid.UUID `json:"user_id,omitempty"`
	EncryptedKey []byte     `json:"encrypted_key,omitempty"`
	KeyPrefix    string     `json:"key_prefix,omitempty"`
}

// CLIAuthRequest is the store-level view of a pending or resolved CLI
// authorization request, without any secret material (poll token, encrypted
// key) — safe to hand to callers that only need to display or check status.
type CLIAuthRequest struct {
	UID       uuid.UUID
	Name      string
	UserCode  string
	Status    string
	ExpiresAt time.Time
}

// CreateCLIAuthRequest persists a new CLI authorization request. pollToken is
// the caller-generated secret that only the requesting CLI holds (it is
// never exposed to the browser); userCode is the short human-checkable code
// shown to both sides so the approving user can catch a mismatched request.
func (s *Store) CreateCLIAuthRequest(ctx context.Context, name, pollToken, userCode string) (*CLIAuthRequest, error) {
	meta := cliAuthMetadata{Name: name, UserCode: userCode, Status: CLIAuthStatusPending}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("failed to encode CLI auth metadata: %w", err)
	}

	state := &OAuthState{
		State:     pollToken,
		Provider:  CLIAuthProvider,
		Metadata:  encoded,
		ExpiresAt: time.Now().Add(CLIAuthRequestTTL),
	}

	if _, err := s.CreateOAuthState(ctx, state); err != nil {
		return nil, err
	}

	return &CLIAuthRequest{
		UID:       state.UID,
		Name:      name,
		UserCode:  userCode,
		Status:    CLIAuthStatusPending,
		ExpiresAt: state.ExpiresAt,
	}, nil
}

// getCLIAuthState fetches the raw (unexpired) row and its decoded metadata by
// public request UID.
func (s *Store) getCLIAuthState(ctx context.Context, uid uuid.UUID) (*OAuthState, *cliAuthMetadata, error) {
	state := new(OAuthState)
	err := s.db.NewSelect().
		Model(state).
		Where("uid = ?", uid).
		Where("provider = ?", CLIAuthProvider).
		Where("expires_at > NOW()").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrCLIAuthNotFound
		}
		return nil, nil, fmt.Errorf("failed to get CLI auth request: %w", err)
	}

	var meta cliAuthMetadata
	if err := json.Unmarshal(state.Metadata, &meta); err != nil {
		return nil, nil, fmt.Errorf("failed to decode CLI auth metadata: %w", err)
	}

	return state, &meta, nil
}

// GetCLIAuthRequest fetches a pending or resolved CLI authorization request
// by its public UID, for display on the approval page. Never exposes the
// poll token or the encrypted key.
func (s *Store) GetCLIAuthRequest(ctx context.Context, uid uuid.UUID) (*CLIAuthRequest, error) {
	state, meta, err := s.getCLIAuthState(ctx, uid)
	if err != nil {
		return nil, err
	}

	return &CLIAuthRequest{
		UID:       state.UID,
		Name:      meta.Name,
		UserCode:  meta.UserCode,
		Status:    meta.Status,
		ExpiresAt: state.ExpiresAt,
	}, nil
}

// RespondToCLIAuthRequest approves or denies a pending request. On approval,
// encryptedKey/keyPrefix are the minted dbb_ key's encrypted material,
// stashed until the CLI polls it exactly once. The update is conditioned on
// the request still being pending (and unexpired), so a request cannot be
// responded to twice — a losing concurrent responder gets
// ErrCLIAuthAlreadyResolved.
func (s *Store) RespondToCLIAuthRequest(ctx context.Context, uid, userID uuid.UUID, approve bool, encryptedKey []byte, keyPrefix string) error {
	_, meta, err := s.getCLIAuthState(ctx, uid)
	if err != nil {
		return err
	}
	if meta.Status != CLIAuthStatusPending {
		return ErrCLIAuthAlreadyResolved
	}

	meta.Status = CLIAuthStatusDenied
	if approve {
		meta.Status = CLIAuthStatusApproved
		meta.EncryptedKey = encryptedKey
		meta.KeyPrefix = keyPrefix
	}
	meta.UserID = &userID

	encoded, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to encode CLI auth metadata: %w", err)
	}

	result, err := s.db.NewUpdate().
		Model((*OAuthState)(nil)).
		Set("metadata = ?::jsonb", string(encoded)).
		Where("uid = ?", uid).
		Where("provider = ?", CLIAuthProvider).
		Where("expires_at > NOW()").
		Where("metadata->>'status' = ?", CLIAuthStatusPending).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update CLI auth request: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return ErrCLIAuthAlreadyResolved
	}

	return nil
}

// PollCLIAuthRequest looks up a request by its poll token. Terminal states
// (approved/denied) are consumed (the row is deleted) so the key material is
// delivered at most once; pending requests are left in place for the next
// poll. Returns ErrCLIAuthNotFound if the token is unknown or the request has
// expired.
func (s *Store) PollCLIAuthRequest(ctx context.Context, pollToken string) (*CLIAuthRequest, []byte, error) {
	state := new(OAuthState)
	err := s.db.NewSelect().
		Model(state).
		Where("state = ?", pollToken).
		Where("provider = ?", CLIAuthProvider).
		Where("expires_at > NOW()").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrCLIAuthNotFound
		}
		return nil, nil, fmt.Errorf("failed to get CLI auth request: %w", err)
	}

	var meta cliAuthMetadata
	if err := json.Unmarshal(state.Metadata, &meta); err != nil {
		return nil, nil, fmt.Errorf("failed to decode CLI auth metadata: %w", err)
	}

	req := &CLIAuthRequest{
		UID:       state.UID,
		Name:      meta.Name,
		UserCode:  meta.UserCode,
		Status:    meta.Status,
		ExpiresAt: state.ExpiresAt,
	}

	if meta.Status == CLIAuthStatusPending {
		return req, nil, nil
	}

	// Terminal state reached — deliver once and consume.
	if _, err := s.db.NewDelete().
		Model((*OAuthState)(nil)).
		Where("uid = ?", state.UID).
		ForceDelete().
		Exec(ctx); err != nil {
		return nil, nil, fmt.Errorf("failed to consume CLI auth request: %w", err)
	}

	return req, meta.EncryptedKey, nil
}
