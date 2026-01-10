package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun/dialect/pgdialect"
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

// DeleteUser deletes a user
func (s *Store) DeleteUser(ctx context.Context, uid uuid.UUID) error {
	result, err := s.db.NewDelete().
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
