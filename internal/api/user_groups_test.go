package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// scopedRouter wires the handlers the scoping tests exercise.
func scopedRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/grant-definitions", server.handleListGrantDefinitions)
	router.GET("/api/v1/grant-definitions/:uid", server.handleGetGrantDefinition)
	router.POST("/api/v1/grant-requests", server.handleCreateGrantRequest)
	router.POST("/api/v1/grant-requests/:uid/approve", server.requireAdmin(), server.handleApproveGrantRequest)
	router.POST("/api/v1/user-groups", server.requireAdmin(), server.handleCreateUserGroup)
	router.GET("/api/v1/user-groups", server.requireAdmin(), server.handleListUserGroups)
	router.PATCH("/api/v1/user-groups/:uid", server.requireAdmin(), server.handleUpdateUserGroup)
	router.DELETE("/api/v1/user-groups/:uid", server.requireAdmin(), server.handleDeleteUserGroup)
	router.PUT("/api/v1/user-groups/:uid/members/:user_uid", server.requireAdmin(), server.handleAddUserGroupMember)
	router.DELETE("/api/v1/user-groups/:uid/members/:user_uid", server.requireAdmin(), server.handleRemoveUserGroupMember)

	return router
}

func doJSON(t *testing.T, router *gin.Engine, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var reader *bytes.Reader

	if body != nil {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var resp map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}

	return w, resp
}

func TestUserGroupsCRUDEndpoints(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	suffix := "ugcrud"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	member := createTestUser(t, dataStore, "member-"+suffix, "memberpass123", []string{store.RoleConnector})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")
	memberToken := loginUser(t, server, "member-"+suffix, "memberpass123")

	router := scopedRouter(server)

	// Non-admins have no access to the group surface at all.
	w, _ := doJSON(t, router, http.MethodGet, "/api/v1/user-groups", memberToken, nil)
	require.Equal(t, http.StatusForbidden, w.Code)

	w, resp := doJSON(t, router, http.MethodPost, "/api/v1/user-groups", adminToken, map[string]any{
		"name":        "analysts-" + suffix,
		"description": "self-serve",
		"member_uids": []string{member.UID.String()},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	rawUID, ok := resp["uid"].(string)
	require.True(t, ok, "response should carry a uid")

	groupUID, err := uuid.Parse(rawUID)
	require.NoError(t, err)
	require.Len(t, resp["member_uids"], 1)

	// Case-insensitive duplicate names are rejected.
	w, _ = doJSON(t, router, http.MethodPost, "/api/v1/user-groups", adminToken, map[string]any{
		"name": "ANALYSTS-" + suffix,
	})
	require.Equal(t, http.StatusConflict, w.Code)

	// Membership can be replaced wholesale through the update endpoint.
	w, resp = doJSON(t, router, http.MethodPatch, "/api/v1/user-groups/"+groupUID.String(), adminToken, map[string]any{
		"name":        "analysts-renamed-" + suffix,
		"member_uids": []string{},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Empty(t, resp["member_uids"])

	// …or one member at a time (idempotently).
	memberPath := "/api/v1/user-groups/" + groupUID.String() + "/members/" + member.UID.String()

	for range 2 {
		w, _ = doJSON(t, router, http.MethodPut, memberPath, adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	uids, err := dataStore.ListUserGroupUIDs(context.Background(), member.UID)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{groupUID}, uids)

	w, _ = doJSON(t, router, http.MethodDelete, memberPath, adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code)

	uids, err = dataStore.ListUserGroupUIDs(context.Background(), member.UID)
	require.NoError(t, err)
	require.Empty(t, uids)

	// Bogus member uids are rejected rather than silently persisted.
	w, _ = doJSON(t, router, http.MethodPatch, "/api/v1/user-groups/"+groupUID.String(), adminToken, map[string]any{
		"name":        "analysts-renamed-" + suffix,
		"member_uids": []string{uuid.New().String()},
	})
	require.Equal(t, http.StatusBadRequest, w.Code)

	w, _ = doJSON(t, router, http.MethodDelete, "/api/v1/user-groups/"+groupUID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code)

	w, _ = doJSON(t, router, http.MethodDelete, "/api/v1/user-groups/"+groupUID.String(), adminToken, nil)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestListGrantDefinitions_FiltersByGroupScope(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "gdscope"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	insider := createTestUser(t, dataStore, "in-"+suffix, "inpass1234", []string{store.RoleConnector})
	createTestUser(t, dataStore, "out-"+suffix, "outpass1234", []string{store.RoleConnector})

	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")
	insiderToken := loginUser(t, server, "in-"+suffix, "inpass1234")
	outsiderToken := loginUser(t, server, "out-"+suffix, "outpass1234")

	group, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "analysts-" + suffix})
	require.NoError(t, err)
	require.NoError(t, dataStore.AddUserToGroup(ctx, group.UID, insider.UID))

	openDef := createTestGrantDefinition(t, dataStore, *admin, "open-"+suffix, false)

	scopedDef, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "scoped-" + suffix,
		DurationSeconds: 3600,
		Controls:        []string{store.ControlReadOnly},
		GroupUIDs:       []uuid.UUID{group.UID},
		CreatedBy:       admin.UID,
	})
	require.NoError(t, err)

	router := scopedRouter(server)

	names := func(token string) []string {
		w, resp := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions", token, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		list, ok := resp["grant_definitions"].([]any)
		require.True(t, ok)

		out := make([]string, 0, len(list))

		for _, item := range list {
			def, ok := item.(map[string]any)
			require.True(t, ok)

			name, ok := def["name"].(string)
			require.True(t, ok)

			out = append(out, name)
		}

		return out
	}

	require.ElementsMatch(t, []string{openDef.Name, scopedDef.Name}, names(adminToken), "admin listing stays unfiltered")
	require.ElementsMatch(t, []string{openDef.Name, scopedDef.Name}, names(insiderToken))
	require.ElementsMatch(t, []string{openDef.Name}, names(outsiderToken), "out-of-scope definitions are invisible")

	// A direct GET must not be a way around the listing filter.
	w, _ := doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+scopedDef.UID.String(), outsiderToken, nil)
	require.Equal(t, http.StatusNotFound, w.Code)

	w, _ = doJSON(t, router, http.MethodGet, "/api/v1/grant-definitions/"+scopedDef.UID.String(), insiderToken, nil)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestCreateGrantRequest_RejectsOutOfScope(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "grscope"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	requester := createTestUser(t, dataStore, "req-"+suffix, "reqpass1234", []string{store.RoleConnector})
	token := loginUser(t, server, "req-"+suffix, "reqpass1234")

	inScopeDB := createTestDBEntry(t, dataStore, "in-db-"+suffix, true)
	otherDB := createTestDBEntry(t, dataStore, "other-db-"+suffix, true)

	group, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "analysts-" + suffix})
	require.NoError(t, err)

	// Auto-approve is the security-critical path: no human reviews it, so the
	// scope check must hold there too.
	def, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "scoped-auto-" + suffix,
		DurationSeconds: 3600,
		Controls:        []string{store.ControlReadOnly},
		AutoApprove:     true,
		GroupUIDs:       []uuid.UUID{group.UID},
		DatabaseUIDs:    []uuid.UUID{inScopeDB.UID},
		CreatedBy:       admin.UID,
	})
	require.NoError(t, err)

	router := scopedRouter(server)

	// Not a group member yet → 403.
	w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-requests", token, map[string]any{
		"grant_definition_id": def.UID.String(),
		"database_id":         inScopeDB.UID.String(),
		"justification":       "need it",
	})
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Equal(t, "FORBIDDEN", resp["code"])

	require.NoError(t, dataStore.AddUserToGroup(ctx, group.UID, requester.UID))

	// Member, but the database is out of scope → still 403.
	w, _ = doJSON(t, router, http.MethodPost, "/api/v1/grant-requests", token, map[string]any{
		"grant_definition_id": def.UID.String(),
		"database_id":         otherDB.UID.String(),
		"justification":       "need it",
	})
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	// Nothing was granted along the way.
	grants, err := dataStore.ListGrants(ctx, store.GrantFilter{UserID: &requester.UID})
	require.NoError(t, err)
	require.Empty(t, grants)

	// Fully in scope → accepted (and auto-approved).
	w, _ = doJSON(t, router, http.MethodPost, "/api/v1/grant-requests", token, map[string]any{
		"grant_definition_id": def.UID.String(),
		"database_id":         inScopeDB.UID.String(),
		"justification":       "need it",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestApproveGrantRequest_ConflictsWhenScopeTightened(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "aprscope"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	requester := createTestUser(t, dataStore, "req-"+suffix, "reqpass1234", []string{store.RoleConnector})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")
	reqToken := loginUser(t, server, "req-"+suffix, "reqpass1234")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	def := createTestGrantDefinition(t, dataStore, *admin, "unscoped-"+suffix, false)

	router := scopedRouter(server)

	w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-requests", reqToken, map[string]any{
		"grant_definition_id": def.UID.String(),
		"database_id":         db.UID.String(),
		"justification":       "need it",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	requestUID, ok := resp["uid"].(string)
	require.True(t, ok, "response should carry a uid")

	// An admin tightens the scope *after* the request was filed.
	group, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "late-" + suffix})
	require.NoError(t, err)

	def.GroupUIDs = []uuid.UUID{group.UID}
	require.NoError(t, dataStore.UpdateGrantDefinition(ctx, def))

	w, resp = doJSON(t, router, http.MethodPost, "/api/v1/grant-requests/"+requestUID+"/approve", adminToken, nil)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.Equal(t, "CONFLICT", resp["code"])

	// Still pending, and no grant was materialized.
	grants, err := dataStore.ListGrants(ctx, store.GrantFilter{UserID: &requester.UID})
	require.NoError(t, err)
	require.Empty(t, grants)

	// Once the requester is in scope again, approval goes through.
	require.NoError(t, dataStore.AddUserToGroup(ctx, group.UID, requester.UID))

	w, _ = doJSON(t, router, http.MethodPost, "/api/v1/grant-requests/"+requestUID+"/approve", adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
