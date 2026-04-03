package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// GetUserByIdentity retrieves a user by their external identity (provider + provider_id).
func (s *Store) GetUserByIdentity(ctx context.Context, provider, providerID string) (*User, error) {
	identity := new(UserIdentity)
	err := s.db.NewSelect().
		Model(identity).
		Where("provider = ?", provider).
		Where("provider_id = ?", providerID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrIdentityNotFound
		}
		return nil, fmt.Errorf("failed to get identity: %w", err)
	}

	user := new(User)
	err = s.db.NewSelect().
		Model(user).
		Where("uid = ?", identity.UserID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	return user, nil
}

// GetUserIdentity retrieves a single user identity by UID.
func (s *Store) GetUserIdentity(ctx context.Context, uid uuid.UUID) (*UserIdentity, error) {
	identity := new(UserIdentity)
	err := s.db.NewSelect().
		Model(identity).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrIdentityNotFound
		}
		return nil, fmt.Errorf("failed to get identity: %w", err)
	}
	return identity, nil
}

// GetUserIdentities retrieves all identities for a given user.
func (s *Store) GetUserIdentities(ctx context.Context, userID uuid.UUID) ([]UserIdentity, error) {
	var identities []UserIdentity
	err := s.db.NewSelect().
		Model(&identities).
		Where("user_id = ?", userID).
		Order("created_at ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list identities: %w", err)
	}
	if identities == nil {
		identities = []UserIdentity{}
	}
	return identities, nil
}

// CreateUserIdentity creates a new user identity link.
func (s *Store) CreateUserIdentity(ctx context.Context, identity *UserIdentity) (*UserIdentity, error) {
	identity.CreatedAt = time.Now()
	identity.UpdatedAt = time.Now()

	_, err := s.db.NewInsert().
		Model(identity).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create identity: %w", err)
	}

	return identity, nil
}

// DeleteUserIdentity soft-deletes a user identity.
func (s *Store) DeleteUserIdentity(ctx context.Context, uid uuid.UUID) error {
	result, err := s.db.NewDelete().
		Model((*UserIdentity)(nil)).
		Where("uid = ?", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to delete identity: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrIdentityNotFound
	}

	return nil
}
