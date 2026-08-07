package api

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// validatePatternsRouter wires just the handler under test, plus login.
func validatePatternsRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/grant-definitions/validate-patterns", server.requireAdmin(), server.handleValidateGrantDefinitionPatterns)

	return router
}

func TestValidateGrantDefinitionPatterns(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "vgdp"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	createTestUser(t, dataStore, "viewer-"+suffix, "viewerpass123", []string{store.RoleViewer})
	viewerToken := loginUser(t, server, "viewer-"+suffix, "viewerpass123")

	router := validatePatternsRouter(server)

	t.Run("compile error is reported per pattern without failing the request", func(t *testing.T) {
		t.Parallel()

		body := map[string]any{
			"patterns": []string{"(unclosed", `(?i)^DELETE\s+FROM\s+users`},
			"queries":  []string{"DELETE FROM users WHERE 1=1"},
		}

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions/validate-patterns", adminToken, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		patterns, ok := resp["patterns"].([]any)
		require.True(t, ok, "patterns field missing or wrong type: %v", resp["patterns"])
		require.Len(t, patterns, 2)

		first, ok := patterns[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "(unclosed", first["pattern"])
		require.NotEmpty(t, first["error"], "invalid pattern must report a compile error")

		second, ok := patterns[1].(map[string]any)
		require.True(t, ok)
		require.Equal(t, `(?i)^DELETE\s+FROM\s+users`, second["pattern"])
		require.Empty(t, second["error"], "valid pattern must not report a compile error")
	})

	t.Run("match, no-match, and normalization are all visible", func(t *testing.T) {
		t.Parallel()

		body := map[string]any{
			"patterns": []string{`(?i)^DELETE\s+FROM\s+users`},
			"queries": []string{
				"   DELETE FROM users WHERE 1=1  ", // matches after trim
				"SELECT 1",                         // no match
			},
		}

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions/validate-patterns", adminToken, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		queries, ok := resp["queries"].([]any)
		require.True(t, ok, "queries field missing or wrong type: %v", resp["queries"])
		require.Len(t, queries, 2)

		matched, ok := queries[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, true, matched["matched"])
		require.Equal(t, "DELETE FROM users WHERE 1=1", matched["normalized"],
			"normalized form must show the trimmed SQL the pattern actually runs against")
		require.Equal(t, `(?i)^DELETE\s+FROM\s+users`, matched["matched_pattern"])

		unmatched, ok := queries[1].(map[string]any)
		require.True(t, ok)
		require.Equal(t, false, unmatched["matched"])
		require.Empty(t, unmatched["matched_pattern"])
		require.Equal(t, "SELECT 1", unmatched["normalized"])
	})

	t.Run("first match wins, same as ApprovalGate.Match", func(t *testing.T) {
		t.Parallel()

		body := map[string]any{
			"patterns": []string{`(?i)DELETE`, `(?i)FROM\s+users`},
			"queries":  []string{"DELETE FROM users"},
		}

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions/validate-patterns", adminToken, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		queries, ok := resp["queries"].([]any)
		require.True(t, ok, "queries field missing or wrong type: %v", resp["queries"])
		result, ok := queries[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, `(?i)DELETE`, result["matched_pattern"], "first pattern in order must win")
	})

	t.Run("empty patterns and queries return empty results, not an error", func(t *testing.T) {
		t.Parallel()

		body := map[string]any{"patterns": []string{}, "queries": []string{}}

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions/validate-patterns", adminToken, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Empty(t, resp["patterns"])
		require.Empty(t, resp["queries"])
	})

	t.Run("non-admin is forbidden", func(t *testing.T) {
		t.Parallel()

		body := map[string]any{"patterns": []string{}, "queries": []string{}}

		w, _ := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions/validate-patterns", viewerToken, body)
		require.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("too many patterns is rejected", func(t *testing.T) {
		t.Parallel()

		many := make([]string, 33)
		for i := range many {
			many[i] = "x"
		}

		body := map[string]any{"patterns": many, "queries": []string{}}

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions/validate-patterns", adminToken, body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})
}
