package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
)

func sslModePayload(name, mode string) map[string]any {
	return map[string]any{
		"name": name, "host": "db.example.com", "port": 5432,
		"database_name": "app", "username": "app", "password": "pw",
		"protocol": "postgresql", "ssl_mode": mode,
	}
}

func TestCreateServerRejectsUnknownSSLMode(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, sslModePayload("typo_db", "verify-ful"))
	require.Equal(t, 400, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "ssl_mode must be one of: disable, allow, prefer, require, verify-ca, verify-full")

	_, listing := doJSON(t, router, "GET", "/api/v1/servers", token, nil)
	rows, ok := listing["databases"].([]any)
	require.True(t, ok, listing)
	assert.Empty(t, rows, "a rejected row must not be persisted")
}

func TestCreateServerAcceptsEveryKnownSSLMode(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	for _, mode := range upstream.SSLModes() {
		w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, sslModePayload("db_"+mode[:4]+"_"+mode[len(mode)-2:], mode))
		require.Equal(t, 200, w.Code, "ssl_mode=%q: %s", mode, w.Body.String())
	}
}

func TestUpdateServerValidatesSSLMode(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, sslModePayload("edit_db", "prefer"))
	require.Equal(t, 200, w.Code, w.Body.String())

	_, listing := doJSON(t, router, "GET", "/api/v1/servers", token, nil)
	rows, ok := listing["databases"].([]any)
	require.True(t, ok, listing)
	require.Len(t, rows, 1)
	row, ok := rows[0].(map[string]any)
	require.True(t, ok, rows[0])
	uid, ok := row["uid"].(string)
	require.True(t, ok, row)

	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{"ssl_mode": "verifyfull"})
	require.Equal(t, 400, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "ssl_mode must be one of")

	for _, mode := range upstream.SSLModes() {
		w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{"ssl_mode": mode})
		require.Equal(t, 200, w.Code, "ssl_mode=%q: %s", mode, w.Body.String())
	}
}
