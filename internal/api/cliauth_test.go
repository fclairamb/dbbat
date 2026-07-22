package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// newCLIAuthTestRouter mounts the CLI authorization routes with the same
// middleware split as production (see server.go): create/poll are
// unauthenticated, get/respond require a session (respond additionally
// requires Web Session or Basic Auth, never a plain API key).
func newCLIAuthTestRouter(server *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	router.POST("/api/v1/auth/cli", server.handleCreateCLIAuthRequest)
	router.POST("/api/v1/auth/cli/poll", server.handlePollCLIAuthRequest)

	authed := router.Group("/api/v1/auth/cli")
	authed.Use(server.authMiddleware())
	authed.GET("/:uid", server.handleGetCLIAuthRequest)
	authed.POST("/:uid/respond", server.requireWebSessionOrBasicAuth(), server.handleRespondToCLIAuthRequest)

	return router
}

func doCLIAuthJSON(router *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func createCLIAuthRequest(t *testing.T, router *gin.Engine, name string) CreateCLIAuthRequestResponse {
	t.Helper()

	w := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli", "", map[string]string{"name": name})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var resp CreateCLIAuthRequestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

// TestCLIAuthFlow_Approve exercises the full happy path: create, fetch for
// display, approve, poll for the key (delivered exactly once), and confirms
// the minted key actually authenticates.
func TestCLIAuthFlow_Approve(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newCLIAuthTestRouter(server)

	approver := createTestUser(t, dataStore, "cliauthapprove1", "approverpassword123", []string{"connector"})
	sessionToken := loginUser(t, server, "cliauthapprove1", "approverpassword123")

	created := createCLIAuthRequest(t, router, "my-tool on host")
	assert.NotEmpty(t, created.PollToken)
	assert.NotEmpty(t, created.UserCode)
	assert.Contains(t, created.AuthorizeURL, created.RequestID.String())

	// The approval page fetches the public details before responding.
	getW := doCLIAuthJSON(router, http.MethodGet, "/api/v1/auth/cli/"+created.RequestID.String(), sessionToken, nil)
	require.Equal(t, http.StatusOK, getW.Code)
	var info CLIAuthRequestInfo
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &info))
	assert.Equal(t, "my-tool on host", info.Name)
	assert.Equal(t, created.UserCode, info.UserCode)
	assert.Equal(t, store.CLIAuthStatusPending, info.Status)

	respondW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/"+created.RequestID.String()+"/respond", sessionToken,
		map[string]bool{"approve": true})
	require.Equal(t, http.StatusNoContent, respondW.Code, respondW.Body.String())

	pollW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/poll", "", map[string]string{"poll_token": created.PollToken})
	require.Equal(t, http.StatusOK, pollW.Code)
	var pollResp map[string]string
	require.NoError(t, json.Unmarshal(pollW.Body.Bytes(), &pollResp))
	assert.Equal(t, "approved", pollResp["status"])
	require.NotEmpty(t, pollResp["key"])
	assert.Regexp(t, `^dbb_`, pollResp["key"])

	// The minted key actually authenticates, and belongs to the approver.
	apiKey, err := dataStore.VerifyAPIKey(t.Context(), pollResp["key"])
	require.NoError(t, err)
	assert.Equal(t, approver.UID, apiKey.UserID)

	// Delivered exactly once: a second poll no longer finds the request.
	secondPollW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/poll", "", map[string]string{"poll_token": created.PollToken})
	require.Equal(t, http.StatusOK, secondPollW.Code)
	var secondPollResp map[string]string
	require.NoError(t, json.Unmarshal(secondPollW.Body.Bytes(), &secondPollResp))
	assert.Equal(t, "expired", secondPollResp["status"])
}

// TestCLIAuthFlow_Deny verifies a denied request never yields a key and is
// also consumed after one poll.
func TestCLIAuthFlow_Deny(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newCLIAuthTestRouter(server)

	createTestUser(t, dataStore, "cliauthdeny1", "denypassword123", []string{"connector"})
	sessionToken := loginUser(t, server, "cliauthdeny1", "denypassword123")

	created := createCLIAuthRequest(t, router, "denied-tool")

	respondW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/"+created.RequestID.String()+"/respond", sessionToken,
		map[string]bool{"approve": false})
	require.Equal(t, http.StatusNoContent, respondW.Code)

	pollW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/poll", "", map[string]string{"poll_token": created.PollToken})
	require.Equal(t, http.StatusOK, pollW.Code)
	var pollResp map[string]string
	require.NoError(t, json.Unmarshal(pollW.Body.Bytes(), &pollResp))
	assert.Equal(t, "denied", pollResp["status"])
	assert.Empty(t, pollResp["key"])
}

// TestCLIAuthRespond_RequiresWebSessionOrBasicAuth verifies a plain API key
// cannot approve a request — only a web session or Basic Auth can, exactly
// like direct API key creation.
func TestCLIAuthRespond_RequiresWebSessionOrBasicAuth(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newCLIAuthTestRouter(server)

	user := createTestUser(t, dataStore, "cliauthapikey1", "apikeypassword123", []string{"connector"})
	_, plainAPIKey, err := dataStore.CreateAPIKey(t.Context(), user.UID, "not a web session", nil)
	require.NoError(t, err)

	created := createCLIAuthRequest(t, router, "blocked-tool")

	respondW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/"+created.RequestID.String()+"/respond", plainAPIKey,
		map[string]bool{"approve": true})
	assert.Equal(t, http.StatusForbidden, respondW.Code)

	// The request must still be pending — the forbidden attempt did nothing.
	getW := doCLIAuthJSON(router, http.MethodGet, "/api/v1/auth/cli/"+created.RequestID.String(), plainAPIKey, nil)
	require.Equal(t, http.StatusOK, getW.Code)
	var info CLIAuthRequestInfo
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &info))
	assert.Equal(t, store.CLIAuthStatusPending, info.Status)
}

// TestCLIAuthRespond_DoubleRespond verifies responding to an
// already-resolved request is rejected with a conflict, not a second key.
func TestCLIAuthRespond_DoubleRespond(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newCLIAuthTestRouter(server)

	createTestUser(t, dataStore, "cliauthdouble1", "doublepassword123", []string{"connector"})
	sessionToken := loginUser(t, server, "cliauthdouble1", "doublepassword123")

	created := createCLIAuthRequest(t, router, "double-respond-tool")

	firstW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/"+created.RequestID.String()+"/respond", sessionToken,
		map[string]bool{"approve": true})
	require.Equal(t, http.StatusNoContent, firstW.Code)

	secondW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/"+created.RequestID.String()+"/respond", sessionToken,
		map[string]bool{"approve": false})
	assert.Equal(t, http.StatusConflict, secondW.Code)
}

// TestCLIAuthPoll_UnknownToken verifies polling with a token that was never
// issued reports "expired" rather than leaking whether it ever existed.
func TestCLIAuthPoll_UnknownToken(t *testing.T) { //nolint:paralleltest // shared database state
	server, _ := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newCLIAuthTestRouter(server)

	pollW := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli/poll", "", map[string]string{"poll_token": "never-issued"})
	require.Equal(t, http.StatusOK, pollW.Code)
	var pollResp map[string]string
	require.NoError(t, json.Unmarshal(pollW.Body.Bytes(), &pollResp))
	assert.Equal(t, "expired", pollResp["status"])
}

// TestCLIAuthCreate_Validation verifies the name field is required and bounded.
func TestCLIAuthCreate_Validation(t *testing.T) { //nolint:paralleltest // shared database state
	server, _ := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newCLIAuthTestRouter(server)

	t.Run("missing name", func(t *testing.T) { //nolint:paralleltest // shared server/router state
		w := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli", "", map[string]string{})
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("name too long", func(t *testing.T) { //nolint:paralleltest // shared server/router state
		longName := make([]byte, cliAuthNameMaxLength+1)
		for i := range longName {
			longName[i] = 'a'
		}
		w := doCLIAuthJSON(router, http.MethodPost, "/api/v1/auth/cli", "", map[string]string{"name": string(longName)})
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}
