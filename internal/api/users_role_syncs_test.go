package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/store"
)

// roleSyncsResponse mirrors the documented shape of GET /users/role-syncs.
type roleSyncsResponse struct {
	RoleSyncs []struct {
		UID       string         `json:"uid"`
		EventType string         `json:"event_type"`
		UserID    string         `json:"user_id"`
		Username  string         `json:"username"`
		Details   map[string]any `json:"details"`
		CreatedAt string         `json:"created_at"`
	} `json:"role_syncs"`
}

// seedRoleSync writes the audit entry an OIDC login leaves behind, exactly as
// auditRoleSync does.
func seedRoleSync(t *testing.T, dataStore *store.Store, user *store.User, groups []string) {
	t.Helper()

	details, err := json.Marshal(map[string]any{
		"provider": "oidc",
		"groups":   groups,
		"roles":    []string(user.Roles),
		"granted":  []string{"viewer"},
		"revoked":  []string{},
	})
	if err != nil {
		t.Fatalf("failed to encode role sync details: %v", err)
	}

	if err := dataStore.LogAuditEvent(context.Background(), &store.AuditEvent{
		EventType: AuditEventOAuthRolesSynced,
		UserID:    &user.UID,
		Details:   details,
	}); err != nil {
		t.Fatalf("failed to log role sync: %v", err)
	}
}

func doGet(t *testing.T, router *gin.Engine, token, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

// TestListRoleSyncs_LatestPerUser exercises the endpoint through the *real*
// router, so it also asserts what a hand-mounted route never could: that the
// literal /users/role-syncs segment coexists with /users/:uid, both at
// registration time (gin panics on a genuine conflict) and at match time.
func TestListRoleSyncs_LatestPerUser(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	admin := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	viewer := createTestUser(t, dataStore, "viewer", "viewerpassword123", []string{"viewer"})
	// Never synced: this one must be absent from the response, which is what
	// lets the UI read absence as "never synced".
	createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})

	// Two syncs for the same user; only the newest may come back.
	seedRoleSync(t, dataStore, viewer, []string{"stale"})
	seedRoleSync(t, dataStore, viewer, []string{"analysts", "on-call"})

	token := loginUser(t, server, "admin", "adminpassword123")
	router := server.setupRouter()

	w := doGet(t, router, token, "/api/v1/users/role-syncs")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /users/role-syncs = %d, want 200: %s", w.Code, w.Body.String())
	}

	var resp roleSyncsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if len(resp.RoleSyncs) != 1 {
		t.Fatalf("got %d role syncs, want 1: %s", len(resp.RoleSyncs), w.Body.String())
	}

	got := resp.RoleSyncs[0]
	if got.UserID != viewer.UID.String() {
		t.Errorf("role sync user_id = %q, want %q", got.UserID, viewer.UID)
	}
	if got.Username != "viewer" {
		t.Errorf("role sync username = %q, want %q", got.Username, "viewer")
	}
	if got.EventType != AuditEventOAuthRolesSynced {
		t.Errorf("role sync event_type = %q, want %q", got.EventType, AuditEventOAuthRolesSynced)
	}
	if got.UID == "" {
		t.Error("role sync uid is empty; the audit entry's UID must come back")
	}
	if got.CreatedAt == "" {
		t.Error("role sync created_at is empty")
	}

	// The groups are the answer to "why did this change?" and must survive.
	groups, _ := got.Details["groups"].([]any)
	if len(groups) != 2 || groups[0] != "analysts" || groups[1] != "on-call" {
		t.Errorf("role sync details groups = %v, want [analysts on-call] (the newest entry)", groups)
	}

	// The param route at the same level still resolves.
	w = doGet(t, router, token, "/api/v1/users/"+admin.UID.String())
	if w.Code != http.StatusOK {
		t.Fatalf("GET /users/{uid} = %d, want 200: %s", w.Code, w.Body.String())
	}

	var user struct {
		UID      string `json:"uid"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &user); err != nil {
		t.Fatalf("failed to parse user response: %v", err)
	}
	if user.UID != admin.UID.String() || user.Username != "admin" {
		t.Errorf("GET /users/{uid} returned %+v, want the admin user", user)
	}
}

// TestListRoleSyncs_RequiresAdminOrViewer pins the gating to the audit list the
// endpoint reads from: a connector may not enumerate who the directory manages.
func TestListRoleSyncs_RequiresAdminOrViewer(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	viewer := createTestUser(t, dataStore, "viewer", "viewerpassword123", []string{"viewer"})
	createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})
	seedRoleSync(t, dataStore, viewer, []string{"analysts"})

	router := server.setupRouter()

	viewerToken := loginUser(t, server, "viewer", "viewerpassword123")
	if w := doGet(t, router, viewerToken, "/api/v1/users/role-syncs"); w.Code != http.StatusOK {
		t.Errorf("viewer GET /users/role-syncs = %d, want 200: %s", w.Code, w.Body.String())
	}

	connectorToken := loginUser(t, server, "connector", "connectorpassword123")
	if w := doGet(t, router, connectorToken, "/api/v1/users/role-syncs"); w.Code != http.StatusForbidden {
		t.Errorf("connector GET /users/role-syncs = %d, want 403: %s", w.Code, w.Body.String())
	}
}
