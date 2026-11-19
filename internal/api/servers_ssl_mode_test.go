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

func oracleSSLPayload(name, mode string) map[string]any {
	return map[string]any{
		"name": name, "host": "ora.example.com", "port": 1521,
		"oracle_service_name": "ORCLPDB1", "username": "app", "password": "pw",
		"protocol": "oracle", "ssl_mode": mode,
	}
}

func TestOracleServerRefusesTLSSSLModes(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, oracleSSLPayload("ora_"+mode, mode))
		require.Equal(t, 400, w.Code, "ssl_mode=%q: %s", mode, w.Body.String())
		assert.Contains(t, w.Body.String(), "does not encrypt the Oracle upstream leg")
	}

	for i, mode := range []string{"disable", "allow", "prefer", ""} {
		w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, oracleSSLPayload("ora_ok_"+string(rune('a'+i)), mode))
		require.Equal(t, 200, w.Code, "ssl_mode=%q: %s", mode, w.Body.String())
	}

	// Positive control: PostgreSQL still takes verify-full.
	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, sslModePayload("pg_vf", "verify-full"))
	require.Equal(t, 200, w.Code, w.Body.String())
}

func TestOracleServerUpdateRefusesTLSSSLModes(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, oracleSSLPayload("ora_edit", "prefer"))
	require.Equal(t, 200, w.Code, w.Body.String())

	// Protocol change of a pristine postgres row at verify-full to oracle.
	w, _ = doJSON(t, router, "POST", "/api/v1/servers", token, sslModePayload("pg_to_ora", "verify-full"))
	require.Equal(t, 200, w.Code, w.Body.String())

	_, listing := doJSON(t, router, "GET", "/api/v1/servers", token, nil)
	rows, ok := listing["databases"].([]any)
	require.True(t, ok, listing)

	uids := map[string]string{}

	for _, r := range rows {
		row, isMap := r.(map[string]any)
		require.True(t, isMap)
		name, _ := row["name"].(string)
		uid, _ := row["uid"].(string)
		uids[name] = uid
	}

	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uids["ora_edit"], token, map[string]any{"ssl_mode": "verify-full"})
	require.Equal(t, 400, w.Code, w.Body.String())

	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uids["ora_edit"], token, map[string]any{"ssl_mode": "disable"})
	require.Equal(t, 200, w.Code, w.Body.String())

	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uids["pg_to_ora"], token,
		map[string]any{"protocol": "oracle", "oracle_service_name": "SVC"})
	require.Equal(t, 400, w.Code, w.Body.String())

	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uids["pg_to_ora"], token,
		map[string]any{"protocol": "oracle", "oracle_service_name": "SVC", "ssl_mode": "prefer"})
	require.Equal(t, 200, w.Code, w.Body.String())
}
