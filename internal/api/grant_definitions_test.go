package api

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// grantDefinitionsRouter wires the handlers the slug tests exercise.
func grantDefinitionsRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/grant-definitions", server.requireAdmin(), server.handleCreateGrantDefinition)
	router.GET("/api/v1/grant-definitions/:uid", server.handleGetGrantDefinition)
	router.PATCH("/api/v1/grant-definitions/:uid", server.requireAdmin(), server.handleUpdateGrantDefinition)
	router.POST("/api/v1/grant-requests", server.handleCreateGrantRequest)

	return router
}

func validCreateDefinitionBody(name, slug string) map[string]any {
	return map[string]any{
		"name":             name,
		"slug":             slug,
		"duration_seconds": 3600,
		"controls":         []string{store.ControlReadOnly},
	}
}

func TestCreateGrantDefinition_Slug(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	suffix := "cgds"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := grantDefinitionsRouter(server)

	t.Run("valid slug creates the definition", func(t *testing.T) {
		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("Read-only 1h "+suffix, "read-only-1h-"+suffix))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "read-only-1h-"+suffix, resp["slug"])
	})

	t.Run("missing slug is rejected", func(t *testing.T) {
		body := validCreateDefinitionBody("No slug "+suffix, "")
		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})

	t.Run("invalid slug format is rejected", func(t *testing.T) {
		for _, bad := range []string{"Has Uppercase", "has_underscore", "-leading-hyphen", "trailing-hyphen-", "double--hyphen", "has space"} {
			body := validCreateDefinitionBody("Bad slug "+suffix, bad)
			w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
			require.Equal(t, http.StatusBadRequest, w.Code, "slug %q: body=%s", bad, w.Body.String())
			require.Equal(t, "VALIDATION_ERROR", resp["code"], "slug %q", bad)
		}
	})

	t.Run("duplicate slug is rejected with 409", func(t *testing.T) {
		slug := "dup-slug-" + suffix

		w, _ := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("First "+suffix, slug))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("Second "+suffix, slug))
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		require.Equal(t, "DUPLICATE_NAME", resp["code"])
	})
}

func TestGetGrantDefinition_BySlug(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	suffix := "ggds"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	def := createTestGrantDefinition(t, dataStore, *admin, "fetch-by-slug-"+suffix, false)

	router := grantDefinitionsRouter(server)

	t.Run("fetch by uid still works", func(t *testing.T) {
		w, resp := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+def.UID.String(), adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, def.UID.String(), resp["uid"])
	})

	t.Run("fetch by slug resolves the same definition", func(t *testing.T) {
		w, resp := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+def.Slug, adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, def.UID.String(), resp["uid"])
	})

	t.Run("unknown slug is a 404", func(t *testing.T) {
		w, _ := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/does-not-exist", adminToken, nil)
		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestCreateGrantRequest_BySlug(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	suffix := "cgrs"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	createTestUser(t, dataStore, "req-"+suffix, "reqpass1234", []string{store.RoleConnector})
	token := loginUser(t, server, "req-"+suffix, "reqpass1234")

	db := createTestDBEntry(t, dataStore, "slug-db-"+suffix, true)
	def := createTestGrantDefinition(t, dataStore, *admin, "req-by-slug-"+suffix, false)

	router := grantDefinitionsRouter(server)

	w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-requests", token, map[string]any{
		"grant_definition_id": def.Slug,
		"database_id":         db.UID.String(),
		"justification":       "requesting by slug",
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "pending", resp["status"])
	require.Equal(t, def.UID.String(), resp["grant_definition_id"], "stored request should hold the resolved uid, not the slug")
}
