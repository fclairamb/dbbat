package api

import (
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestGrantDefinition_NotifyChannel covers the Slack channel key through
// create and PATCH: an unknown key is a 400 naming the known ones, "none" and
// a configured key are accepted, an unrelated PATCH keeps the key, and
// clear_notify_channel restores the default.
func TestGrantDefinition_NotifyChannel(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.config = &config.Config{
		RunMode: "test",
		SlackNotify: config.SlackNotifyConfig{
			Channels: "prod=#dbbat-prod,nonprod=#dbbat-nonprod",
		},
	}
	server.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	suffix := "nch"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := grantDefinitionsRouter(server)

	t.Run("unknown key is rejected", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("Unknown "+suffix, "unknown-"+suffix)
		body["notify_channel"] = "staging"

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
		require.Contains(t, w.Body.String(), "nonprod")
		require.Contains(t, w.Body.String(), "prod")
	})

	t.Run("omitted means default", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("Default "+suffix, "default-"+suffix))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Nil(t, resp["notify_channel"])
	})

	t.Run("none and configured keys, patch and clear", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("None "+suffix, "none-"+suffix)
		body["notify_channel"] = "none"

		w, created := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "none", created["notify_channel"])

		uid, _ := created["uid"].(string)

		// An unrelated PATCH keeps the key.
		w, patched := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
			map[string]any{"description": "untouched"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "none", patched["notify_channel"])

		uid, _ = patched["uid"].(string)

		w, _ = doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
			map[string]any{"notify_channel": "staging"})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

		w, prod := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
			map[string]any{"notify_channel": "prod"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "prod", prod["notify_channel"])

		uid, _ = prod["uid"].(string)

		w, cleared := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken,
			map[string]any{"clear_notify_channel": true})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Nil(t, cleared["notify_channel"])
	})
}

func TestNotifyChannelKeys(t *testing.T) {
	t.Parallel()

	require.Empty(t, (&Server{}).notifyChannelKeys())

	s := &Server{config: &config.Config{SlackNotify: config.SlackNotifyConfig{Channels: "b=#b,a=#a"}}}
	require.Equal(t, []string{"a", "b"}, s.notifyChannelKeys())
}
