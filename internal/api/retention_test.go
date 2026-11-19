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

	"github.com/fclairamb/dbbat/internal/store"
)

func retentionRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.PUT("/api/v1/instance/retention", server.requireAdmin(), server.handleUpdateInstanceRetention)
	router.PUT("/api/v1/servers/:uid", server.requireAdmin(), server.handleUpdateDatabase)
	router.GET("/api/v1/servers/:uid", server.handleGetDatabase)

	return router
}

func doRetention(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

func TestValidateRetentionSeconds(t *testing.T) {
	t.Parallel()

	withArchive := &Server{} // no config: no archive bucket

	s := func(v int64) *int64 { return &v }
	set := func(v *int64) store.OptionalSeconds { return store.OptionalSeconds{Set: true, Value: v} }

	// No archive bucket: an archive window is a 400, a delete window is fine.
	assert.Contains(t, withArchive.validateRetentionSeconds(set(s(10)), store.OptionalSeconds{}, nil, nil), "DBB_ARCHIVE_URL")
	assert.Empty(t, withArchive.validateRetentionSeconds(store.OptionalSeconds{}, set(s(10)), nil, nil))
	assert.Empty(t, withArchive.validateRetentionSeconds(set(nil), set(s(0)), nil, nil))
	assert.Contains(t, withArchive.validateRetentionSeconds(store.OptionalSeconds{}, set(s(-1)), nil, nil), "negative")
}

func TestUpdateInstanceRetention(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	createTestUser(t, dataStore, "admin-ret", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-ret", "adminpass123")
	router := retentionRouter(server)

	woken := 0
	server.SetRetentionWaker(func() { woken++ })

	// Malformed, negative and sub-second values are refused.
	for _, body := range []string{
		`{"delete_after":"30d"}`, `{"delete_after":"-1h"}`, `{"delete_after":"1500ms"}`,
		`{"archive_after":"1h"}`, // no bucket configured
		`{"archive_after":"2h","delete_after":"1h"}`,
	} {
		w := doRetention(t, router, http.MethodPut, "/api/v1/instance/retention", token, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, body)
	}

	assert.Zero(t, woken)

	w := doRetention(t, router, http.MethodPut, "/api/v1/instance/retention", token, `{"delete_after":"720h"}`)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Equal(t, 1, woken, "a policy edit wakes the sweeper")

	raw, err := dataStore.GetRetentionParams(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "720h", raw.DeleteAfter)

	// Empty falls back to the environment.
	w = doRetention(t, router, http.MethodPut, "/api/v1/instance/retention", token, `{}`)
	require.Equal(t, http.StatusNoContent, w.Code)

	raw, err = dataStore.GetRetentionParams(context.Background())
	require.NoError(t, err)
	assert.Empty(t, raw.DeleteAfter)
}

// PUT /servers/{uid} takes the windows tri-state, and GET reports the
// effective ones with their source.
func TestServerRetentionTriState(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	createTestUser(t, dataStore, "admin-ret2", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-ret2", "adminpass123")
	router := retentionRouter(server)

	row := newEditTestServerRow(t, dataStore, "ret_target")
	url := "/api/v1/servers/" + row.UID.String()

	type data struct {
		DeleteAfterSeconds *int64 `json:"delete_after_seconds"`
		Effective          struct {
			Delete struct {
				Seconds int64  `json:"seconds"`
				Source  string `json:"source"`
			} `json:"delete"`
		} `json:"effective_retention"`
	}

	type body struct{ Data data }

	read := func() body {
		w := doRetention(t, router, http.MethodGet, url, token, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var b body
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b.Data))

		return b
	}

	// Nothing set anywhere: never.
	b := read()
	assert.Nil(t, b.Data.DeleteAfterSeconds)
	assert.Zero(t, b.Data.Effective.Delete.Seconds)

	// A number sets it.
	w := doRetention(t, router, http.MethodPut, url, token, `{"delete_after_seconds":3600}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	b = read()
	require.NotNil(t, b.Data.DeleteAfterSeconds)
	assert.Equal(t, int64(3600), b.Data.Effective.Delete.Seconds)
	assert.Equal(t, "server", b.Data.Effective.Delete.Source)

	// Absent leaves it alone.
	w = doRetention(t, router, http.MethodPut, url, token, `{"description":"x"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotNil(t, read().Data.DeleteAfterSeconds)

	// A negative value and an archive window with no bucket are 400s.
	for _, bad := range []string{`{"delete_after_seconds":-5}`, `{"archive_after_seconds":60}`} {
		w = doRetention(t, router, http.MethodPut, url, token, bad)
		assert.Equal(t, http.StatusBadRequest, w.Code, bad)
	}

	// null inherits again.
	w = doRetention(t, router, http.MethodPut, url, token, `{"delete_after_seconds":null}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Nil(t, read().Data.DeleteAfterSeconds)
}

func TestServerGroupRetentionWindows(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	createTestUser(t, dataStore, "admin-ret3", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-ret3", "adminpass123")

	router := retentionRouter(server)
	router.POST("/api/v1/server-groups", server.requireAdmin(), server.handleCreateServerGroup)
	router.PATCH("/api/v1/server-groups/:uid", server.requireAdmin(), server.handleUpdateServerGroup)

	type group struct {
		Data struct {
			UID                string `json:"uid"`
			DeleteAfterSeconds *int64 `json:"delete_after_seconds"`
		}
	}

	// A window given at creation is stored.
	w := doRetention(t, router, http.MethodPost, "/api/v1/server-groups", token,
		`{"name":"ret-grp","delete_after_seconds":7200}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var g group
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &g.Data))
	require.NotNil(t, g.Data.DeleteAfterSeconds)
	assert.Equal(t, int64(7200), *g.Data.DeleteAfterSeconds)

	// A negative window is refused, and creates nothing.
	w = doRetention(t, router, http.MethodPost, "/api/v1/server-groups", token,
		`{"name":"ret-grp-bad","delete_after_seconds":-1}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// Absent leaves it alone, null inherits again.
	url := "/api/v1/server-groups/" + g.Data.UID
	w = doRetention(t, router, http.MethodPatch, url, token, `{"name":"ret-grp"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &g.Data))
	require.NotNil(t, g.Data.DeleteAfterSeconds)

	w = doRetention(t, router, http.MethodPatch, url, token, `{"name":"ret-grp","delete_after_seconds":null}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	g = group{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &g.Data))
	assert.Nil(t, g.Data.DeleteAfterSeconds)
}
