package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// newTerminateTestRouter mounts the terminate route with the same middleware
// chain as production — authMiddleware then requireAdmin — so the role gate
// under test is the real one rather than a hand-rolled lookalike.
func newTerminateTestRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/connections/:uid/terminate", server.requireAdmin(), server.handleTerminateConnection)

	return router
}

func doTerminate(router *gin.Engine, token, uid, body string) *httptest.ResponseRecorder {
	var reader interface {
		Read([]byte) (int, error)
	} = http.NoBody

	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+uid+"/terminate", reader)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

// TestTerminateConnection_LiveSessionAccepted is the happy path: a live session
// answers 202 and the request lands on the row, where the owning replica's
// poller will find it.
func TestTerminateConnection_LiveSessionAccepted(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tclive"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db_"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.2.2.1")
	require.NoError(t, err)

	router := newTerminateTestRouter(server)
	w := doTerminate(router, token, conn.UID.String(), `{"reason":"runaway report"}`)

	require.Equal(t, http.StatusAccepted, w.Code, "response body: %s", w.Body.String())

	var body struct {
		Message string `json:"message"`
		Local   bool   `json:"local"`
	}

	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Message)
	// No proxy session registered in this test, so nothing local was signaled.
	require.False(t, body.Local)

	// The request columns are what carry the intent to the replica that owns
	// the session, so they are the assertion that matters.
	reloaded, err := dataStore.GetConnectionByUID(t.Context(), conn.UID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.TerminateRequestedAt)
	require.NotNil(t, reloaded.TerminateRequestedBy)
	require.Equal(t, admin.UID, *reloaded.TerminateRequestedBy)
	require.NotNil(t, reloaded.TerminateReason)
	require.Equal(t, "runaway report", *reloaded.TerminateReason)

	// The session itself is untouched: this is a request, not a close.
	require.Nil(t, reloaded.DisconnectedAt)
	require.Nil(t, reloaded.TerminationReason)
}

// TestTerminateConnection_ClosedSessionConflict verifies a session that has
// already ended answers 409, not 404 — "no such session" and "nothing left to
// end" are different answers.
func TestTerminateConnection_ClosedSessionConflict(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tcclosed"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db_"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.2.2.2")
	require.NoError(t, err)
	require.NoError(t, dataStore.CloseConnection(t.Context(), conn.UID))

	router := newTerminateTestRouter(server)
	w := doTerminate(router, token, conn.UID.String(), "")

	require.Equal(t, http.StatusConflict, w.Code, "response body: %s", w.Body.String())

	reloaded, err := dataStore.GetConnectionByUID(t.Context(), conn.UID)
	require.NoError(t, err)
	require.Nil(t, reloaded.TerminateRequestedAt, "a refused request must not be recorded")
}

// TestTerminateConnection_UnknownUID verifies an unknown uid is a 404.
func TestTerminateConnection_UnknownUID(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tcnf"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := newTerminateTestRouter(server)
	w := doTerminate(router, token, "00000000-0000-0000-0000-000000000000", "")

	require.Equal(t, http.StatusNotFound, w.Code, "response body: %s", w.Body.String())
}

// TestTerminateConnection_InvalidUID verifies a malformed uid is a 400.
func TestTerminateConnection_InvalidUID(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tcbad"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := newTerminateTestRouter(server)
	w := doTerminate(router, token, "not-a-uuid", "")

	require.Equal(t, http.StatusBadRequest, w.Code, "response body: %s", w.Body.String())
}

// TestTerminateConnection_AdminOnly verifies the role gate: ending somebody
// else's live database session is not something a viewer or a connector may do,
// including the connector who owns the session.
func TestTerminateConnection_AdminOnly(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tcrole"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "viewer-"+suffix, "viewerpass123", []string{store.RoleViewer})

	ownerToken := loginUser(t, server, "owner-"+suffix, "ownerpass123")
	viewerToken := loginUser(t, server, "viewer-"+suffix, "viewerpass123")

	db := createTestDBEntry(t, dataStore, "db_"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.2.2.3")
	require.NoError(t, err)

	router := newTerminateTestRouter(server)

	for name, token := range map[string]string{"viewer": viewerToken, "owner": ownerToken} {
		w := doTerminate(router, token, conn.UID.String(), "")
		require.Equal(t, http.StatusForbidden, w.Code, "%s response body: %s", name, w.Body.String())
	}

	reloaded, err := dataStore.GetConnectionByUID(t.Context(), conn.UID)
	require.NoError(t, err)
	require.Nil(t, reloaded.TerminateRequestedAt)
}

// TestTerminateConnection_ReasonTooLong verifies the free text is bounded: the
// column is not a place to paste a log.
func TestTerminateConnection_ReasonTooLong(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tclong"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db_"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.2.2.4")
	require.NoError(t, err)

	body, err := json.Marshal(map[string]string{
		"reason": strings.Repeat("x", maxTerminateReasonLength+1),
	})
	require.NoError(t, err)

	router := newTerminateTestRouter(server)
	w := doTerminate(router, token, conn.UID.String(), string(body))

	require.Equal(t, http.StatusBadRequest, w.Code, "response body: %s", w.Body.String())
}

// TestTerminateConnection_SignalsLocalSession verifies the fast path: when this
// replica is the one serving the session, the handler signals the registry
// itself rather than leaving the admin to wait for a poll tick.
func TestTerminateConnection_SignalsLocalSession(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tclocal"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db_"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.2.2.5")
	require.NoError(t, err)

	// Stand in for a live proxy session on this process.
	handle := dataStore.Sessions().Register(conn.UID)
	t.Cleanup(func() { dataStore.Sessions().Deregister(conn.UID, handle) })

	router := newTerminateTestRouter(server)
	w := doTerminate(router, token, conn.UID.String(), `{"reason":"blocking the migration"}`)

	require.Equal(t, http.StatusAccepted, w.Code, "response body: %s", w.Body.String())

	var body struct {
		Local bool `json:"local"`
	}

	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.True(t, body.Local)

	require.True(t, handle.Terminated())

	req := handle.Request()
	require.Equal(t, store.TerminationAdminTerminated, req.Reason)
	require.Equal(t, "admin-"+suffix, req.By)
	require.Equal(t, "blocking the migration", req.Detail)
}

// TestTerminateConnection_NoReasonStoresNull verifies an empty reason is stored
// as NULL rather than as an empty string that reads like one.
func TestTerminateConnection_NoReasonStoresNull(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tcnoreason"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db_"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.2.2.6")
	require.NoError(t, err)

	router := newTerminateTestRouter(server)
	w := doTerminate(router, token, conn.UID.String(), "")

	require.Equal(t, http.StatusAccepted, w.Code, "response body: %s", w.Body.String())

	reloaded, err := dataStore.GetConnectionByUID(t.Context(), conn.UID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.TerminateRequestedAt)
	require.Nil(t, reloaded.TerminateReason)
}

// TestGetConnection_ExposesTerminatedBy verifies the detail response names the
// admin who asked, so the page can say "terminated by alice" instead of showing
// a bare uuid.
func TestGetConnection_ExposesTerminatedBy(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "tcby"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db_"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.2.2.7")
	require.NoError(t, err)

	_, err = dataStore.RequestConnectionTermination(t.Context(), conn.UID, admin.UID, "too slow")
	require.NoError(t, err)

	w := doGetConnection(newConnectionsTestRouter(server), token, conn.UID.String())
	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())

	var got struct {
		TerminatedBy *TerminationRequester `json:"terminated_by"`
		Reason       *string               `json:"terminate_reason"`
	}

	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.NotNil(t, got.TerminatedBy)
	require.Equal(t, admin.UID, got.TerminatedBy.UID)
	require.Equal(t, "admin-"+suffix, got.TerminatedBy.Username)
	require.NotNil(t, got.Reason)
	require.Equal(t, "too slow", *got.Reason)
}
