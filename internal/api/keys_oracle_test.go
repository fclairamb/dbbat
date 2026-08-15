package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// setupTestServerWithKey is setupTestServer with a master key, which is what an
// Oracle-capable deployment has: without one no key ever gets O5LOGON material,
// so the field under test could not be exercised.
func setupTestServerWithKey(t *testing.T, master []byte) (*Server, *store.Store) {
	t.Helper()

	dataStore := newIsolatedStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	return NewServer(dataStore, master, logger, &config.Config{RunMode: "test"}), dataStore
}

// listedKeyOracleCapability decodes a keys listing into key_prefix →
// oracle_capable, keeping the tri-state (absent) distinguishable from false.
func listedKeyOracleCapability(t *testing.T, w *httptest.ResponseRecorder) map[string]*bool {
	t.Helper()

	var body struct {
		Keys []struct {
			KeyPrefix     string `json:"key_prefix"`
			OracleCapable *bool  `json:"oracle_capable"`
		} `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	got := make(map[string]*bool, len(body.Keys))
	for _, k := range body.Keys {
		got[k.KeyPrefix] = k.OracleCapable
	}

	return got
}

// TestListAPIKeys_ReportsOracleCapability is the listing half of the bug: a key
// minted before Oracle support authenticates against this very endpoint and can
// never be used for an Oracle login, and until now the response said nothing that
// told them apart.
func TestListAPIKeys_ReportsOracleCapability(t *testing.T) {
	t.Parallel()

	master := bytes.Repeat([]byte{0x3d}, 32)
	server, dataStore := setupTestServerWithKey(t, master)
	ctx := context.Background()

	user := createTestUser(t, dataStore, "keysoracle1", "oraclepassword123", []string{"connector"})

	// The key the bug is about: created without a master key, so no verifier was
	// ever derived — exactly the pre-Oracle-support row in production.
	legacy, _, err := dataStore.CreateAPIKey(ctx, user.UID, "Legacy Key", nil)
	require.NoError(t, err)

	modern, _, err := dataStore.CreateAPIKey(ctx, user.UID, "Modern Key", nil, master)
	require.NoError(t, err)

	token := loginUser(t, server, "keysoracle1", "oraclepassword123")

	w := doListAPIKeys(newKeysTestRouter(server), token, "")
	require.Equal(t, http.StatusOK, w.Code)

	got := listedKeyOracleCapability(t, w)

	require.NotNil(t, got[legacy.KeyPrefix], "every listed key must answer the Oracle question")
	assert.False(t, *got[legacy.KeyPrefix],
		"a key with no O5LOGON verifier must be reported as unusable for Oracle")

	require.NotNil(t, got[modern.KeyPrefix])
	assert.True(t, *got[modern.KeyPrefix])
}

// TestListAPIKeys_OracleCapabilityUnderAnotherMasterKey covers the other way a
// key stops working for Oracle: the material is there but this process cannot
// decrypt it (DBB_KEY rotated or misconfigured). The proxy treats such a key as
// no candidate at all, so the listing must not claim it is fine — which is why
// the field is computed by decrypting rather than by testing the column.
func TestListAPIKeys_OracleCapabilityUnderAnotherMasterKey(t *testing.T) {
	t.Parallel()

	mintKey := bytes.Repeat([]byte{0x3d}, 32)
	serveKey := bytes.Repeat([]byte{0x9a}, 32)

	server, dataStore := setupTestServerWithKey(t, serveKey)
	ctx := context.Background()

	user := createTestUser(t, dataStore, "keysoracle2", "oraclepassword123", []string{"connector"})

	stranded, _, err := dataStore.CreateAPIKey(ctx, user.UID, "Stranded Key", nil, mintKey)
	require.NoError(t, err)

	token := loginUser(t, server, "keysoracle2", "oraclepassword123")

	w := doListAPIKeys(newKeysTestRouter(server), token, "")
	require.Equal(t, http.StatusOK, w.Code)

	got := listedKeyOracleCapability(t, w)
	require.NotNil(t, got[stranded.KeyPrefix])
	assert.False(t, *got[stranded.KeyPrefix])
}

// TestListAPIKeys_OracleCapabilityOmittedWithoutMasterKey: with no master key
// the server cannot evaluate the predicate, and saying "false" would libel every
// key on the instance. The field is left out, so a client can tell "unusable"
// from "unknown".
func TestListAPIKeys_OracleCapabilityOmittedWithoutMasterKey(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	user := createTestUser(t, dataStore, "keysoracle3", "oraclepassword123", []string{"connector"})

	key, _, err := dataStore.CreateAPIKey(ctx, user.UID, "Some Key", nil)
	require.NoError(t, err)

	token := loginUser(t, server, "keysoracle3", "oraclepassword123")

	w := doListAPIKeys(newKeysTestRouter(server), token, "")
	require.Equal(t, http.StatusOK, w.Code)

	got := listedKeyOracleCapability(t, w)
	require.Contains(t, got, key.KeyPrefix)
	assert.Nil(t, got[key.KeyPrefix], "unknown must not be reported as false")
}
