package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// putTagging performs an authenticated PUT /instance/tagging and returns the
// response recorder, so each test asserts on its own status.
func putTagging(t *testing.T, router *gin.Engine, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/instance/tagging", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

// TestHandleUpdateInstanceTagging_AdminOnlyAndValidation covers the edge
// behavior: a non-admin is refused, and an unrecognized oracle mode is a 400
// at write time — never a stored value that would fold to "off" on every
// replica's next read or fail their next restart the way the env var does.
func TestHandleUpdateInstanceTagging_AdminOnlyAndValidation(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	t.Cleanup(func() {
		_ = dataStore.DeleteParameter(ctx, store.GroupTagging, store.KeyTaggingEnabled)
		_ = dataStore.DeleteParameter(ctx, store.GroupTagging, store.KeyTaggingOracle)
	})

	createTestUser(t, dataStore, "admin-tag", "adminpass123", []string{store.RoleAdmin})
	createTestUser(t, dataStore, "conn-tag", "connpass123", []string{store.RoleConnector})

	adminToken := loginUser(t, server, "admin-tag", "adminpass123")
	connToken := loginUser(t, server, "conn-tag", "connpass123")

	router := setupInstanceRouter(server)

	t.Run("non-admin is forbidden", func(t *testing.T) {
		t.Parallel()

		w := putTagging(t, router, connToken, map[string]any{"enabled": true, "oracle": "off"})
		require.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("unrecognized oracle mode is a 400", func(t *testing.T) {
		t.Parallel()

		w := putTagging(t, router, adminToken,
			map[string]any{"enabled": true, "oracle": "everyone"})
		require.Equal(t, http.StatusBadRequest, w.Code)

		// And the bad value must not have been stored.
		_, err := dataStore.GetParameter(ctx, store.GroupTagging, store.KeyTaggingOracle)
		require.ErrorIs(t, err, store.ErrParameterNotFound)
	})

	t.Run("missing body is a 400", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodPut, "/api/v1/instance/tagging", bytes.NewReader(nil))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// TestHandleUpdateInstanceTagging_RoundTripAndSources covers the write path
// and the GET /instance reporting: the raw admin-only block mirrors what the
// store holds, and resolved_tagging names where each effective value came
// from — parameter when set, env when only DBB_QUERY_TAGGING supplies one.
//
//nolint:paralleltest,tparallel // The subtests walk one server through nothing configured -> env -> parameter, so they are deliberately sequential.
func TestHandleUpdateInstanceTagging_RoundTripAndSources(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	t.Cleanup(func() {
		_ = dataStore.DeleteParameter(ctx, store.GroupTagging, store.KeyTaggingEnabled)
		_ = dataStore.DeleteParameter(ctx, store.GroupTagging, store.KeyTaggingOracle)
	})

	createTestUser(t, dataStore, "admin-rt", "adminpass123", []string{store.RoleAdmin, store.RoleConnector})
	createTestUser(t, dataStore, "conn-rt", "connpass123", []string{store.RoleConnector})

	adminToken := loginUser(t, server, "admin-rt", "adminpass123")
	connToken := loginUser(t, server, "conn-rt", "connpass123")

	router := setupInstanceRouter(server)

	// The subtests below share one server and one store and build on each
	// other (nothing configured → env → parameter), so they are deliberately
	// sequential. Server.config is mutated as part of that progression.
	t.Run("nothing configured resolves to off with no source", func(t *testing.T) {
		resp := getInstance(t, router, adminToken)

		resolved, ok := resp["resolved_tagging"].(map[string]any)
		require.True(t, ok, "resolved_tagging must be present for every caller")
		assert.Equal(t, false, resolved["enabled"])
		assert.Empty(t, resolved["enabled_source"])
		assert.Equal(t, "off", resolved["oracle"])
		assert.Empty(t, resolved["oracle_source"])
	})

	t.Run("environment default shows as env", func(t *testing.T) {
		server.config.QueryTagging = config.QueryTaggingConfig{
			Enabled: true,
			Oracle:  config.QueryTaggingOracleUser,
		}

		resp := getInstance(t, router, adminToken)

		resolved, ok := resp["resolved_tagging"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, resolved["enabled"])
		assert.Equal(t, "env", resolved["enabled_source"])
		assert.Equal(t, "user", resolved["oracle"])
		assert.Equal(t, "env", resolved["oracle_source"])

		tagging, ok := resp["tagging"].(map[string]any)
		require.True(t, ok, "the admin response carries the raw block even when nothing is stored")
		assert.Empty(t, tagging["enabled"],
			"an environment-driven decision leaves the parameters unset — that is what makes it revertible by a redeploy")
		assert.Empty(t, tagging["oracle"])
	})

	t.Run("stored parameters win and show as parameter", func(t *testing.T) {
		w := putTagging(t, router, adminToken,
			map[string]any{"enabled": false, "oracle": "user"})
		require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

		resp := getInstance(t, router, adminToken)

		resolved, ok := resp["resolved_tagging"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, resolved["enabled"],
			`an explicit "false" must override the DBB_QUERY_TAGGING default that is on`)
		assert.Equal(t, "parameter", resolved["enabled_source"])
		assert.Equal(t, "user", resolved["oracle"])
		assert.Equal(t, "parameter", resolved["oracle_source"])

		tagging, ok := resp["tagging"].(map[string]any)
		require.True(t, ok, "admin response must include the raw tagging block")
		assert.Equal(t, "false", tagging["enabled"])
		assert.Equal(t, "user", tagging["oracle"])

		// The environment default stays in the config for the next subtests —
		// restore a clean slate for them first.
		server.config.QueryTagging = config.QueryTaggingConfig{}
	})

	t.Run("non-admin sees resolved but no raw block", func(t *testing.T) {
		resp := getInstance(t, router, connToken)

		_, ok := resp["resolved_tagging"].(map[string]any)
		require.True(t, ok)

		_, hasTagging := resp["tagging"]
		assert.False(t, hasTagging, "non-admin response must not include the raw tagging block")
	})
}
