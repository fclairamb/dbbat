package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestGrantDefinition_NotifyBytesStep covers the byte soft limit's three
// states through create, PATCH and clear: null inherits the instance default,
// 0 turns the notice off for the definition, a positive value is the step. A
// negative value is a 400.
func TestGrantDefinition_NotifyBytesStep(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "nbs"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := grantDefinitionsRouter(server)

	t.Run("omitted means inherit", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("Inherit "+suffix, "inherit-"+suffix))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Nil(t, resp["notify_bytes_step"])
	})

	t.Run("negative is rejected", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("Negative "+suffix, "negative-"+suffix)
		body["notify_bytes_step"] = -1

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})

	t.Run("create, patch, zero and clear", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("Step "+suffix, "step-"+suffix)
		body["notify_bytes_step"] = 1 << 30

		w, created := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.InDelta(t, float64(1<<30), created["notify_bytes_step"], 0.001)

		uid, _ := created["uid"].(string)
		require.NotEmpty(t, uid)

		w, got := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+uid, adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.InDelta(t, float64(1<<30), got["notify_bytes_step"], 0.001)

		// An unrelated PATCH keeps the step.
		w, patched := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
			map[string]any{"description": "untouched"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.InDelta(t, float64(1<<30), patched["notify_bytes_step"], 0.001)

		uid, _ = patched["uid"].(string)

		w, resp := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
			map[string]any{"notify_bytes_step": -5})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])

		w, zeroed := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
			map[string]any{"notify_bytes_step": 0})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NotNil(t, zeroed["notify_bytes_step"], "PATCHing 0 must store 0 (off), not null (inherit)")
		require.InDelta(t, 0, zeroed["notify_bytes_step"], 0.001)

		uid, _ = zeroed["uid"].(string)

		w, cleared := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
			map[string]any{"clear_notify_bytes_step": true})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Nil(t, cleared["notify_bytes_step"], "clear_notify_bytes_step must restore inheritance")
	})
}
