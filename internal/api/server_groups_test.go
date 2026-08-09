package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// serverGroupsRouter wires the server-group handlers behind the same admin
// gate the real router uses.
func serverGroupsRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/server-groups", server.requireAdmin(), server.handleCreateServerGroup)
	router.GET("/api/v1/server-groups", server.requireAdmin(), server.handleListServerGroups)
	router.GET("/api/v1/server-groups/:uid", server.requireAdmin(), server.handleGetServerGroup)
	router.PATCH("/api/v1/server-groups/:uid", server.requireAdmin(), server.handleUpdateServerGroup)
	router.DELETE("/api/v1/server-groups/:uid", server.requireAdmin(), server.handleDeleteServerGroup)
	router.GET("/api/v1/server-groups/:uid/members", server.requireAdmin(), server.handleListServerGroupMembers)
	router.PUT("/api/v1/server-groups/:uid/members/:server_uid", server.requireAdmin(), server.handleAddServerGroupMember)
	router.DELETE("/api/v1/server-groups/:uid/members/:server_uid",
		server.requireAdmin(), server.handleRemoveServerGroupMember)

	return router
}

func TestServerGroupsCRUDEndpoints(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "sgcrud"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	createTestUser(t, dataStore, "member-"+suffix, "memberpass123", []string{store.RoleConnector})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")
	memberToken := loginUser(t, server, "member-"+suffix, "memberpass123")

	dbA := createTestDBEntry(t, dataStore, "sg-db-a-"+suffix, true)
	dbB := createTestDBEntry(t, dataStore, "sg-db-b-"+suffix, true)

	router := serverGroupsRouter(server)

	// Server groups are an access-control surface: admin-only, like user
	// groups.
	w, _ := doJSON(t, router, http.MethodGet, "/api/v1/server-groups", memberToken, nil)
	require.Equal(t, http.StatusForbidden, w.Code)

	w, resp := doJSON(t, router, http.MethodPost, "/api/v1/server-groups", adminToken, map[string]any{
		"name":        "analytics-" + suffix,
		"description": "the read replicas",
		"member_uids": []string{dbA.UID.String()},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.ElementsMatch(t, []any{dbA.UID.String()}, resp["member_uids"])
	require.EqualValues(t, 0, resp["active_grant_count"])

	groupUID, ok := resp["uid"].(string)
	require.True(t, ok, "response should carry a uid")

	// Case-insensitive name uniqueness.
	w, _ = doJSON(t, router, http.MethodPost, "/api/v1/server-groups", adminToken, map[string]any{
		"name": "ANALYTICS-" + suffix,
	})
	require.Equal(t, http.StatusConflict, w.Code)

	// A membership replace is wholesale.
	w, resp = doJSON(t, router, http.MethodPatch, "/api/v1/server-groups/"+groupUID, adminToken, map[string]any{
		"name":        "analytics-" + suffix,
		"description": "renamed",
		"member_uids": []string{dbB.UID.String()},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.ElementsMatch(t, []any{dbB.UID.String()}, resp["member_uids"])

	// Omitting member_uids leaves membership untouched.
	w, resp = doJSON(t, router, http.MethodPatch, "/api/v1/server-groups/"+groupUID, adminToken, map[string]any{
		"name": "analytics-" + suffix,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.ElementsMatch(t, []any{dbB.UID.String()}, resp["member_uids"])

	w, resp = doJSON(t, router, http.MethodGet, "/api/v1/server-groups/"+groupUID+"/members", adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	servers, ok := resp["servers"].([]any)
	require.True(t, ok)
	require.Len(t, servers, 1)

	// Member add/remove by path are idempotent.
	w, _ = doJSON(t, router, http.MethodPut,
		"/api/v1/server-groups/"+groupUID+"/members/"+dbA.UID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w, _ = doJSON(t, router, http.MethodPut,
		"/api/v1/server-groups/"+groupUID+"/members/"+dbA.UID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w, resp = doJSON(t, router, http.MethodGet, "/api/v1/server-groups/"+groupUID, adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, resp["member_uids"], 2)

	w, _ = doJSON(t, router, http.MethodDelete,
		"/api/v1/server-groups/"+groupUID+"/members/"+dbA.UID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w, _ = doJSON(t, router, http.MethodDelete, "/api/v1/server-groups/"+groupUID, adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w, _ = doJSON(t, router, http.MethodGet, "/api/v1/server-groups/"+groupUID, adminToken, nil)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestServerGroupRejectsSSHMember pins that an SSH bastion — a dial path, not
// a grantable target — can never become a server-group member, mirroring the
// same refusal on grant-definition scope.
func TestServerGroupRejectsSSHMember(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "sgssh"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	bastion, err := dataStore.CreateServer(context.Background(), &store.Server{
		Name:     "bastion-" + suffix,
		Host:     "ssh.example.com",
		Port:     22,
		Username: "jump",
		Password: "secret",
		Protocol: store.ProtocolSSH,
	}, dbTestEncryptionKey)
	require.NoError(t, err)

	router := serverGroupsRouter(server)

	w, _ := doJSON(t, router, http.MethodPost, "/api/v1/server-groups", adminToken, map[string]any{
		"name":        "with-ssh-" + suffix,
		"member_uids": []string{bastion.UID.String()},
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
