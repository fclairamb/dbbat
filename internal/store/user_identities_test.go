package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateUserIdentity(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	user, err := store.CreateUser(ctx, "identity-user-"+suffix, "hash", []string{RoleViewer})
	require.NoError(t, err)

	t.Run("create identity", func(t *testing.T) {
		identity := &UserIdentity{
			UserID:      user.UID,
			Provider:    IdentityTypeSlack,
			ProviderID:  "U12345-" + suffix,
			Email:       "user@example.com",
			DisplayName: "Test User",
			Metadata:    json.RawMessage(`{"team_id":"T123"}`),
		}

		created, err := store.CreateUserIdentity(ctx, identity)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, created.UID)
		assert.Equal(t, IdentityTypeSlack, created.Provider)
		assert.Equal(t, "U12345-"+suffix, created.ProviderID)
		assert.Equal(t, "user@example.com", created.Email)
		assert.Equal(t, "Test User", created.DisplayName)
		assert.False(t, created.CreatedAt.IsZero())
		assert.False(t, created.UpdatedAt.IsZero())
	})
}

func TestGetUserIdentity(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	user, err := store.CreateUser(ctx, "get-identity-user-"+suffix, "hash", []string{RoleViewer})
	require.NoError(t, err)

	identity := &UserIdentity{
		UserID:     user.UID,
		Provider:   IdentityTypeSlack,
		ProviderID: "U99999-" + suffix,
	}
	created, err := store.CreateUserIdentity(ctx, identity)
	require.NoError(t, err)

	t.Run("existing identity", func(t *testing.T) {
		found, err := store.GetUserIdentity(ctx, created.UID)
		require.NoError(t, err)
		assert.Equal(t, created.UID, found.UID)
		assert.Equal(t, user.UID, found.UserID)
		assert.Equal(t, IdentityTypeSlack, found.Provider)
	})

	t.Run("non-existing identity", func(t *testing.T) {
		_, err := store.GetUserIdentity(ctx, uuid.New())
		assert.ErrorIs(t, err, ErrIdentityNotFound)
	})
}

func TestGetUserIdentities(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	user, err := store.CreateUser(ctx, "list-identity-user-"+suffix, "hash", []string{RoleViewer})
	require.NoError(t, err)

	t.Run("empty list", func(t *testing.T) {
		emptyUser, err := store.CreateUser(ctx, "list-identity-empty-"+uuid.NewString()[:8], "hash", []string{RoleViewer})
		require.NoError(t, err)

		identities, err := store.GetUserIdentities(ctx, emptyUser.UID)
		require.NoError(t, err)
		assert.Empty(t, identities)
	})

	// Create two identities
	for _, pid := range []string{"U001-" + suffix, "U002-" + suffix} {
		_, err := store.CreateUserIdentity(ctx, &UserIdentity{
			UserID:     user.UID,
			Provider:   IdentityTypeSlack,
			ProviderID: pid,
		})
		require.NoError(t, err)
	}

	t.Run("with identities", func(t *testing.T) {
		identities, err := store.GetUserIdentities(ctx, user.UID)
		require.NoError(t, err)
		assert.Len(t, identities, 2)
	})
}

func TestGetUserByIdentity(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	user, err := store.CreateUser(ctx, "lookup-by-identity-"+suffix, "hash", []string{RoleViewer})
	require.NoError(t, err)

	providerID := "ULOOKUP-" + suffix

	_, err = store.CreateUserIdentity(ctx, &UserIdentity{
		UserID:     user.UID,
		Provider:   IdentityTypeSlack,
		ProviderID: providerID,
	})
	require.NoError(t, err)

	t.Run("existing identity", func(t *testing.T) {
		found, err := store.GetUserByIdentity(ctx, IdentityTypeSlack, providerID)
		require.NoError(t, err)
		assert.Equal(t, user.UID, found.UID)
		assert.Equal(t, "lookup-by-identity-"+suffix, found.Username)
	})

	t.Run("non-existing identity", func(t *testing.T) {
		_, err := store.GetUserByIdentity(ctx, IdentityTypeSlack, "NONEXISTENT")
		assert.ErrorIs(t, err, ErrIdentityNotFound)
	})
}

func TestDeleteUserIdentity(t *testing.T) { //nolint:tparallel // subtests share parent data
	t.Parallel()

	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	user, err := store.CreateUser(ctx, "delete-identity-user-"+suffix, "hash", []string{RoleViewer})
	require.NoError(t, err)

	providerID := "UDELETE-" + suffix

	identity, err := store.CreateUserIdentity(ctx, &UserIdentity{
		UserID:     user.UID,
		Provider:   IdentityTypeSlack,
		ProviderID: providerID,
	})
	require.NoError(t, err)

	t.Run("delete existing identity", func(t *testing.T) {
		err := store.DeleteUserIdentity(ctx, identity.UID)
		require.NoError(t, err)

		// Should not be found after deletion (soft delete)
		_, err = store.GetUserIdentity(ctx, identity.UID)
		assert.ErrorIs(t, err, ErrIdentityNotFound)
	})

	t.Run("delete non-existing identity", func(t *testing.T) {
		err := store.DeleteUserIdentity(ctx, uuid.New())
		assert.ErrorIs(t, err, ErrIdentityNotFound)
	})

	t.Run("soft delete allows re-creation with same provider_id", func(t *testing.T) {
		// After soft-deleting, the partial unique index should allow a new row
		newIdentity, err := store.CreateUserIdentity(ctx, &UserIdentity{
			UserID:     user.UID,
			Provider:   IdentityTypeSlack,
			ProviderID: providerID,
		})
		require.NoError(t, err)
		assert.NotEqual(t, identity.UID, newIdentity.UID)
	})
}
