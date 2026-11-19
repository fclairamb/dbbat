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

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("create state", func(t *testing.T) {
		t.Parallel()

		stateToken := "random-state-token-" + uuid.NewString()[:8]

		state := &OAuthState{
			State:       stateToken,
			Provider:    IdentityTypeSlack,
			RedirectURL: "https://example.com/callback",
			Metadata:    json.RawMessage(`{"team_id":"T123"}`),
		}

		created, err := store.CreateOAuthState(ctx, state, 10*time.Minute)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, created.UID)
		assert.Equal(t, stateToken, created.State)
		assert.Equal(t, IdentityTypeSlack, created.Provider)
		assert.False(t, created.CreatedAt.IsZero())
	})
}

func TestConsumeOAuthState(t *testing.T) {
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("consume valid state", func(t *testing.T) {
		t.Parallel()

		state := &OAuthState{
			State:       "consume-valid-" + uuid.NewString(),
			Provider:    IdentityTypeSlack,
			RedirectURL: "https://example.com/callback",
		}
		_, err := store.CreateOAuthState(ctx, state, 10*time.Minute)
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
			State:    "consume-expired-" + uuid.NewString(),
			Provider: IdentityTypeSlack,
		}
		// Negative TTL: born expired, on the database's clock.
		_, err := store.CreateOAuthState(ctx, state, -1*time.Minute)
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

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	// Every namespace that shares the table: OAuth authorize state, device
	// authorization requests and login-exchange codes.
	providers := []string{IdentityTypeSlack, DeviceAuthProvider, LoginExchangeProvider}

	live := make(map[string]string, len(providers))

	for _, provider := range providers {
		for i := 0; i < 2; i++ {
			_, err := store.CreateOAuthState(ctx, &OAuthState{
				State:    "expired-" + uuid.NewString(),
				Provider: provider,
			}, -1*time.Hour)
			require.NoError(t, err)
		}

		state := "valid-" + uuid.NewString()
		_, err := store.CreateOAuthState(ctx, &OAuthState{State: state, Provider: provider}, 1*time.Hour)
		require.NoError(t, err)

		live[provider] = state
	}

	deleted, err := store.CleanupExpiredOAuthStates(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2*len(providers)), deleted)

	for _, provider := range providers {
		var remaining []OAuthState
		require.NoError(t, store.db.NewSelect().Model(&remaining).Where("provider = ?", provider).Scan(ctx))
		require.Len(t, remaining, 1, "provider %s: only the live row is kept", provider)
		assert.Equal(t, live[provider], remaining[0].State)
	}
}
