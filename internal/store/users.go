package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// CreateUser creates a new user with the specified roles
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, roles []string) (*User, error) {
	// Default to connector role if no roles specified
	if len(roles) == 0 {
		roles = []string{RoleConnector}
	}

	user := &User{
		Username:     username,
		PasswordHash: passwordHash,
		Roles:        roles,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	_, err := s.db.NewInsert().
		Model(user).
		Returning("*").
		Exec(ctx)
	if err != nil {
		if isUniqueViolation(err, "users_username_active_uq") {
			return nil, ErrUserNameConflict
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return user, nil
}

// GetUserByUsername retrieves a user by username
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	user := new(User)
	err := s.db.NewSelect().
		Model(user).
		Where("username = ?", username).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	return user, nil
}

// GetUserByUID retrieves a user by UID
func (s *Store) GetUserByUID(ctx context.Context, uid uuid.UUID) (*User, error) {
	user := new(User)
	err := s.db.NewSelect().
		Model(user).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	return user, nil
}

// ListUsers retrieves all users
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	var users []User
	err := s.db.NewSelect().
		Model(&users).
		Order("username ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	if users == nil {
		users = []User{}
	}
	return users, nil
}

// CountAdmins returns the number of users holding the admin role
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	count, err := s.db.NewSelect().
		Model((*User)(nil)).
		Where("? = ANY(roles)", RoleAdmin).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to count admin users: %w", err)
	}
	return count, nil
}

// UpdateUser updates a user
func (s *Store) UpdateUser(ctx context.Context, uid uuid.UUID, updates UserUpdate) error {
	q := s.db.NewUpdate().
		Model((*User)(nil)).
		Where("uid = ?", uid).
		Set("updated_at = ?", time.Now())

	if updates.PasswordHash != nil {
		q = q.Set("password_hash = ?", *updates.PasswordHash)
		q = q.Set("password_changed_at = ?", time.Now())
	}

	if updates.Roles != nil {
		q = q.Set("roles = ?", updates.Roles)
	}

	result, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrUserNotFound
	}

	return nil
}

// DeleteUser deletes a user and all of their linked OAuth identities.
func (s *Store) DeleteUser(ctx context.Context, uid uuid.UUID) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().
			Model((*UserIdentity)(nil)).
			Where("user_id = ?", uid).
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to delete user identities: %w", err)
		}

		result, err := tx.NewDelete().
			Model((*User)(nil)).
			Where("uid = ?", uid).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to delete user: %w", err)
		}

		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected: %w", err)
		}

		if rowsAffected == 0 {
			return ErrUserNotFound
		}

		return nil
	})
}

// EnsureUserOracleSalts returns the user's shared O5LOGON salts, generating and
// persisting them lazily on first use (typically at API key creation). All of a
// user's API keys derive their O5LOGON verifiers from these salts so the Oracle
// proxy can commit to one salt in the AUTH challenge and still accept any of
// the user's keys as the password.
//
// Concurrency-safe: the persist is a compare-and-set (only writes when the
// oracle material is still absent), and on a lost race the winner's salts are
// re-read so both callers converge on the same values.
func (s *Store) EnsureUserOracleSalts(ctx context.Context, userID uuid.UUID) (*OracleUserData, error) {
	user, err := s.GetUserByUID(ctx, userID)
	if err != nil {
		return nil, err
	}

	if data := user.OracleData(); data != nil &&
		len(data.O5LogonUserSalt6949) > 0 && len(data.O5LogonUserSalt18453) > 0 {
		return data, nil
	}

	salt6949 := make([]byte, crypto.O5LogonSaltLength)
	if _, err := rand.Read(salt6949); err != nil {
		return nil, fmt.Errorf("failed to generate user O5LOGON salt: %w", err)
	}

	salt18453 := make([]byte, crypto.O5LogonPbkdf2SaltLength)
	if _, err := rand.Read(salt18453); err != nil {
		return nil, fmt.Errorf("failed to generate user O5LOGON PBKDF2 salt: %w", err)
	}

	protocolData := user.ProtocolData
	if protocolData == nil {
		protocolData = &UserProtocolData{}
	}

	protocolData.Oracle = &OracleUserData{
		O5LogonUserSalt6949:  salt6949,
		O5LogonUserSalt18453: salt18453,
	}

	encoded, err := json.Marshal(protocolData)
	if err != nil {
		return nil, fmt.Errorf("failed to encode user protocol data: %w", err)
	}

	res, err := s.db.NewUpdate().
		Model((*User)(nil)).
		Set("protocol_data = ?::jsonb", string(encoded)).
		Set("updated_at = ?", time.Now()).
		Where("uid = ?", userID).
		Where("protocol_data IS NULL OR protocol_data -> 'oracle' IS NULL").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to persist user O5LOGON salts: %w", err)
	}

	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		// Lost a concurrent-generation race — adopt the winner's salts.
		winner, err := s.GetUserByUID(ctx, userID)
		if err != nil {
			return nil, err
		}

		if data := winner.OracleData(); data != nil {
			return data, nil
		}

		return nil, ErrUserNotFound
	}

	return protocolData.Oracle, nil
}

// EnsureDefaultAdmin creates a default admin user if no users exist
func (s *Store) EnsureDefaultAdmin(ctx context.Context, passwordHash string) error {
	// Check if any users exist
	count, err := s.db.NewSelect().
		Model((*User)(nil)).
		Count(ctx)
	if err != nil {
		return fmt.Errorf("failed to count users: %w", err)
	}

	// If users exist, nothing to do
	if count > 0 {
		return nil
	}

	// Create default admin user with admin and connector roles
	_, err = s.CreateUser(ctx, "admin", passwordHash, []string{RoleAdmin, RoleConnector})
	if err != nil {
		return fmt.Errorf("failed to create default admin: %w", err)
	}

	return nil
}

// TouchUserLastLogin stamps users.last_login_at with the current instant.
//
// Called only from the *interactive* login paths (password login, the OAuth
// callback, the OAuth exchange) — see User.LastLoginAt for why nothing else
// may call it.
//
// A targeted UPDATE rather than a model write: last_login_at is the only
// column this is allowed to touch, and it deliberately leaves updated_at
// alone, which tracks administrative edits to the account and would otherwise
// be moved by every sign-in.
//
// A UID that names no row is not an error: the caller has already
// authenticated somebody, and failing a login because the bookkeeping write
// found nothing to update would be strictly worse than a stale timestamp.
func (s *Store) TouchUserLastLogin(ctx context.Context, uid uuid.UUID) error {
	_, err := s.db.NewUpdate().
		Model((*User)(nil)).
		Set("last_login_at = ?", time.Now()).
		Where("uid = ?", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to stamp last login: %w", err)
	}

	return nil
}

// UserLastConnection is when one user last opened a proxy session, as returned
// by ListLastConnectionPerUser.
type UserLastConnection struct {
	UserID      uuid.UUID `bun:"user_id" json:"user_id"`
	Username    string    `bun:"username" json:"username"`
	ConnectedAt time.Time `bun:"connected_at" json:"connected_at"`
}

// ListLastConnectionPerUser returns the most recent proxy connection of every
// user that has one — one row per user, computed in the database.
//
// Deliberately not derived from a page of the connections listing: on a busy
// instance a user whose last session fell outside such a page would render as
// "never connected", which is the same thing shown for someone who genuinely
// never has. Absence from this response is therefore exact.
//
// It reads "last connection *still on record*", not "last connection ever":
// connection rows are deletable and DBB_QUERY_STORAGE_RETENTION's sibling
// sweeps reap them, so a user whose sessions have all aged out reports as
// never having connected. That is the correct reading of the stored evidence,
// and the reason the column is labeled from the record rather than from
// history.
//
// Served by idx_connections_user_connected_at.
func (s *Store) ListLastConnectionPerUser(ctx context.Context) ([]UserLastConnection, error) {
	rows := []UserLastConnection{}

	err := s.db.NewSelect().
		Model((*Connection)(nil)).
		DistinctOn("c.user_id").
		ColumnExpr("c.user_id, c.connected_at").
		ColumnExpr("u.username").
		Join("JOIN users AS u ON u.uid = c.user_id AND u.deleted_at IS NULL").
		Order("c.user_id", "c.connected_at DESC").
		Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("failed to list the last connection per user: %w", err)
	}

	return rows, nil
}
