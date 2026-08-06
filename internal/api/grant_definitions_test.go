package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

func TestCreateGrantDefinition_Slug(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "cgds"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := grantDefinitionsRouter(server)

	t.Run("valid slug creates the definition", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("Read-only 1h "+suffix, "read-only-1h-"+suffix))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "read-only-1h-"+suffix, resp["slug"])
	})

	t.Run("missing slug is rejected", func(t *testing.T) {
		t.Parallel()

		body := validCreateDefinitionBody("No slug "+suffix, "")
		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})

	t.Run("invalid slug format is rejected", func(t *testing.T) {
		t.Parallel()

		for _, bad := range []string{"Has Uppercase", "has_underscore", "-leading-hyphen", "trailing-hyphen-", "double--hyphen", "has space"} {
			body := validCreateDefinitionBody("Bad slug "+suffix, bad)
			w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
			require.Equal(t, http.StatusBadRequest, w.Code, "slug %q: body=%s", bad, w.Body.String())
			require.Equal(t, "VALIDATION_ERROR", resp["code"], "slug %q", bad)
		}
	})

	t.Run("duplicate slug is rejected with 409", func(t *testing.T) {
		t.Parallel()

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

func TestGetGrantDefinition_BySlug(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "ggds"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	def := createTestGrantDefinition(t, dataStore, *admin, "fetch-by-slug-"+suffix, false)

	router := grantDefinitionsRouter(server)

	t.Run("fetch by uid still works", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+def.UID.String(), adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, def.UID.String(), resp["uid"])
	})

	t.Run("fetch by slug resolves the same definition", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+def.Slug, adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, def.UID.String(), resp["uid"])
	})

	t.Run("unknown slug is a 404", func(t *testing.T) {
		t.Parallel()

		w, _ := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/does-not-exist", adminToken, nil)
		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

// TestCreateGrantDefinition_RejectsUUIDShapedSlug pins down the design
// decision that superseded the earlier "UUID-shaped slugs fall back to a
// slug lookup" behavior: a UUID is no longer a valid slug at all, in either
// form uuid.Parse accepts — the canonical 36-char hyphenated layout and the
// bare 32-hex-digit layout, both of which otherwise satisfy slugPattern.
func TestCreateGrantDefinition_RejectsUUIDShapedSlug(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "ggdsu"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := grantDefinitionsRouter(server)

	canonical := uuid.New().String()
	bareHex := strings.ReplaceAll(canonical, "-", "")

	t.Run("canonical hyphenated form is rejected", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("UUID slug canonical "+suffix, canonical))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})

	t.Run("bare 32-hex-digit form is rejected", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken,
			validCreateDefinitionBody("UUID slug bare hex "+suffix, bareHex))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})
}

func TestCreateGrantRequest_BySlug(t *testing.T) {
	t.Parallel()

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

// TestUpdateGrantDefinition_PartialToggleDoesNotWipeApprovalConfig is the
// direct regression test for the bug: PATCHing only auto_approve (the
// shape the list-view toggle sends) must leave approval_patterns,
// approver_group_uids and priority exactly as they were, because PATCH is
// now a genuine partial update rather than a full replace.
func TestUpdateGrantDefinition_PartialToggleDoesNotWipeApprovalConfig(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "ugdpt"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	approverGroup, err := dataStore.CreateUserGroup(context.Background(), &store.UserGroup{
		Name:      "approvers-" + suffix,
		CreatedBy: &admin.UID,
	})
	require.NoError(t, err)

	priority := int16(5)
	def, err := dataStore.CreateGrantDefinition(context.Background(), &store.GrantDefinition{
		Name:              "partial-toggle-" + suffix,
		Slug:              "partial-toggle-" + suffix,
		DurationSeconds:   3600,
		Controls:          []string{store.ControlReadOnly},
		Priority:          &priority,
		ApprovalPatterns:  []string{"^DELETE", "^DROP"},
		ApproverGroupUIDs: []uuid.UUID{approverGroup.UID},
		CreatedBy:         admin.UID,
	})
	require.NoError(t, err)
	require.False(t, def.AutoApprove)

	router := grantDefinitionsRouter(server)

	w, resp := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+def.UID.String(), adminToken,
		map[string]any{"auto_approve": true})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, true, resp["auto_approve"])
	require.ElementsMatch(t, []string{"^DELETE", "^DROP"}, resp["approval_patterns"],
		"toggling auto_approve alone must not wipe approval_patterns")
	require.ElementsMatch(t, []string{approverGroup.UID.String()}, resp["approver_group_uids"],
		"toggling auto_approve alone must not wipe approver_group_uids")
	require.EqualValues(t, priority, resp["priority"], "toggling auto_approve alone must not wipe priority")

	// Re-read to confirm the persisted row, not just the response body.
	w, resp = doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+def.UID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, true, resp["auto_approve"])
	require.ElementsMatch(t, []string{"^DELETE", "^DROP"}, resp["approval_patterns"])
	require.ElementsMatch(t, []string{approverGroup.UID.String()}, resp["approver_group_uids"])
	require.EqualValues(t, priority, resp["priority"])
}

// TestUpdateGrantDefinition_FullBodyStillSetsPatterns pins down that a full
// PATCH body — the shape the edit dialog sends — still applies every field,
// including explicitly replacing approval_patterns, exactly as a full
// replace did before PATCH became partial.
func TestUpdateGrantDefinition_FullBodyStillSetsPatterns(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "ugdfb"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	def, err := dataStore.CreateGrantDefinition(context.Background(), &store.GrantDefinition{
		Name:             "full-body-" + suffix,
		Slug:             "full-body-" + suffix,
		DurationSeconds:  3600,
		Controls:         []string{store.ControlReadOnly},
		ApprovalPatterns: []string{"^DELETE"},
		CreatedBy:        admin.UID,
	})
	require.NoError(t, err)

	router := grantDefinitionsRouter(server)

	w, resp := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+def.UID.String(), adminToken,
		map[string]any{
			"name":                  "full-body-updated-" + suffix,
			"slug":                  def.Slug,
			"description":           "updated via full body",
			"duration_seconds":      7200,
			"controls":              []string{store.ControlReadOnly, store.ControlBlockDDL},
			"max_query_counts":      nil,
			"max_bytes_transferred": nil,
			"priority":              nil,
			"auto_approve":          false,
			"group_uids":            []string{},
			"database_uids":         []string{},
			"approval_patterns":     []string{"^TRUNCATE", "^DROP"},
			"approver_group_uids":   []string{},
		})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "full-body-updated-"+suffix, resp["name"])
	require.EqualValues(t, 7200, resp["duration_seconds"])
	require.ElementsMatch(t, []string{"^TRUNCATE", "^DROP"}, resp["approval_patterns"])
}

// TestUpdateGrantDefinition_ExplicitEmptyArrayClears pins down that presence
// still wins over absence for slice fields: a PATCH that explicitly sends
// approval_patterns: [] clears the list, which is different from omitting
// the field entirely (covered by the partial-toggle test above). Untouched
// fields (here, priority) must still survive.
func TestUpdateGrantDefinition_ExplicitEmptyArrayClears(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "ugdec"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	priority := int16(3)
	def, err := dataStore.CreateGrantDefinition(context.Background(), &store.GrantDefinition{
		Name:             "empty-clears-" + suffix,
		Slug:             "empty-clears-" + suffix,
		DurationSeconds:  3600,
		Controls:         []string{store.ControlReadOnly},
		Priority:         &priority,
		ApprovalPatterns: []string{"^DELETE", "^DROP"},
		CreatedBy:        admin.UID,
	})
	require.NoError(t, err)

	router := grantDefinitionsRouter(server)

	w, resp := doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+def.UID.String(), adminToken,
		map[string]any{"approval_patterns": []string{}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Empty(t, resp["approval_patterns"], "an explicit empty array must clear approval_patterns")
	require.EqualValues(t, priority, resp["priority"], "a field absent from the PATCH body must survive untouched")
}
