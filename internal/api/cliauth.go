package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// CLI authorization constants.
const (
	cliAuthNameMaxLength       = 200
	cliAuthPollIntervalSeconds = 2
	cliAuthUserCodeLength      = 8
)

// cliAuthUserCodeAlphabet excludes visually ambiguous characters (0/O, 1/I/L)
// so the code can be read aloud or retyped without confusion.
const cliAuthUserCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// CreateCLIAuthRequestRequest is the request body for POST /auth/cli.
type CreateCLIAuthRequestRequest struct {
	Name string `json:"name" binding:"required"`
}

// CreateCLIAuthRequestResponse is returned to the CLI after opening a
// request. PollToken is a secret: it must never be logged or displayed, only
// held by the requesting CLI and sent back to /auth/cli/poll.
type CreateCLIAuthRequestResponse struct {
	RequestID       uuid.UUID `json:"request_id"`
	AuthorizeURL    string    `json:"authorize_url"`
	PollToken       string    `json:"poll_token"`
	UserCode        string    `json:"user_code"`
	ExpiresAt       time.Time `json:"expires_at"`
	IntervalSeconds int       `json:"interval_seconds"`
}

// handleCreateCLIAuthRequest starts a new CLI authorization (device-flow
// style) request. Unauthenticated: anyone can ask for a request to be
// opened, but the request grants nothing by itself — it only becomes an API
// key once a logged-in user approves it in the browser.
// POST /api/v1/auth/cli
func (s *Server) handleCreateCLIAuthRequest(c *gin.Context) {
	var req CreateCLIAuthRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: name is required")
		return
	}
	if len(req.Name) > cliAuthNameMaxLength {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "name too long")
		return
	}

	pollToken, err := generateRandomState()
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to generate CLI auth poll token")
		return
	}

	userCode, err := generateCLIAuthUserCode()
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to generate CLI auth user code")
		return
	}

	ctx := c.Request.Context()
	cliReq, err := s.store.CreateCLIAuthRequest(ctx, req.Name, pollToken, userCode)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to create CLI auth request")
		return
	}

	details, _ := json.Marshal(map[string]interface{}{"name": req.Name, "request_id": cliReq.UID})
	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType: "cli_auth.requested",
		Details:   details,
	})

	c.JSON(http.StatusCreated, CreateCLIAuthRequestResponse{
		RequestID:       cliReq.UID,
		AuthorizeURL:    s.buildFrontendURL(c.Request, "/cli-auth/"+cliReq.UID.String()),
		PollToken:       pollToken,
		UserCode:        userCode,
		ExpiresAt:       cliReq.ExpiresAt,
		IntervalSeconds: cliAuthPollIntervalSeconds,
	})
}

// CLIAuthRequestInfo is the public detail of a CLI authorization request,
// safe to show on the (authenticated) approval page.
type CLIAuthRequestInfo struct {
	Name      string    `json:"name"`
	UserCode  string    `json:"user_code"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleGetCLIAuthRequest returns the public details of a pending CLI
// authorization request, for the approval page to render (name + user code).
// Requires authentication so an unauthenticated visitor cannot probe request
// UIDs; the frontend route is itself behind the login wall.
// GET /api/v1/auth/cli/:uid
func (s *Server) handleGetCLIAuthRequest(c *gin.Context) {
	uid, err := uuid.Parse(c.Param("uid"))
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request id")
		return
	}

	cliReq, err := s.store.GetCLIAuthRequest(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "CLI authorization request not found or expired")
		return
	}

	c.JSON(http.StatusOK, CLIAuthRequestInfo{
		Name:      cliReq.Name,
		UserCode:  cliReq.UserCode,
		Status:    cliReq.Status,
		ExpiresAt: cliReq.ExpiresAt,
	})
}

// RespondToCLIAuthRequestRequest is the request body for
// POST /auth/cli/:uid/respond.
type RespondToCLIAuthRequestRequest struct {
	Approve bool `json:"approve"`
}

// handleRespondToCLIAuthRequest approves or denies a pending CLI
// authorization request. Requires Web Session or Basic Auth, exactly like
// direct API key creation — this endpoint mints a real dbb_ key on approval,
// owned by whichever authenticated user approves it.
// POST /api/v1/auth/cli/:uid/respond
func (s *Server) handleRespondToCLIAuthRequest(c *gin.Context) {
	uid, err := uuid.Parse(c.Param("uid"))
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request id")
		return
	}

	var req RespondToCLIAuthRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request")
		return
	}

	ctx := c.Request.Context()
	currentUser := getCurrentUser(c)

	cliReq, err := s.store.GetCLIAuthRequest(ctx, uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "CLI authorization request not found or expired")
		return
	}
	if cliReq.Status != store.CLIAuthStatusPending {
		writeError(c, http.StatusConflict, ErrCodeConflict, "request already responded to")
		return
	}

	var encryptedKey []byte
	var keyPrefix string
	if req.Approve {
		apiKey, plainKey, err := s.store.CreateAPIKey(ctx, currentUser.UID, "CLI: "+cliReq.Name, nil, s.encryptionKey)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to mint API key for CLI authorization")
			return
		}

		encryptedKey, err = crypto.Encrypt([]byte(plainKey), s.encryptionKey, crypto.CLIAuthAAD(uid.String()))
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to encrypt CLI authorization key")
			return
		}
		keyPrefix = apiKey.KeyPrefix
	}

	if err := s.store.RespondToCLIAuthRequest(ctx, uid, currentUser.UID, req.Approve, encryptedKey, keyPrefix); err != nil {
		if errors.Is(err, store.ErrCLIAuthAlreadyResolved) || errors.Is(err, store.ErrCLIAuthNotFound) {
			writeError(c, http.StatusConflict, ErrCodeConflict, "request already responded to or expired")
			return
		}
		writeInternalError(c, s.logger, err, "failed to respond to CLI authorization request")
		return
	}

	eventType := "cli_auth.denied"
	if req.Approve {
		eventType = "cli_auth.approved"
	}
	details, _ := json.Marshal(map[string]interface{}{"name": cliReq.Name, "request_id": uid, "key_prefix": keyPrefix})
	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   eventType,
		UserID:      &currentUser.UID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	c.Status(http.StatusNoContent)
}

// PollCLIAuthRequestRequest is the request body for POST /auth/cli/poll.
type PollCLIAuthRequestRequest struct {
	PollToken string `json:"poll_token" binding:"required"`
}

// handlePollCLIAuthRequest lets the CLI check on (and, once resolved,
// retrieve) the result of a CLI authorization request using its poll token.
// Deliberately not rate limited by IP like the other unauthenticated auth
// endpoints: a legitimate CLI polls this every few seconds for up to the
// request's TTL, and the poll token itself (32 random bytes) is the
// capability — brute-forcing it is infeasible.
// POST /api/v1/auth/cli/poll
func (s *Server) handlePollCLIAuthRequest(c *gin.Context) {
	var req PollCLIAuthRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "poll_token is required")
		return
	}

	ctx := c.Request.Context()
	cliReq, encryptedKey, err := s.store.PollCLIAuthRequest(ctx, req.PollToken)
	if err != nil {
		if errors.Is(err, store.ErrCLIAuthNotFound) {
			c.JSON(http.StatusOK, gin.H{"status": "expired"})
			return
		}
		writeInternalError(c, s.logger, err, "failed to poll CLI authorization request")
		return
	}

	switch cliReq.Status {
	case store.CLIAuthStatusPending:
		c.JSON(http.StatusOK, gin.H{"status": "pending"})
	case store.CLIAuthStatusDenied:
		c.JSON(http.StatusOK, gin.H{"status": "denied"})
	default:
		plainKey, err := crypto.Decrypt(encryptedKey, s.encryptionKey, crypto.CLIAuthAAD(cliReq.UID.String()))
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to decrypt CLI authorization key")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "approved", "key": string(plainKey)})
	}
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

// generateCLIAuthUserCode produces an 8-character, dash-split code (e.g.
// "WDJP-4KXR") shown to the user in both the terminal and the browser, so
// they can catch a mismatched or attacker-initiated authorization request
// before approving it.
func generateCLIAuthUserCode() (string, error) {
	raw := make([]byte, cliAuthUserCodeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}

	code := make([]byte, cliAuthUserCodeLength)
	for i, v := range raw {
		code[i] = cliAuthUserCodeAlphabet[int(v)%len(cliAuthUserCodeAlphabet)]
	}

	half := cliAuthUserCodeLength / 2
	return string(code[:half]) + "-" + string(code[half:]), nil
}
