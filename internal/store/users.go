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
	"github.com/uptrace/bun/dialect/pgdialect"

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
		q = q.Set("roles = ?", pgdialect.Array(updates.Roles))
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
