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

// LoginExchangeProvider namespaces oauth_states rows that hold a freshly
// created web session waiting to be picked up by the browser, so they share
// the table with real OAuth CSRF states (and device authorization requests)
// without ever colliding: /auth/:provider routes are only registered for
// configured providers, never "login-exchange".
const LoginExchangeProvider = "login-exchange"

// LoginExchangeTTL bounds how long an unclaimed exchange code stays usable.
// The browser redeems it on the very next page load, so this is deliberately
// far shorter than the OAuth state TTL — it only has to survive one redirect.
const LoginExchangeTTL = 2 * time.Minute

// ErrLoginExchangeNotFound means the code is unknown, already redeemed, or
// expired. The three are deliberately indistinguishable to the caller.
var ErrLoginExchangeNotFound = errors.New("login exchange code not found or expired")

// loginExchangeMetadata is the JSON shape stored in OAuthState.Metadata for
// provider=LoginExchangeProvider rows. The session token never lands in the
// database in the clear: it is AES-GCM encrypted and AAD-bound to the row UID
// by the API layer, exactly like a pending device authorization key.
type loginExchangeMetadata struct {
	UserID       uuid.UUID `json:"user_id"`
	EncryptedKey []byte    `json:"encrypted_key"`
}

// CreateLoginExchange parks an encrypted web session token behind a one-time
// code. The row UID is generated here (rather than by the database default) so
// the caller can bind the ciphertext's AAD to it before the insert.
func (s *Store) CreateLoginExchange(ctx context.Context, exchangeUID uuid.UUID, code string, userID uuid.UUID, encryptedKey []byte) error {
	encoded, err := json.Marshal(loginExchangeMetadata{UserID: userID, EncryptedKey: encryptedKey})
	if err != nil {
		return fmt.Errorf("failed to encode login exchange metadata: %w", err)
	}

	state := &OAuthState{
		UID:      exchangeUID,
		State:    code,
		Provider: LoginExchangeProvider,
		Metadata: encoded,
	}

	// Database-stamped expiry: ConsumeLoginExchange filters on
	// `expires_at > NOW()`, so a process-stamped one would make this two-minute
	// window `2m ± skew` — long enough, on a process ahead of its store, to
	// keep a session token redeemable past its intended life.
	if _, err := s.CreateOAuthState(ctx, state, LoginExchangeTTL); err != nil {
		return err
	}

	return nil
}

// ConsumeLoginExchange redeems a code exactly once: the row is deleted as part
// of the read, so a replay (or a second tab racing the first) gets
// ErrLoginExchangeNotFound. Returns the row UID so the caller can rebuild the
// decryption AAD, the owning user, and the encrypted session token.
func (s *Store) ConsumeLoginExchange(ctx context.Context, code string) (uuid.UUID, uuid.UUID, []byte, error) {
	state := new(OAuthState)
	err := s.db.NewSelect().
		Model(state).
		Where("state = ?", code).
		Where("provider = ?", LoginExchangeProvider).
		Where("expires_at > NOW()").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, uuid.Nil, nil, ErrLoginExchangeNotFound
		}
		return uuid.Nil, uuid.Nil, nil, fmt.Errorf("failed to get login exchange code: %w", err)
	}

	// Delete conditioned on the uid: the loser of a concurrent redemption
	// affects zero rows and is told the code is gone, so the token is handed
	// out at most once.
	result, err := s.db.NewDelete().
		Model((*OAuthState)(nil)).
		Where("uid = ?", state.UID).
		ForceDelete().
		Exec(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, nil, fmt.Errorf("failed to consume login exchange code: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return uuid.Nil, uuid.Nil, nil, fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return uuid.Nil, uuid.Nil, nil, ErrLoginExchangeNotFound
	}

	var meta loginExchangeMetadata
	if err := json.Unmarshal(state.Metadata, &meta); err != nil {
		return uuid.Nil, uuid.Nil, nil, fmt.Errorf("failed to decode login exchange metadata: %w", err)
	}

	return state.UID, meta.UserID, meta.EncryptedKey, nil
}
