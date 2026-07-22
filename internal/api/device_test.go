package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// newDeviceTestRouter mounts the device authorization routes with the same
// middleware split as production (see server.go): the authorization and token
// endpoints are unauthenticated; the consent GET requires a session and the
// consent POST additionally requires Web Session or Basic Auth (never a plain
// API key).
func newDeviceTestRouter(server *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	router.POST("/api/v1/auth/device", server.handleDeviceAuthorization)
	router.POST("/api/v1/auth/device/token", server.handleDeviceToken)

	authed := router.Group("/api/v1/auth/device")
	authed.Use(server.authMiddleware())
	authed.GET("/consent", server.handleGetDeviceConsent)
	authed.POST("/consent", server.requireWebSessionOrBasicAuth(), server.handleDeviceConsent)

	return router
}

func doDeviceJSON(router *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
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

func createDeviceAuth(t *testing.T, router *gin.Engine, clientName string) DeviceAuthorizationResponse {
	t.Helper()

	w := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device", "", map[string]string{"client_name": clientName})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp DeviceAuthorizationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func pollDeviceToken(router *gin.Engine, deviceCode string) *httptest.ResponseRecorder {
	return doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device/token", "", map[string]string{
		"grant_type":  deviceGrantType,
		"device_code": deviceCode,
	})
}

// TestDeviceFlow_Approve exercises the full happy path: authorization request,
// consent-page fetch by user_code, approve, token poll (delivered exactly
// once), and confirms the minted token actually authenticates.
func TestDeviceFlow_Approve(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	approver := createTestUser(t, dataStore, "deviceapprove1", "approverpassword123", []string{"connector"})
	sessionToken := loginUser(t, server, "deviceapprove1", "approverpassword123")

	created := createDeviceAuth(t, router, "my-tool on host")
	assert.NotEmpty(t, created.DeviceCode)
	assert.NotEmpty(t, created.UserCode)
	assert.Contains(t, created.VerificationURIComplete, url.QueryEscape(created.UserCode))
	assert.Equal(t, devicePollIntervalSeconds, created.Interval)

	// The consent page fetches the public details before responding.
	getW := doDeviceJSON(router, http.MethodGet,
		"/api/v1/auth/device/consent?user_code="+url.QueryEscape(created.UserCode), sessionToken, nil)
	require.Equal(t, http.StatusOK, getW.Code)
	var info DeviceConsentInfo
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &info))
	assert.Equal(t, "my-tool on host", info.ClientName)
	assert.Equal(t, created.UserCode, info.UserCode)
	assert.Equal(t, store.DeviceAuthStatusPending, info.Status)

	respondW := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device/consent", sessionToken,
		map[string]any{"user_code": created.UserCode, "approve": true})
	require.Equal(t, http.StatusNoContent, respondW.Code, respondW.Body.String())

	pollW := pollDeviceToken(router, created.DeviceCode)
	require.Equal(t, http.StatusOK, pollW.Code, pollW.Body.String())
	var tok map[string]string
	require.NoError(t, json.Unmarshal(pollW.Body.Bytes(), &tok))
	assert.Equal(t, "Bearer", tok["token_type"])
	require.NotEmpty(t, tok["access_token"])
	assert.Regexp(t, `^dbb_`, tok["access_token"])

	// The minted key actually authenticates, and belongs to the approver.
	apiKey, err := dataStore.VerifyAPIKey(t.Context(), tok["access_token"])
	require.NoError(t, err)
	assert.Equal(t, approver.UID, apiKey.UserID)

	// Delivered exactly once: a second poll reports expired_token.
	secondW := pollDeviceToken(router, created.DeviceCode)
	require.Equal(t, http.StatusBadRequest, secondW.Code)
	var errResp map[string]string
	require.NoError(t, json.Unmarshal(secondW.Body.Bytes(), &errResp))
	assert.Equal(t, "expired_token", errResp["error"])
}

// TestDeviceFlow_Deny verifies a denied request yields access_denied and no key.
func TestDeviceFlow_Deny(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	createTestUser(t, dataStore, "devicedeny1", "denypassword123", []string{"connector"})
	sessionToken := loginUser(t, server, "devicedeny1", "denypassword123")

	created := createDeviceAuth(t, router, "denied-tool")

	respondW := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device/consent", sessionToken,
		map[string]any{"user_code": created.UserCode, "approve": false})
	require.Equal(t, http.StatusNoContent, respondW.Code)

	pollW := pollDeviceToken(router, created.DeviceCode)
	require.Equal(t, http.StatusBadRequest, pollW.Code)
	var errResp map[string]string
	require.NoError(t, json.Unmarshal(pollW.Body.Bytes(), &errResp))
	assert.Equal(t, "access_denied", errResp["error"])
}

// TestDeviceToken_Pending verifies polling a still-pending request returns
// authorization_pending.
func TestDeviceToken_Pending(t *testing.T) { //nolint:paralleltest // shared database state
	server, _ := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	created := createDeviceAuth(t, router, "pending-tool")

	pollW := pollDeviceToken(router, created.DeviceCode)
	require.Equal(t, http.StatusBadRequest, pollW.Code)
	var errResp map[string]string
	require.NoError(t, json.Unmarshal(pollW.Body.Bytes(), &errResp))
	assert.Equal(t, "authorization_pending", errResp["error"])
}

// TestDeviceConsent_NormalizesUserCode verifies a user code typed in a
// different case / without the dash still resolves.
func TestDeviceConsent_NormalizesUserCode(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	createTestUser(t, dataStore, "devicenorm1", "normpassword123", []string{"connector"})
	sessionToken := loginUser(t, server, "devicenorm1", "normpassword123")

	created := createDeviceAuth(t, router, "norm-tool")
	// Lowercase + strip the dash — the server must normalize this back.
	messy := strings.ToLower(strings.ReplaceAll(created.UserCode, "-", ""))

	respondW := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device/consent", sessionToken,
		map[string]any{"user_code": messy, "approve": true})
	require.Equal(t, http.StatusNoContent, respondW.Code, respondW.Body.String())

	pollW := pollDeviceToken(router, created.DeviceCode)
	require.Equal(t, http.StatusOK, pollW.Code)
}

// TestDeviceConsent_RequiresWebSessionOrBasicAuth verifies a plain API key
// cannot approve — only a web session or Basic Auth can, like direct API key
// creation.
func TestDeviceConsent_RequiresWebSessionOrBasicAuth(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	user := createTestUser(t, dataStore, "deviceapikey1", "apikeypassword123", []string{"connector"})
	_, plainAPIKey, err := dataStore.CreateAPIKey(t.Context(), user.UID, "not a web session", nil)
	require.NoError(t, err)

	created := createDeviceAuth(t, router, "blocked-tool")

	respondW := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device/consent", plainAPIKey,
		map[string]any{"user_code": created.UserCode, "approve": true})
	assert.Equal(t, http.StatusForbidden, respondW.Code)

	// Still pending — the forbidden attempt did nothing.
	getW := doDeviceJSON(router, http.MethodGet,
		"/api/v1/auth/device/consent?user_code="+url.QueryEscape(created.UserCode), plainAPIKey, nil)
	require.Equal(t, http.StatusOK, getW.Code)
	var info DeviceConsentInfo
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &info))
	assert.Equal(t, store.DeviceAuthStatusPending, info.Status)
}

// TestDeviceConsent_DoubleRespond verifies responding twice is rejected with a
// conflict, not a second key.
func TestDeviceConsent_DoubleRespond(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	createTestUser(t, dataStore, "devicedouble1", "doublepassword123", []string{"connector"})
	sessionToken := loginUser(t, server, "devicedouble1", "doublepassword123")

	created := createDeviceAuth(t, router, "double-tool")

	firstW := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device/consent", sessionToken,
		map[string]any{"user_code": created.UserCode, "approve": true})
	require.Equal(t, http.StatusNoContent, firstW.Code)

	secondW := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device/consent", sessionToken,
		map[string]any{"user_code": created.UserCode, "approve": false})
	assert.Equal(t, http.StatusConflict, secondW.Code)
}

// TestDeviceToken_UnknownDeviceCode verifies an unknown device_code reports
// expired_token rather than leaking whether it ever existed.
func TestDeviceToken_UnknownDeviceCode(t *testing.T) { //nolint:paralleltest // shared database state
	server, _ := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	pollW := pollDeviceToken(router, "never-issued-device-code")
	require.Equal(t, http.StatusBadRequest, pollW.Code)
	var errResp map[string]string
	require.NoError(t, json.Unmarshal(pollW.Body.Bytes(), &errResp))
	assert.Equal(t, "expired_token", errResp["error"])
}

// TestDeviceToken_BadGrantType verifies a wrong grant_type is rejected.
func TestDeviceToken_BadGrantType(t *testing.T) { //nolint:paralleltest // shared database state
	server, _ := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	created := createDeviceAuth(t, router, "grant-tool")

	w := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device/token", "", map[string]string{
		"grant_type":  "authorization_code",
		"device_code": created.DeviceCode,
	})
	require.Equal(t, http.StatusBadRequest, w.Code)
	var errResp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, "unsupported_grant_type", errResp["error"])
}

// TestDeviceAuthorization_Validation verifies the client_name bound and the
// empty-body default.
func TestDeviceAuthorization_Validation(t *testing.T) { //nolint:paralleltest // shared database state
	server, _ := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	router := newDeviceTestRouter(server)

	t.Run("empty body defaults client name", func(t *testing.T) { //nolint:paralleltest // shared server/router state
		w := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device", "", nil)
		require.Equal(t, http.StatusOK, w.Code)
		var resp DeviceAuthorizationResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.NotEmpty(t, resp.DeviceCode)
	})

	t.Run("client_name too long", func(t *testing.T) { //nolint:paralleltest // shared server/router state
		long := strings.Repeat("a", deviceClientNameMaxLength+1)
		w := doDeviceJSON(router, http.MethodPost, "/api/v1/auth/device", "", map[string]string{"client_name": long})
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// TestNormalizeUserCode covers the input canonicalization directly.
func TestNormalizeUserCode(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "WDJP4KXR", normalizeUserCode("WDJP-4KXR"))
	assert.Equal(t, "WDJP4KXR", normalizeUserCode("wdjp-4kxr"))
	assert.Equal(t, "WDJP4KXR", normalizeUserCode("  wdjp 4kxr  "))
	assert.Equal(t, "WDJP4KXR", normalizeUserCode("WDJP4KXR"))
	assert.Empty(t, normalizeUserCode(""))
}

// TestFormatUserCode covers the canonical->display formatting.
func TestFormatUserCode(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "WDJP-4KXR", formatUserCode("WDJP4KXR"))
	// Non-canonical length is returned unchanged.
	assert.Equal(t, "SHORT", formatUserCode("SHORT"))
}
