package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// listAuditAs runs GET /audit?event_type=user.updated as the holder of token
// and returns the body.
func listAuditAs(t *testing.T, server *Server, token string) string {
	t.Helper()

	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/audit", server.requireAdminOrViewer(), server.handleListAudit)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit?event_type="+auditEventUserUpdated, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	return w.Body.String()
}

// TestUpdateUser_AuditNeverStoresPlaintextPassword: a password set through
// PUT /users/:uid must not reach audit_log, which viewers can read.
func TestUpdateUser_AuditNeverStoresPlaintextPassword(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	createTestUser(t, dataStore, "admin", "adminpassword123", []string{store.RoleAdmin})
	target := createTestUser(t, dataStore, "target", "targetpassword123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "auditor", "auditorpassword123", []string{store.RoleViewer})
	adminToken := loginUser(t, server, "admin", "adminpassword123")

	const secret = "Tr0ub4dor-plaintext-canary"

	w := doUpdateUserPassword(newUsersTestRouter(server), adminToken, target.UID.String(), secret)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	events, err := dataStore.ListAuditEvents(ctx, store.AuditFilter{
		EventType: ptrOf(auditEventUserUpdated), UserID: &target.UID,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.NotContains(t, string(events[0].Details), secret, "the stored entry must not hold the password")

	var details struct {
		UpdatedFields map[string]any `json:"updated_fields"`
	}
	require.NoError(t, json.Unmarshal(events[0].Details, &details))
	assert.Equal(t, true, details.UpdatedFields["password_changed"], "the entry still says the password changed")
	assert.NotContains(t, details.UpdatedFields, "password")

	body := listAuditAs(t, server, loginUser(t, server, "auditor", "auditorpassword123"))
	assert.NotContains(t, body, secret)

	// Positive control: a roles-only update records the roles and no
	// password_changed.
	w = doUpdateUserRoles(newUsersTestRouter(server), adminToken, target.UID.String(), []string{store.RoleViewer})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	events, err = dataStore.ListAuditEvents(ctx, store.AuditFilter{
		EventType: ptrOf(auditEventUserUpdated), UserID: &target.UID,
	})
	require.NoError(t, err)
	require.Len(t, events, 2)

	for _, e := range events {
		if !strings.Contains(string(e.Details), "password_changed") {
			assert.Contains(t, string(e.Details), store.RoleViewer)
		}
	}
}

// TestListAudit_RedactsLegacyUserUpdatedPasswords: entries written by earlier
// versions hold the plaintext and cannot be rewritten without breaking the
// chain, so the listing masks them when it serves them.
func TestListAudit_RedactsLegacyUserUpdatedPasswords(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	createTestUser(t, dataStore, "auditor", "auditorpassword123", []string{store.RoleViewer})
	target := createTestUser(t, dataStore, "target", "targetpassword123", []string{store.RoleConnector})

	const secret = "legacy-plaintext-canary"

	legacy, err := json.Marshal(map[string]any{"updated_fields": map[string]any{
		"password": secret, "roles": []string{store.RoleConnector}, "user_group_uids": nil, "group_uids": nil,
	}})
	require.NoError(t, err)
	require.NoError(t, dataStore.LogAuditEvent(ctx, &store.AuditEvent{
		EventType: auditEventUserUpdated, UserID: &target.UID, Details: legacy,
	}))

	// A roles-only legacy entry, and another event type that happens to carry
	// a "password" key: neither is touched.
	rolesOnly, err := json.Marshal(map[string]any{"updated_fields": map[string]any{
		"password": nil, "roles": []string{store.RoleViewer},
	}})
	require.NoError(t, err)
	require.NoError(t, dataStore.LogAuditEvent(ctx, &store.AuditEvent{
		EventType: auditEventUserUpdated, UserID: &target.UID, Details: rolesOnly,
	}))

	body := listAuditAs(t, server, loginUser(t, server, "auditor", "auditorpassword123"))
	assert.NotContains(t, body, secret, "a viewer must not read a legacy plaintext password")
	assert.Contains(t, body, `"password_changed":true`)
	assert.Contains(t, body, store.RoleViewer, "the rest of the entry is still served")

	var resp struct {
		Events []struct {
			Details map[string]map[string]any `json:"details"`
		} `json:"audit_events"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Len(t, resp.Events, 2)

	for _, e := range resp.Events {
		assert.NotContains(t, e.Details["updated_fields"], "password")
	}

	// The stored rows are untouched, so the chain still verifies.
	stored, err := dataStore.ListAuditEvents(ctx, store.AuditFilter{
		EventType: ptrOf(auditEventUserUpdated), UserID: &target.UID,
	})
	require.NoError(t, err)

	found := false
	for _, e := range stored {
		found = found || strings.Contains(string(e.Details), secret)
	}
	assert.True(t, found, "the listing masks the copy it serves, never the stored row")
}

func TestRedactUserUpdatedDetails(t *testing.T) {
	t.Parallel()

	out, changed := redactUserUpdatedDetails(json.RawMessage(`{"updated_fields":{"password":"x","roles":["admin"]}}`))
	assert.True(t, changed)
	assert.JSONEq(t, `{"updated_fields":{"password_changed":true,"roles":["admin"]}}`, string(out))

	out, changed = redactUserUpdatedDetails(json.RawMessage(`{"updated_fields":{"password":null,"roles":["admin"]}}`))
	assert.True(t, changed)
	assert.JSONEq(t, `{"updated_fields":{"roles":["admin"]}}`, string(out))

	in := json.RawMessage(`{"updated_fields":{"roles":["admin"],"password_changed":true}}`)
	out, changed = redactUserUpdatedDetails(in)
	assert.False(t, changed)
	assert.Equal(t, string(in), string(out))

	out, changed = redactUserUpdatedDetails(json.RawMessage(`{"updated_fields":"password=x"}`))
	assert.True(t, changed)
	assert.NotContains(t, string(out), "x\"")
}
