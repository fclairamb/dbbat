package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestGrantDefinition_StatementTimeout_ThreeStates is the round-trip that
// matters most about this field: `null` and `0` mean *different* things
// ("inherit the instance-wide default" vs "no limit, overriding it"), so a
// serializer that folded either into the other would silently change access
// policy. The three states have to survive create, read and PATCH.
func TestGrantDefinition_StatementTimeout_ThreeStates(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "stto"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := grantDefinitionsRouter(server)

	t.Run("omitted means inherit and round-trips as null", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("Inherit "+suffix, "inherit-"+suffix))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Nil(t, resp["statement_timeout_seconds"],
			"an omitted limit must come back as null, not 0")
	})

	t.Run("zero means no limit and is not folded into null", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("No limit "+suffix, "no-limit-"+suffix)
		body["statement_timeout_seconds"] = 0

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NotNil(t, resp["statement_timeout_seconds"],
			"an explicit 0 must not come back as null: it overrides the global limit")
		require.InDelta(t, 0, resp["statement_timeout_seconds"], 0.001)
	})

	t.Run("a positive value round-trips", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("Thirty "+suffix, "thirty-"+suffix)
		body["statement_timeout_seconds"] = 30

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.InDelta(t, 30, resp["statement_timeout_seconds"], 0.001)

		// And on the read path.
		uid, _ := resp["uid"].(string)
		require.NotEmpty(t, uid)

		w, got := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+uid, adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.InDelta(t, 30, got["statement_timeout_seconds"], 0.001)
	})

	t.Run("a negative value is rejected", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("Negative "+suffix, "negative-"+suffix)
		body["statement_timeout_seconds"] = -1

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})

	t.Run("an absurd value is rejected", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("Absurd "+suffix, "absurd-"+suffix)
		body["statement_timeout_seconds"] = 90000 // > 24h

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})
}

// TestGrantDefinition_StatementTimeout_PatchTriState covers the half a plain
// pointer cannot express: because `null` already means "inherit", sending
// `null` in a PATCH is indistinguishable from omitting the field, so restoring
// inheritance needs the explicit clear flag.
func TestGrantDefinition_StatementTimeout_PatchTriState(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "sttp"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := grantDefinitionsRouter(server)

	body := validCreateDefinitionBody("Patched "+suffix, "patched-"+suffix)
	body["statement_timeout_seconds"] = 30

	w, created := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	uid, _ := created["uid"].(string)
	require.NotEmpty(t, uid)

	// A PATCH touching an unrelated field must not disturb the limit. Note the
	// successor's uid: definitions are immutably versioned, so every edit
	// archives the row and inserts a new one.
	w, patched := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
		map[string]any{"description": "untouched"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.InDelta(t, 30, patched["statement_timeout_seconds"], 0.001,
		"an unrelated PATCH silently dropped the limit")

	uid, _ = patched["uid"].(string)

	// Setting it to 0 is an edit, not a clear.
	w, zeroed := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
		map[string]any{"statement_timeout_seconds": 0})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, zeroed["statement_timeout_seconds"],
		"PATCHing 0 must store 0 (no limit), not null (inherit)")
	require.InDelta(t, 0, zeroed["statement_timeout_seconds"], 0.001)

	uid, _ = zeroed["uid"].(string)

	// Only the clear flag restores inheritance.
	w, cleared := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
		map[string]any{"clear_statement_timeout_seconds": true})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Nil(t, cleared["statement_timeout_seconds"],
		"clear_statement_timeout_seconds must restore inheritance")
}
