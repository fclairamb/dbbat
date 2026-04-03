package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateOAuthState(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	t.Run("create state", func(t *testing.T) {
		t.Parallel()

		state := &OAuthState{
			State:       "random-state-token-1",
			Provider:    IdentityTypeSlack,
			RedirectURL: "https://example.com/callback",
			Metadata:    json.RawMessage(`{"team_id":"T123"}`),
			ExpiresAt:   time.Now().Add(10 * time.Minute),
		}

		created, err := store.CreateOAuthState(ctx, state)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, created.UID)
		assert.Equal(t, "random-state-token-1", created.State)
		assert.Equal(t, IdentityTypeSlack, created.Provider)
		assert.False(t, created.CreatedAt.IsZero())
	})
}

func TestConsumeOAuthState(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	t.Run("consume valid state", func(t *testing.T) {
		t.Parallel()

		state := &OAuthState{
			State:       "consume-valid-" + uuid.NewString(),
			Provider:    IdentityTypeSlack,
			RedirectURL: "https://example.com/callback",
			ExpiresAt:   time.Now().Add(10 * time.Minute),
		}
		_, err := store.CreateOAuthState(ctx, state)
		require.NoError(t, err)

		consumed, err := store.ConsumeOAuthState(ctx, state.State)
		require.NoError(t, err)
		assert.Equal(t, state.State, consumed.State)
		assert.Equal(t, IdentityTypeSlack, consumed.Provider)

		// Second consume should fail (row deleted)
		_, err = store.ConsumeOAuthState(ctx, state.State)
		assert.ErrorIs(t, err, ErrOAuthStateNotFound)
	})

	t.Run("consume expired state", func(t *testing.T) {
		t.Parallel()

		state := &OAuthState{
			State:     "consume-expired-" + uuid.NewString(),
			Provider:  IdentityTypeSlack,
			ExpiresAt: time.Now().Add(-1 * time.Minute), // Already expired
		}
		_, err := store.CreateOAuthState(ctx, state)
		require.NoError(t, err)

		_, err = store.ConsumeOAuthState(ctx, state.State)
		assert.ErrorIs(t, err, ErrOAuthStateNotFound)
	})

	t.Run("consume non-existing state", func(t *testing.T) {
		t.Parallel()

		_, err := store.ConsumeOAuthState(ctx, "nonexistent-state")
		assert.ErrorIs(t, err, ErrOAuthStateNotFound)
	})
}

func TestCleanupExpiredOAuthStates(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	// Create some expired states
	for i := 0; i < 3; i++ {
		_, err := store.CreateOAuthState(ctx, &OAuthState{
			State:     "expired-" + uuid.NewString(),
			Provider:  IdentityTypeSlack,
			ExpiresAt: time.Now().Add(-1 * time.Hour),
		})
		require.NoError(t, err)
	}

	// Create a valid state
	validState := &OAuthState{
		State:     "valid-" + uuid.NewString(),
		Provider:  IdentityTypeSlack,
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}
	_, err := store.CreateOAuthState(ctx, validState)
	require.NoError(t, err)

	// Cleanup
	deleted, err := store.CleanupExpiredOAuthStates(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), deleted)

	// Valid state should still be consumable
	consumed, err := store.ConsumeOAuthState(ctx, validState.State)
	require.NoError(t, err)
	assert.Equal(t, validState.State, consumed.State)
}
