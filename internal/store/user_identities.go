package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ListAdminSlackUserIDs returns the Slack user IDs (provider_id) of every
// user holding the admin role who has a linked Slack identity. Used by the
// grant-request notifier to @-mention approvers on the pending message.
//
// One query joins users carrying 'admin' in their roles array to their
// user_identities row for provider 'slack'. Soft-deleted users and
// identities are excluded (bun applies the soft-delete filter for the
// modeled UserIdentity; the users join is guarded explicitly).
func (s *Store) ListAdminSlackUserIDs(ctx context.Context) ([]string, error) {
	var slackIDs []string

	err := s.db.NewSelect().
		Model((*UserIdentity)(nil)).
		Column("ui.provider_id").
		Join("JOIN users AS u ON u.uid = ui.user_id").
		Where("ui.provider = ?", IdentityTypeSlack).
		Where("? = ANY(u.roles)", RoleAdmin).
		Where("u.deleted_at IS NULL").
		Scan(ctx, &slackIDs)
	if err != nil {
		return nil, fmt.Errorf("list admin slack user ids: %w", err)
	}

	if slackIDs == nil {
		slackIDs = []string{}
	}

	return slackIDs, nil
}

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

// GetIdentityByProviderID retrieves an identity row by (provider, provider_id) without joining the user.
// Use this when you need the identity uid itself rather than the associated User.
func (s *Store) GetIdentityByProviderID(ctx context.Context, provider, providerID string) (*UserIdentity, error) {
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
	return identity, nil
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
