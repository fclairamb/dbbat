package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// OAuth 2.0 Device Authorization Grant (RFC 8628) constants.
const (
	deviceClientNameMaxLength = 200
	devicePollIntervalSeconds = 2
	deviceUserCodeLength      = 8
	deviceUserCodeAttempts    = 5

	// deviceGrantType is the RFC 8628 grant_type the token endpoint accepts.
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// deviceDefaultClientName labels a request that didn't send a client_name.
	deviceDefaultClientName = "Unknown application"
)

// errExhaustedUserCodes means user-code generation kept colliding with live
// requests — practically impossible given the code entropy, so it's a 500.
var errExhaustedUserCodes = errors.New("exhausted user code attempts")

// deviceUserCodeAlphabet excludes visually ambiguous characters (0/O, 1/I/L)
// so the code can be read aloud or retyped without confusion.
const deviceUserCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// DeviceAuthorizationRequest is the request body for POST /auth/device.
// client_name is a dbbat extension used for the consent-page label; client_id
// is accepted for OAuth compatibility but ignored (dbbat is not a multi-client
// authorization server).
type DeviceAuthorizationRequest struct {
	ClientName string `json:"client_name"`
	ClientID   string `json:"client_id"`
}

// DeviceAuthorizationResponse is the RFC 8628 device authorization response.
type DeviceAuthorizationResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// handleDeviceAuthorization starts a new device authorization request
// (RFC 8628 §3.1-3.2). Unauthenticated: anyone can ask for a request to be
// opened, but it grants nothing until a logged-in user approves it.
// POST /api/v1/auth/device
func (s *Server) handleDeviceAuthorization(c *gin.Context) {
	var req DeviceAuthorizationRequest
	// A body is optional; tolerate an empty one.
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, http.ErrBodyReadAfterClose) {
		req = DeviceAuthorizationRequest{}
	}

	clientName := strings.TrimSpace(req.ClientName)
	if clientName == "" {
		clientName = deviceDefaultClientName
	}
	if len(clientName) > deviceClientNameMaxLength {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "client_name too long")
		return
	}

	ctx := c.Request.Context()

	var created *store.DeviceAuthRequest
	var deviceCode string
	for attempt := 0; attempt < deviceUserCodeAttempts; attempt++ {
		dc, err := generateRandomState()
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to generate device code")
			return
		}

		userCode, err := generateDeviceUserCode()
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to generate user code")
			return
		}

		created, err = s.store.CreateDeviceAuthRequest(ctx, clientName, dc, userCode)
		if err == nil {
			deviceCode = dc
			break
		}
		if errors.Is(err, store.ErrDeviceAuthUserCodeTaken) {
			created = nil
			continue
		}
		writeInternalError(c, s.logger, err, "failed to create device authorization request")
		return
	}
	if created == nil {
		writeInternalError(c, s.logger, errExhaustedUserCodes, "failed to allocate a unique user code")
		return
	}

	details, _ := json.Marshal(map[string]interface{}{"client_name": clientName, "request_id": created.UID})
	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType: "device_auth.requested",
		Details:   details,
	})

	displayCode := formatUserCode(created.UserCode)
	verificationURI := s.buildFrontendURL(c.Request, "/device")

	c.JSON(http.StatusOK, DeviceAuthorizationResponse{
		DeviceCode:              deviceCode,
		UserCode:                displayCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: verificationURI + "?user_code=" + url.QueryEscape(displayCode),
		ExpiresIn:               int(time.Until(created.ExpiresAt).Seconds()),
		Interval:                devicePollIntervalSeconds,
	})
}

// DeviceTokenRequest is the request body for POST /auth/device/token.
type DeviceTokenRequest struct {
	GrantType  string `json:"grant_type" binding:"required"`
	DeviceCode string `json:"device_code" binding:"required"`
	ClientID   string `json:"client_id"`
}

// handleDeviceToken is the RFC 8628 token endpoint (§3.4-3.5). The client polls
// it with the device_code; it answers with OAuth error codes while pending and
// a Bearer token once approved. Deliberately not IP rate limited: a legitimate
// client polls this every few seconds for the request's TTL, and the
// device_code (32 random bytes) is the capability — brute-forcing it is
// infeasible.
// POST /api/v1/auth/device/token
func (s *Server) handleDeviceToken(c *gin.Context) {
	var req DeviceTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeOAuthError(c, "invalid_request", "grant_type and device_code are required")
		return
	}

	if req.GrantType != deviceGrantType {
		writeOAuthError(c, "unsupported_grant_type", "unsupported grant_type")
		return
	}

	ctx := c.Request.Context()
	deviceReq, encryptedKey, err := s.store.PollDeviceAuthToken(ctx, req.DeviceCode)
	if err != nil {
		if errors.Is(err, store.ErrDeviceAuthNotFound) {
			writeOAuthError(c, "expired_token", "the device_code has expired or is unknown")
			return
		}
		writeInternalError(c, s.logger, err, "failed to poll device authorization request")
		return
	}

	switch deviceReq.Status {
	case store.DeviceAuthStatusPending:
		writeOAuthError(c, "authorization_pending", "the authorization request is still pending")
	case store.DeviceAuthStatusDenied:
		writeOAuthError(c, "access_denied", "the authorization request was denied")
	default:
		plainKey, err := crypto.Decrypt(encryptedKey, s.encryptionKey, crypto.DeviceAuthAAD(deviceReq.UID.String()))
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to decrypt device authorization key")
			return
		}
		c.JSON(http.StatusOK, gin.H{"access_token": string(plainKey), "token_type": "Bearer"})
	}
}

// DeviceConsentInfo is the public detail of a device authorization request,
// safe to show on the (authenticated) consent page.
type DeviceConsentInfo struct {
	ClientName string    `json:"client_name"`
	UserCode   string    `json:"user_code"`
	Status     string    `json:"status"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// handleGetDeviceConsent returns the public details of a device authorization
// request for the consent page to render (client name + user code). Requires
// authentication so an unauthenticated visitor cannot probe user codes; the
// frontend route is itself behind the login wall.
// GET /api/v1/auth/device/consent?user_code=WDJP-4KXR
func (s *Server) handleGetDeviceConsent(c *gin.Context) {
	userCode := normalizeUserCode(c.Query("user_code"))
	if userCode == "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "user_code is required")
		return
	}

	deviceReq, err := s.store.GetDeviceAuthByUserCode(c.Request.Context(), userCode)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "device authorization request not found or expired")
		return
	}

	c.JSON(http.StatusOK, DeviceConsentInfo{
		ClientName: deviceReq.ClientName,
		UserCode:   formatUserCode(deviceReq.UserCode),
		Status:     deviceReq.Status,
		ExpiresAt:  deviceReq.ExpiresAt,
	})
}

// DeviceConsentRequest is the request body for POST /auth/device/consent.
type DeviceConsentRequest struct {
	UserCode string `json:"user_code" binding:"required"`
	Approve  bool   `json:"approve"`
}

// handleDeviceConsent approves or denies a pending device authorization
// request. Requires Web Session or Basic Auth, exactly like direct API key
// creation — approval mints a real dbb_ key owned by the approving user.
// POST /api/v1/auth/device/consent
func (s *Server) handleDeviceConsent(c *gin.Context) {
	var req DeviceConsentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "user_code is required")
		return
	}

	userCode := normalizeUserCode(req.UserCode)
	if userCode == "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "user_code is required")
		return
	}

	ctx := c.Request.Context()
	currentUser := getCurrentUser(c)

	deviceReq, err := s.store.GetDeviceAuthByUserCode(ctx, userCode)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "device authorization request not found or expired")
		return
	}
	if deviceReq.Status != store.DeviceAuthStatusPending {
		writeError(c, http.StatusConflict, ErrCodeConflict, "request already responded to")
		return
	}

	var encryptedKey []byte
	var keyPrefix string
	if req.Approve {
		apiKey, plainKey, err := s.store.CreateAPIKey(ctx, currentUser.UID, "device: "+deviceReq.ClientName, nil, s.encryptionKey)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to mint API key for device authorization")
			return
		}

		encryptedKey, err = crypto.Encrypt([]byte(plainKey), s.encryptionKey, crypto.DeviceAuthAAD(deviceReq.UID.String()))
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to encrypt device authorization key")
			return
		}
		keyPrefix = apiKey.KeyPrefix
	}

	if err := s.store.RespondToDeviceAuthByUserCode(ctx, userCode, currentUser.UID, req.Approve, encryptedKey, keyPrefix); err != nil {
		if errors.Is(err, store.ErrDeviceAuthAlreadyResolved) || errors.Is(err, store.ErrDeviceAuthNotFound) {
			writeError(c, http.StatusConflict, ErrCodeConflict, "request already responded to or expired")
			return
		}
		writeInternalError(c, s.logger, err, "failed to respond to device authorization request")
		return
	}

	eventType := "device_auth.denied"
	if req.Approve {
		eventType = "device_auth.approved"
	}
	details, _ := json.Marshal(map[string]interface{}{"client_name": deviceReq.ClientName, "request_id": deviceReq.UID, "key_prefix": keyPrefix})
	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   eventType,
		UserID:      &currentUser.UID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	c.Status(http.StatusNoContent)
}

// writeOAuthError sends an RFC 6749 §5.2 style error response
// ({error, error_description}) — the shape OAuth clients expect on the token
// endpoint, distinct from dbbat's usual {code, message} envelope. All token
// endpoint errors are HTTP 400 per RFC 6749 §5.2.
func writeOAuthError(c *gin.Context, code, description string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": code, "error_description": description})
}

// buildFrontendURL constructs an absolute URL to a frontend SPA route,
// combining the request's scheme/host with the configured base path
// (default /app), mirroring buildCallbackURL / redirectWithError.
func (s *Server) buildFrontendURL(r *http.Request, path string) string {
	scheme := "https"
	if r.TLS == nil {
		if fwdProto := r.Header.Get("X-Forwarded-Proto"); fwdProto != "" {
			scheme = fwdProto
		} else {
			scheme = "http"
		}
	}

	baseURL := "/app"
	if s.config != nil && s.config.BaseURL != "" {
		baseURL = s.config.BaseURL
	}

	return fmt.Sprintf("%s://%s%s%s", scheme, r.Host, baseURL, path)
}

// generateDeviceUserCode produces an 8-character canonical (dashless,
// uppercase) user code. The display form (WDJP-4KXR) is derived with
// formatUserCode.
func generateDeviceUserCode() (string, error) {
	raw := make([]byte, deviceUserCodeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}

	code := make([]byte, deviceUserCodeLength)
	for i, v := range raw {
		code[i] = deviceUserCodeAlphabet[int(v)%len(deviceUserCodeAlphabet)]
	}
	return string(code), nil
}

// formatUserCode inserts a dash at the midpoint of a canonical user code for
// display (WDJP4KXR -> WDJP-4KXR).
func formatUserCode(canonical string) string {
	if len(canonical) != deviceUserCodeLength {
		return canonical
	}
	half := deviceUserCodeLength / 2
	return canonical[:half] + "-" + canonical[half:]
}

// normalizeUserCode canonicalizes a user-entered code: uppercased, with every
// character not in the alphabet (dashes, spaces, etc.) stripped. So "wdjp-4kxr",
// "WDJP 4KXR", and "WDJP4KXR" all resolve to the same stored code.
func normalizeUserCode(input string) string {
	upper := strings.ToUpper(strings.TrimSpace(input))
	var b strings.Builder
	for _, r := range upper {
		if strings.ContainsRune(deviceUserCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
