package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateCLIAuthRequest(t *testing.T) {
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	pollToken := "poll-" + uuid.NewString()
	req, err := store.CreateCLIAuthRequest(ctx, "my-tool on host", pollToken, "ABCD-1234")
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, req.UID)
	assert.Equal(t, "my-tool on host", req.Name)
	assert.Equal(t, "ABCD-1234", req.UserCode)
	assert.Equal(t, CLIAuthStatusPending, req.Status)
	assert.False(t, req.ExpiresAt.IsZero())
}

func TestGetCLIAuthRequest(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("found", func(t *testing.T) {
		req, err := store.CreateCLIAuthRequest(ctx, "found-tool", "poll-"+uuid.NewString(), "AAAA-1111")
		require.NoError(t, err)

		fetched, err := store.GetCLIAuthRequest(ctx, req.UID)
		require.NoError(t, err)
		assert.Equal(t, req.UID, fetched.UID)
		assert.Equal(t, "found-tool", fetched.Name)
		assert.Equal(t, CLIAuthStatusPending, fetched.Status)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := store.GetCLIAuthRequest(ctx, uuid.New())
		assert.ErrorIs(t, err, ErrCLIAuthNotFound)
	})
}

func TestRespondToCLIAuthRequest(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("approve", func(t *testing.T) {
		req, err := store.CreateCLIAuthRequest(ctx, "approve-tool", "poll-"+uuid.NewString(), "BBBB-2222")
		require.NoError(t, err)

		userID := uuid.New()
		err = store.RespondToCLIAuthRequest(ctx, req.UID, userID, true, []byte("encrypted"), "dbb_prefix")
		require.NoError(t, err)

		fetched, err := store.GetCLIAuthRequest(ctx, req.UID)
		require.NoError(t, err)
		assert.Equal(t, CLIAuthStatusApproved, fetched.Status)
	})

	t.Run("deny", func(t *testing.T) {
		req, err := store.CreateCLIAuthRequest(ctx, "deny-tool", "poll-"+uuid.NewString(), "CCCC-3333")
		require.NoError(t, err)

		err = store.RespondToCLIAuthRequest(ctx, req.UID, uuid.New(), false, nil, "")
		require.NoError(t, err)

		fetched, err := store.GetCLIAuthRequest(ctx, req.UID)
		require.NoError(t, err)
		assert.Equal(t, CLIAuthStatusDenied, fetched.Status)
	})

	t.Run("already resolved", func(t *testing.T) {
		req, err := store.CreateCLIAuthRequest(ctx, "double-tool", "poll-"+uuid.NewString(), "DDDD-4444")
		require.NoError(t, err)

		userID := uuid.New()
		require.NoError(t, store.RespondToCLIAuthRequest(ctx, req.UID, userID, true, []byte("encrypted"), "dbb_prefix"))

		err = store.RespondToCLIAuthRequest(ctx, req.UID, userID, false, nil, "")
		assert.ErrorIs(t, err, ErrCLIAuthAlreadyResolved)
	})

	t.Run("not found", func(t *testing.T) {
		err := store.RespondToCLIAuthRequest(ctx, uuid.New(), uuid.New(), true, []byte("x"), "dbb_prefix")
		assert.ErrorIs(t, err, ErrCLIAuthNotFound)
	})
}

func TestPollCLIAuthRequest(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("pending stays pending", func(t *testing.T) {
		pollToken := "poll-" + uuid.NewString()
		_, err := store.CreateCLIAuthRequest(ctx, "pending-tool", pollToken, "EEEE-5555")
		require.NoError(t, err)

		req, key, err := store.PollCLIAuthRequest(ctx, pollToken)
		require.NoError(t, err)
		assert.Equal(t, CLIAuthStatusPending, req.Status)
		assert.Nil(t, key)

		// Polling again still finds it — pending requests are not consumed.
		req, key, err = store.PollCLIAuthRequest(ctx, pollToken)
		require.NoError(t, err)
		assert.Equal(t, CLIAuthStatusPending, req.Status)
		assert.Nil(t, key)
	})

	t.Run("approved is delivered exactly once", func(t *testing.T) {
		pollToken := "poll-" + uuid.NewString()
		created, err := store.CreateCLIAuthRequest(ctx, "approved-tool", pollToken, "FFFF-6666")
		require.NoError(t, err)

		encKey := []byte("super-secret-encrypted-key")
		require.NoError(t, store.RespondToCLIAuthRequest(ctx, created.UID, uuid.New(), true, encKey, "dbb_prefix"))

		req, key, err := store.PollCLIAuthRequest(ctx, pollToken)
		require.NoError(t, err)
		assert.Equal(t, CLIAuthStatusApproved, req.Status)
		assert.Equal(t, encKey, key)

		// Second poll: the row was consumed, so it now looks not-found.
		_, _, err = store.PollCLIAuthRequest(ctx, pollToken)
		assert.ErrorIs(t, err, ErrCLIAuthNotFound)
	})

	t.Run("denied is delivered exactly once", func(t *testing.T) {
		pollToken := "poll-" + uuid.NewString()
		created, err := store.CreateCLIAuthRequest(ctx, "denied-tool", pollToken, "GGGG-7777")
		require.NoError(t, err)

		require.NoError(t, store.RespondToCLIAuthRequest(ctx, created.UID, uuid.New(), false, nil, ""))

		req, key, err := store.PollCLIAuthRequest(ctx, pollToken)
		require.NoError(t, err)
		assert.Equal(t, CLIAuthStatusDenied, req.Status)
		assert.Nil(t, key)

		_, _, err = store.PollCLIAuthRequest(ctx, pollToken)
		assert.ErrorIs(t, err, ErrCLIAuthNotFound)
	})

	t.Run("unknown token", func(t *testing.T) {
		_, _, err := store.PollCLIAuthRequest(ctx, "nonexistent-poll-token")
		assert.ErrorIs(t, err, ErrCLIAuthNotFound)
	})
}
