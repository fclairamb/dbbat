package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// basicAuthRouter puts one route behind the real auth middleware.
func basicAuthRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.GET("/api/v1/protected", server.authMiddleware(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	return router
}

func basicAuthGet(router *gin.Engine, username, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/protected", nil)
	req.SetBasicAuth(username, password)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

// A user who has not yet changed the password they were given — the seeded
// admin/admin included — must not reach the API through Basic Auth, exactly as
// POST /auth/login refuses them a session.
func TestBasicAuth_RefusesUnchangedPassword(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	router := basicAuthRouter(server)

	hash, err := crypto.HashPassword("initial-password")
	require.NoError(t, err)

	_, err = dataStore.CreateUser(t.Context(), "fresh-admin", hash, []string{store.RoleAdmin})
	require.NoError(t, err)

	w := basicAuthGet(router, "fresh-admin", "initial-password")
	require.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, string(ErrCodePasswordChangeRequired), body["code"])
}

// Positive control: once the password has been changed, Basic Auth works.
func TestBasicAuth_AllowsChangedPassword(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	router := basicAuthRouter(server)

	createTestUser(t, dataStore, "settled-admin", "settled-password", []string{store.RoleAdmin})

	w := basicAuthGet(router, "settled-admin", "settled-password")
	assert.Equal(t, http.StatusOK, w.Code)
}
