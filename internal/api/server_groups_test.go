package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

	dbA := createTestDBEntry(t, dataStore, "sg_db_a_"+suffix, true)
	dbB := createTestDBEntry(t, dataStore, "sg_db_b_"+suffix, true)

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
		Name:     "bastion_" + suffix,
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

// TestServerGroupMemberAddReportsBlastRadius pins the payload that makes the
// REST API say what the admin UI warns about: adding a server to a group
// widens every *live* grant bound to that group, instantly and with no
// separate approval.
//
// The group here carries two grants — one inside its window, one long
// expired — and the response must count exactly the live one. An expired grant
// authorizes nothing, so reporting it would overstate the edit; miscounting in
// the other direction would understate it, which is the failure that matters.
func TestServerGroupMemberAddReportsBlastRadius(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "sgblast"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	holder := createTestUser(t, dataStore, "holder-"+suffix, "holderpass123", []string{store.RoleConnector})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	anchor := createTestDBEntry(t, dataStore, "sg_anchor_"+suffix, true)
	joiner := createTestDBEntry(t, dataStore, "sg_joiner_"+suffix, true)

	group, err := dataStore.CreateServerGroup(ctx, &store.ServerGroup{
		Name:      "blast-" + suffix,
		CreatedBy: &admin.UID,
	})
	require.NoError(t, err)
	require.NoError(t, dataStore.SetServerGroupMembers(ctx, group.UID, []uuid.UUID{anchor.UID}))

	// Scoping the definition to the group is what binds the grants issued from
	// it to that group rather than to the anchor database alone.
	def := newTestDefinition(t, dataStore, admin.UID, store.GrantDefinition{
		ServerGroupUIDs: []uuid.UUID{group.UID},
	})

	live, err := dataStore.CreateGrant(ctx, &store.Grant{
		UserID:            holder.UID,
		DatabaseID:        anchor.UID,
		GrantDefinitionID: def.UID,
		GrantedBy:         admin.UID,
		StartsAt:          time.Now().Add(-time.Minute),
		ExpiresAt:         time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NotNil(t, live.ServerGroupUID, "the grant must be bound to the group, not just the anchor")

	expired, err := dataStore.CreateGrant(ctx, &store.Grant{
		UserID:            holder.UID,
		DatabaseID:        anchor.UID,
		GrantDefinitionID: def.UID,
		GrantedBy:         admin.UID,
		StartsAt:          time.Now().Add(-2 * time.Hour),
		ExpiresAt:         time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)
	require.NotNil(t, expired.ServerGroupUID)

	router := serverGroupsRouter(server)

	w, resp := doJSON(t, router, http.MethodPut,
		"/api/v1/server-groups/"+group.UID.String()+"/members/"+joiner.UID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "member added", resp["message"])
	require.EqualValues(t, 1, resp["live_grants_widened"])
	require.Equal(t, []any{"holder-" + suffix}, resp["users"])

	// The audit trail records the blast radius, not just that the edit
	// happened — the count is the whole point of the entry.
	eventType := "server_group.member_added"
	entries, err := dataStore.ListAuditEvents(ctx, store.AuditFilter{
		EventType:   &eventType,
		PerformedBy: &admin.UID,
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, entries, 1)

	var details map[string]any
	require.NoError(t, json.Unmarshal(entries[0].Details, &details))
	require.EqualValues(t, 1, details["live_grants_widened"])
	require.Equal(t, []any{"holder-" + suffix}, details["users"])
	require.Equal(t, group.UID.String(), details["server_group_uid"])
	require.Equal(t, joiner.UID.String(), details["server_uid"])

	// Removing a member stays a plain acknowledgement: narrowing surprises
	// nobody, so the DELETE half deliberately carries no blast radius.
	w, resp = doJSON(t, router, http.MethodDelete,
		"/api/v1/server-groups/"+group.UID.String()+"/members/"+joiner.UID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, resp, "live_grants_widened")
}
