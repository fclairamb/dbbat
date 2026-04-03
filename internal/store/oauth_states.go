package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CreateOAuthState persists a new OAuth state for CSRF protection.
func (s *Store) CreateOAuthState(ctx context.Context, state *OAuthState) (*OAuthState, error) {
	_, err := s.db.NewInsert().
		Model(state).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create oauth state: %w", err)
	}

	return state, nil
}

// ConsumeOAuthState retrieves and deletes an OAuth state in one operation.
// It only matches states that have not yet expired.
func (s *Store) ConsumeOAuthState(ctx context.Context, stateToken string) (*OAuthState, error) {
	state := new(OAuthState)
	err := s.db.NewSelect().
		Model(state).
		Where("state = ?", stateToken).
		Where("expires_at > NOW()").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOAuthStateNotFound
		}
		return nil, fmt.Errorf("failed to get oauth state: %w", err)
	}

	_, err = s.db.NewDelete().
		Model((*OAuthState)(nil)).
		Where("uid = ?", state.UID).
		ForceDelete().
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to delete oauth state: %w", err)
	}

	return state, nil
}

// CleanupExpiredOAuthStates removes all expired OAuth states.
func (s *Store) CleanupExpiredOAuthStates(ctx context.Context) (int64, error) {
	result, err := s.db.NewDelete().
		Model((*OAuthState)(nil)).
		Where("expires_at <= NOW()").
		ForceDelete().
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup expired oauth states: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return rowsAffected, nil
}
