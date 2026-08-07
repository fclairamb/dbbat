package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// createTestGrantDefinition inserts a grant definition directly via the
// store for API-level grant-request tests.
func createTestGrantDefinition(
	t *testing.T,
	dataStore *store.Store,
	createdBy store.User,
	name string,
	autoApprove bool,
) *store.GrantDefinition {
	t.Helper()

	def, err := dataStore.CreateGrantDefinition(context.Background(), &store.GrantDefinition{
		Name: name,
		// The test names passed in are already slug-shaped (lowercase,
		// hyphenated), so reuse them directly rather than adding a second
		// slugification helper just for tests.
		Slug:            name,
		DurationSeconds: 3600,
		Controls:        []string{store.ControlReadOnly},
		AutoApprove:     autoApprove,
		CreatedBy:       createdBy.UID,
	})
	require.NoError(t, err, "createTestGrantDefinition %q", name)

	return def
}

func grantRequestsRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/grant-requests", server.handleCreateGrantRequest)

	return router
}

func postGrantRequest(t *testing.T, router *gin.Engine, token string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/grant-requests", bytes.NewReader(payload))
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

func TestCreateGrantRequest_AutoApproveYieldsActiveGrantInstantly(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "cgra"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	requester := createTestUser(t, dataStore, "req-"+suffix, "reqpass123", []string{store.RoleConnector})
	token := loginUser(t, server, "req-"+suffix, "reqpass123")

	db := createTestDBEntry(t, dataStore, "auto-db-"+suffix, true)
	def := createTestGrantDefinition(t, dataStore, *admin, "auto-def-"+suffix, true)

	router := grantRequestsRouter(server)

	w, resp := postGrantRequest(t, router, token, map[string]any{
		"grant_definition_id": def.UID.String(),
		"database_id":         db.UID.String(),
		"justification":       "investigating incident 42",
	})

	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())
	require.Equal(t, "approved", resp["status"], "request should be approved instantly")
	require.NotNil(t, resp["resulting_grant_id"], "resulting_grant_id should be set")
	require.Nil(t, resp["decided_by"], "decided_by should be nil — no human decider")

	// The grant itself must actually exist and be usable by the requester.
	grants, err := dataStore.ListGrants(context.Background(), store.GrantFilter{UserID: &requester.UID})
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, db.UID, grants[0].DatabaseID)

	// Audit trail: both grant_request.created and grant_request.approved,
	// the latter marking the automatic decision.
	events, err := dataStore.ListAuditEvents(context.Background(), store.AuditFilter{})
	require.NoError(t, err)

	var sawCreated, sawApprovedAuto bool

	for _, ev := range events {
		switch ev.EventType {
		case "grant_request.created":
			sawCreated = true
		case "grant_request.approved":
			var details map[string]any
			_ = json.Unmarshal(ev.Details, &details)

			if details["via"] == "auto_approve" {
				sawApprovedAuto = true
			}
		}
	}

	require.True(t, sawCreated, "expected a grant_request.created audit event")
	require.True(t, sawApprovedAuto, "expected a grant_request.approved audit event marked via=auto_approve")
}

func TestCreateGrantRequest_NonAutoApproveStaysPending(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "cgrp"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	createTestUser(t, dataStore, "req-"+suffix, "reqpass123", []string{store.RoleConnector})
	token := loginUser(t, server, "req-"+suffix, "reqpass123")

	db := createTestDBEntry(t, dataStore, "manual-db-"+suffix, true)
	def := createTestGrantDefinition(t, dataStore, *admin, "manual-def-"+suffix, false)

	router := grantRequestsRouter(server)

	w, resp := postGrantRequest(t, router, token, map[string]any{
		"grant_definition_id": def.UID.String(),
		"database_id":         db.UID.String(),
	})

	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())
	require.Equal(t, "pending", resp["status"])
	require.Nil(t, resp["resulting_grant_id"])
}

func TestCreateGrantRequest_AutoApproveRequiresJustification(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "cgrj"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	createTestUser(t, dataStore, "req-"+suffix, "reqpass123", []string{store.RoleConnector})
	token := loginUser(t, server, "req-"+suffix, "reqpass123")

	db := createTestDBEntry(t, dataStore, "auto-nojust-db-"+suffix, true)
	def := createTestGrantDefinition(t, dataStore, *admin, "auto-nojust-def-"+suffix, true)

	router := grantRequestsRouter(server)

	w, _ := postGrantRequest(t, router, token, map[string]any{
		"grant_definition_id": def.UID.String(),
		"database_id":         db.UID.String(),
	})

	require.Equal(t, http.StatusBadRequest, w.Code, "response body: %s", w.Body.String())
}

// TestListGrantRequests_EmbedsLiveDefinition pins the wire contract the
// grant-requests UI depends on: a request carries the **live** version of its
// definition, not the archived version its grant_definition_id points at once
// the definition has been edited.
//
// Without it, the UI is left resolving that uid against the definitions
// listing — which only ever returns live rows — and comes up empty, losing the
// definition's name and auto-approve state from the row.
func TestListGrantRequests_EmbedsLiveDefinition(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "lgrld"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "live-db-"+suffix, true)
	def := createTestGrantDefinition(t, dataStore, *admin, "live-def-"+suffix, false)

	_, err := dataStore.CreateGrantRequest(ctx, &store.GrantRequest{
		UserID:            admin.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	})
	require.NoError(t, err)

	// Enable auto-approve: archives def, inserts a successor.
	edited := *def
	edited.AutoApprove = true

	next, err := dataStore.UpdateGrantDefinition(ctx, &edited)
	require.NoError(t, err)
	require.NotEqual(t, def.UID, next.UID, "the edit should have been versioned")

	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/grant-requests", server.handleListGrantRequests)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/grant-requests", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())

	var resp struct {
		GrantRequests []struct {
			GrantDefinitionID string `json:"grant_definition_id"`
			Definition        *struct {
				UID         string `json:"uid"`
				Name        string `json:"name"`
				AutoApprove bool   `json:"auto_approve"`
			} `json:"definition"`
		} `json:"grant_requests"`
	}

	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.GrantRequests, 1)

	got := resp.GrantRequests[0]
	require.Equal(t, def.UID.String(), got.GrantDefinitionID, "the pinned version should be unchanged")
	require.NotNil(t, got.Definition, "the request should embed its definition")
	require.Equal(t, next.UID.String(), got.Definition.UID, "should embed the live version")
	require.Equal(t, next.Name, got.Definition.Name)
	require.True(t, got.Definition.AutoApprove, "the live version's auto_approve should be visible")
}
