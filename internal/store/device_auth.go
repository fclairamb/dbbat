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

// DeviceAuthProvider namespaces oauth_states rows used for the OAuth 2.0
// Device Authorization Grant (RFC 8628) handshake, so they can share the table
// with real OAuth CSRF states without ever colliding: /auth/:provider routes
// are only registered for configured providers, never "device".
const DeviceAuthProvider = "device"

// DeviceAuthTTL bounds how long a device authorization request stays valid
// (RFC 8628 expires_in).
const DeviceAuthTTL = 10 * time.Minute

// Device authorization status values, stored inside OAuthState.Metadata.
const (
	DeviceAuthStatusPending  = "pending"
	DeviceAuthStatusApproved = "approved"
	DeviceAuthStatusDenied   = "denied"
)

// Device authorization errors.
var (
	ErrDeviceAuthNotFound        = errors.New("device authorization request not found or expired")
	ErrDeviceAuthAlreadyResolved = errors.New("device authorization request already responded to")
	ErrDeviceAuthUserCodeTaken   = errors.New("user code already in use")
)

// deviceAuthMetadata is the JSON shape stored in OAuthState.Metadata for
// provider=DeviceAuthProvider rows. UserCode is the canonical (dashless,
// uppercase) form; the display form with a dash is derived by the API layer.
type deviceAuthMetadata struct {
	ClientName   string     `json:"client_name"`
	UserCode     string     `json:"user_code"`
	Status       string     `json:"status"`
	UserID       *uuid.UUID `json:"user_id,omitempty"`
	EncryptedKey []byte     `json:"encrypted_key,omitempty"`
	KeyPrefix    string     `json:"key_prefix,omitempty"`
}

// DeviceAuthRequest is the store-level view of a pending or resolved device
// authorization request, without any secret material (device code, encrypted
// key) — safe to hand to callers that only need to display or check status.
type DeviceAuthRequest struct {
	UID        uuid.UUID
	ClientName string
	UserCode   string // canonical (dashless, uppercase)
	Status     string
	ExpiresAt  time.Time
}

// CreateDeviceAuthRequest persists a new device authorization request.
// deviceCode is the caller-generated secret that only the requesting client
// holds (it is never exposed to the browser); userCode is the short
// human-checkable code the approving user enters/verifies in the browser,
// passed in canonical form. Returns ErrDeviceAuthUserCodeTaken if a live
// request already holds that user code, so the caller can regenerate.
func (s *Store) CreateDeviceAuthRequest(ctx context.Context, clientName, deviceCode, userCode string) (*DeviceAuthRequest, error) {
	taken, err := s.deviceAuthUserCodeExists(ctx, userCode)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, ErrDeviceAuthUserCodeTaken
	}

	meta := deviceAuthMetadata{ClientName: clientName, UserCode: userCode, Status: DeviceAuthStatusPending}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("failed to encode device auth metadata: %w", err)
	}

	state := &OAuthState{
		State:     deviceCode,
		Provider:  DeviceAuthProvider,
		Metadata:  encoded,
		ExpiresAt: time.Now().Add(DeviceAuthTTL),
	}

	if _, err := s.CreateOAuthState(ctx, state); err != nil {
		return nil, err
	}

	return &DeviceAuthRequest{
		UID:        state.UID,
		ClientName: clientName,
		UserCode:   userCode,
		Status:     DeviceAuthStatusPending,
		ExpiresAt:  state.ExpiresAt,
	}, nil
}

// deviceAuthUserCodeExists reports whether an unexpired request already holds
// the given canonical user code.
func (s *Store) deviceAuthUserCodeExists(ctx context.Context, userCode string) (bool, error) {
	count, err := s.db.NewSelect().
		Model((*OAuthState)(nil)).
		Where("provider = ?", DeviceAuthProvider).
		Where("metadata->>'user_code' = ?", userCode).
		Where("expires_at > NOW()").
		Count(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to check device auth user code: %w", err)
	}
	return count > 0, nil
}

// getDeviceAuthStateByUserCode fetches the raw (unexpired) row and its decoded
// metadata by canonical user code. In the astronomically unlikely event of a
// live collision, the most recent row wins.
func (s *Store) getDeviceAuthStateByUserCode(ctx context.Context, userCode string) (*OAuthState, *deviceAuthMetadata, error) {
	state := new(OAuthState)
	err := s.db.NewSelect().
		Model(state).
		Where("provider = ?", DeviceAuthProvider).
		Where("metadata->>'user_code' = ?", userCode).
		Where("expires_at > NOW()").
		Order("created_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrDeviceAuthNotFound
		}
		return nil, nil, fmt.Errorf("failed to get device auth request: %w", err)
	}

	var meta deviceAuthMetadata
	if err := json.Unmarshal(state.Metadata, &meta); err != nil {
		return nil, nil, fmt.Errorf("failed to decode device auth metadata: %w", err)
	}

	return state, &meta, nil
}

// GetDeviceAuthByUserCode fetches a pending or resolved device authorization
// request by its canonical user code, for display on the consent page. Never
// exposes the device code or the encrypted key.
func (s *Store) GetDeviceAuthByUserCode(ctx context.Context, userCode string) (*DeviceAuthRequest, error) {
	state, meta, err := s.getDeviceAuthStateByUserCode(ctx, userCode)
	if err != nil {
		return nil, err
	}

	return &DeviceAuthRequest{
		UID:        state.UID,
		ClientName: meta.ClientName,
		UserCode:   meta.UserCode,
		Status:     meta.Status,
		ExpiresAt:  state.ExpiresAt,
	}, nil
}

// RespondToDeviceAuthByUserCode approves or denies a pending request, keyed by
// canonical user code. On approval, encryptedKey/keyPrefix are the minted dbb_
// key's encrypted material, stashed until the client polls it exactly once.
// The update is conditioned (by internal uid) on the request still being
// pending and unexpired, so a request cannot be responded to twice — a losing
// concurrent responder gets ErrDeviceAuthAlreadyResolved.
func (s *Store) RespondToDeviceAuthByUserCode(ctx context.Context, userCode string, userID uuid.UUID, approve bool, encryptedKey []byte, keyPrefix string) error {
	state, meta, err := s.getDeviceAuthStateByUserCode(ctx, userCode)
	if err != nil {
		return err
	}
	if meta.Status != DeviceAuthStatusPending {
		return ErrDeviceAuthAlreadyResolved
	}

	meta.Status = DeviceAuthStatusDenied
	if approve {
		meta.Status = DeviceAuthStatusApproved
		meta.EncryptedKey = encryptedKey
		meta.KeyPrefix = keyPrefix
	}
	meta.UserID = &userID

	encoded, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to encode device auth metadata: %w", err)
	}

	result, err := s.db.NewUpdate().
		Model((*OAuthState)(nil)).
		Set("metadata = ?::jsonb", string(encoded)).
		Where("uid = ?", state.UID).
		Where("provider = ?", DeviceAuthProvider).
		Where("expires_at > NOW()").
		Where("metadata->>'status' = ?", DeviceAuthStatusPending).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update device auth request: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return ErrDeviceAuthAlreadyResolved
	}

	return nil
}

// PollDeviceAuthToken looks up a request by its device code (the client's
// secret). Terminal states (approved/denied) are consumed (the row is deleted)
// so the key material is delivered at most once; pending requests are left in
// place for the next poll. Returns ErrDeviceAuthNotFound if the device code is
// unknown or the request has expired.
func (s *Store) PollDeviceAuthToken(ctx context.Context, deviceCode string) (*DeviceAuthRequest, []byte, error) {
	state := new(OAuthState)
	err := s.db.NewSelect().
		Model(state).
		Where("state = ?", deviceCode).
		Where("provider = ?", DeviceAuthProvider).
		Where("expires_at > NOW()").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrDeviceAuthNotFound
		}
		return nil, nil, fmt.Errorf("failed to get device auth request: %w", err)
	}

	var meta deviceAuthMetadata
	if err := json.Unmarshal(state.Metadata, &meta); err != nil {
		return nil, nil, fmt.Errorf("failed to decode device auth metadata: %w", err)
	}

	req := &DeviceAuthRequest{
		UID:        state.UID,
		ClientName: meta.ClientName,
		UserCode:   meta.UserCode,
		Status:     meta.Status,
		ExpiresAt:  state.ExpiresAt,
	}

	if meta.Status == DeviceAuthStatusPending {
		return req, nil, nil
	}

	// Terminal state reached — deliver once and consume.
	if _, err := s.db.NewDelete().
		Model((*OAuthState)(nil)).
		Where("uid = ?", state.UID).
		ForceDelete().
		Exec(ctx); err != nil {
		return nil, nil, fmt.Errorf("failed to consume device auth request: %w", err)
	}

	return req, meta.EncryptedKey, nil
}
