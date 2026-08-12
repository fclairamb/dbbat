package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateOAuthState persists a new OAuth state (or one of the rows that share
// its table: a device authorization request, a login exchange) with a lifetime
// of ttl.
//
// The expiry is stamped by the *database* clock, in the very statement that
// inserts the row, because that is the clock every reader tests it against
// (`expires_at > NOW()`). Stamping it from time.Now() instead made the real
// TTL `ttl ± skew`: a process running ahead of its store lengthened it — a
// login exchange or device authorization redeemable past its intended life —
// and one running behind shortened it. See Store.Now.
//
// A negative ttl yields a row that is already expired; tests use that to
// build historical rows without reintroducing the process clock.
func (s *Store) CreateOAuthState(ctx context.Context, state *OAuthState, ttl time.Duration) (*OAuthState, error) {
	_, err := s.db.NewInsert().
		Model(state).
		Value("expires_at", "NOW() + make_interval(secs => ?)", ttl.Seconds()).
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
