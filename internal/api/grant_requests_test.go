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
		Name:            name,
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

func TestCreateGrantRequest_AutoApproveYieldsActiveGrantInstantly(t *testing.T) { //nolint:paralleltest // shared migration lock
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

func TestCreateGrantRequest_NonAutoApproveStaysPending(t *testing.T) { //nolint:paralleltest // shared migration lock
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

func TestCreateGrantRequest_AutoApproveRequiresJustification(t *testing.T) { //nolint:paralleltest // shared migration lock
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
