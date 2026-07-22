package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateDeviceAuthRequest(t *testing.T) {
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	deviceCode := "device-" + uuid.NewString()
	req, err := store.CreateDeviceAuthRequest(ctx, "my-tool on host", deviceCode, "ABCD1234")
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, req.UID)
	assert.Equal(t, "my-tool on host", req.ClientName)
	assert.Equal(t, "ABCD1234", req.UserCode)
	assert.Equal(t, DeviceAuthStatusPending, req.Status)
	assert.False(t, req.ExpiresAt.IsZero())
}

func TestCreateDeviceAuthRequest_UserCodeCollision(t *testing.T) {
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	userCode := "COLLIDE7"
	_, err := store.CreateDeviceAuthRequest(ctx, "first", "device-"+uuid.NewString(), userCode)
	require.NoError(t, err)

	_, err = store.CreateDeviceAuthRequest(ctx, "second", "device-"+uuid.NewString(), userCode)
	assert.ErrorIs(t, err, ErrDeviceAuthUserCodeTaken)
}

func TestGetDeviceAuthByUserCode(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("found", func(t *testing.T) {
		userCode := "FOUND" + uuid.NewString()[:3]
		_, err := store.CreateDeviceAuthRequest(ctx, "found-tool", "device-"+uuid.NewString(), userCode)
		require.NoError(t, err)

		fetched, err := store.GetDeviceAuthByUserCode(ctx, userCode)
		require.NoError(t, err)
		assert.Equal(t, "found-tool", fetched.ClientName)
		assert.Equal(t, DeviceAuthStatusPending, fetched.Status)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := store.GetDeviceAuthByUserCode(ctx, "NOSUCH99")
		assert.ErrorIs(t, err, ErrDeviceAuthNotFound)
	})
}

func TestRespondToDeviceAuthByUserCode(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("approve", func(t *testing.T) {
		userCode := "APPRV" + uuid.NewString()[:3]
		_, err := store.CreateDeviceAuthRequest(ctx, "approve-tool", "device-"+uuid.NewString(), userCode)
		require.NoError(t, err)

		err = store.RespondToDeviceAuthByUserCode(ctx, userCode, uuid.New(), true, []byte("encrypted"), "dbb_pref")
		require.NoError(t, err)

		fetched, err := store.GetDeviceAuthByUserCode(ctx, userCode)
		require.NoError(t, err)
		assert.Equal(t, DeviceAuthStatusApproved, fetched.Status)
	})

	t.Run("deny", func(t *testing.T) {
		userCode := "DENY" + uuid.NewString()[:4]
		_, err := store.CreateDeviceAuthRequest(ctx, "deny-tool", "device-"+uuid.NewString(), userCode)
		require.NoError(t, err)

		err = store.RespondToDeviceAuthByUserCode(ctx, userCode, uuid.New(), false, nil, "")
		require.NoError(t, err)

		fetched, err := store.GetDeviceAuthByUserCode(ctx, userCode)
		require.NoError(t, err)
		assert.Equal(t, DeviceAuthStatusDenied, fetched.Status)
	})

	t.Run("already resolved", func(t *testing.T) {
		userCode := "DBLE" + uuid.NewString()[:4]
		_, err := store.CreateDeviceAuthRequest(ctx, "double-tool", "device-"+uuid.NewString(), userCode)
		require.NoError(t, err)

		require.NoError(t, store.RespondToDeviceAuthByUserCode(ctx, userCode, uuid.New(), true, []byte("enc"), "dbb_pref"))

		err = store.RespondToDeviceAuthByUserCode(ctx, userCode, uuid.New(), false, nil, "")
		assert.ErrorIs(t, err, ErrDeviceAuthAlreadyResolved)
	})

	t.Run("not found", func(t *testing.T) {
		err := store.RespondToDeviceAuthByUserCode(ctx, "GHOST999", uuid.New(), true, []byte("x"), "dbb_pref")
		assert.ErrorIs(t, err, ErrDeviceAuthNotFound)
	})
}

func TestPollDeviceAuthToken(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("pending stays pending", func(t *testing.T) {
		deviceCode := "device-" + uuid.NewString()
		_, err := store.CreateDeviceAuthRequest(ctx, "pending-tool", deviceCode, "PEND"+uuid.NewString()[:4])
		require.NoError(t, err)

		req, key, err := store.PollDeviceAuthToken(ctx, deviceCode)
		require.NoError(t, err)
		assert.Equal(t, DeviceAuthStatusPending, req.Status)
		assert.Nil(t, key)

		// Polling again still finds it — pending requests are not consumed.
		req, key, err = store.PollDeviceAuthToken(ctx, deviceCode)
		require.NoError(t, err)
		assert.Equal(t, DeviceAuthStatusPending, req.Status)
		assert.Nil(t, key)
	})

	t.Run("approved is delivered exactly once", func(t *testing.T) {
		deviceCode := "device-" + uuid.NewString()
		userCode := "ONCE" + uuid.NewString()[:4]
		_, err := store.CreateDeviceAuthRequest(ctx, "approved-tool", deviceCode, userCode)
		require.NoError(t, err)

		encKey := []byte("super-secret-encrypted-key")
		require.NoError(t, store.RespondToDeviceAuthByUserCode(ctx, userCode, uuid.New(), true, encKey, "dbb_pref"))

		req, key, err := store.PollDeviceAuthToken(ctx, deviceCode)
		require.NoError(t, err)
		assert.Equal(t, DeviceAuthStatusApproved, req.Status)
		assert.Equal(t, encKey, key)

		// Second poll: the row was consumed, so it now looks not-found.
		_, _, err = store.PollDeviceAuthToken(ctx, deviceCode)
		assert.ErrorIs(t, err, ErrDeviceAuthNotFound)
	})

	t.Run("denied is delivered exactly once", func(t *testing.T) {
		deviceCode := "device-" + uuid.NewString()
		userCode := "DEN1" + uuid.NewString()[:4]
		_, err := store.CreateDeviceAuthRequest(ctx, "denied-tool", deviceCode, userCode)
		require.NoError(t, err)

		require.NoError(t, store.RespondToDeviceAuthByUserCode(ctx, userCode, uuid.New(), false, nil, ""))

		req, key, err := store.PollDeviceAuthToken(ctx, deviceCode)
		require.NoError(t, err)
		assert.Equal(t, DeviceAuthStatusDenied, req.Status)
		assert.Nil(t, key)

		_, _, err = store.PollDeviceAuthToken(ctx, deviceCode)
		assert.ErrorIs(t, err, ErrDeviceAuthNotFound)
	})

	t.Run("unknown device code", func(t *testing.T) {
		_, _, err := store.PollDeviceAuthToken(ctx, "nonexistent-device-code")
		assert.ErrorIs(t, err, ErrDeviceAuthNotFound)
	})
}
