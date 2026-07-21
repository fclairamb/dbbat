package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/conncheck"
	"github.com/fclairamb/dbbat/internal/store"
)

// connCheckTimeout bounds a connectivity check triggered from the API. It sits
// below the HTTP server's write timeout so a wedged bastion produces a proper
// JSON answer rather than a severed response.
const connCheckTimeout = 12 * time.Second

// ConnectionTestResponse is the API shape of a connectivity check. It mirrors
// conncheck.Result and carries no secret material — only the stage reached, a
// machine-readable code, a human-readable message, and the bastion's public
// host key.
type ConnectionTestResponse struct {
	OK            bool   `json:"ok"`
	Stage         string `json:"stage"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	HostKeyPinned bool   `json:"host_key_pinned,omitempty"`
	KnownHostKey  string `json:"ssh_known_host_key,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
}

// toConnectionTestResponse converts a check result to its API shape.
func toConnectionTestResponse(res conncheck.Result) ConnectionTestResponse {
	return ConnectionTestResponse{
		OK:            res.OK,
		Stage:         string(res.Stage),
		Code:          string(res.Code),
		Message:       res.Message,
		HostKeyPinned: res.HostKeyPinned,
		KnownHostKey:  res.KnownHostKey,
		DurationMs:    res.DurationMs,
	}
}

// handleTestServerConnection validates that a server row actually works: for an
// SSH bastion, that it is reachable and accepts the stored key (pinning the
// host key under admin supervision on first success); for a database target,
// that it can be dialed — through its bastion when via_uid is set — and that
// the stored credentials are accepted.
//
// A failed check is still HTTP 200: the staged result is the answer the admin
// asked for, and the UI keys its guidance off `stage`/`code`. Only "we could
// not run the check at all" (bad uid, unknown server) is a 4xx.
func (s *Server) handleTestServerConnection(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid server UID")

		return
	}

	srv, err := s.store.GetServerByUID(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "server not found")

		return
	}

	res := s.runConnectionCheck(c.Request.Context(), srv)

	s.auditConnectionCheck(c, srv, res)

	successResponse(c, toConnectionTestResponse(res))
}

// runConnectionCheck executes a bounded connectivity check against srv and logs
// the outcome. Only the server uid, stage and code are logged — never
// credentials or key material.
func (s *Server) runConnectionCheck(ctx context.Context, srv *store.Server) conncheck.Result {
	res := conncheck.New(s.store, s.encryptionKey).WithTimeout(connCheckTimeout).Check(ctx, srv)

	if s.logger != nil {
		s.logger.InfoContext(ctx, "server connectivity check",
			slog.String("server_uid", srv.UID.String()),
			slog.String("protocol", srv.Protocol),
			slog.Bool("ok", res.OK),
			slog.String("stage", string(res.Stage)),
			slog.String("code", string(res.Code)))
	}

	return res
}

// auditConnectionCheck records the check in the audit log. The details carry
// the stage and code only: the message can quote an upstream error, which does
// not belong in a durable audit record.
func (s *Server) auditConnectionCheck(c *gin.Context, srv *store.Server, res conncheck.Result) {
	currentUser := getCurrentUser(c)
	if currentUser == nil {
		return
	}

	details, _ := json.Marshal(map[string]any{
		"server_uid": srv.UID,
		"protocol":   srv.Protocol,
		"ok":         res.OK,
		"stage":      string(res.Stage),
		"code":       string(res.Code),
	})

	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "server.connection_tested",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})
}

// maybeInlineConnectionTest runs the same check inline on create/update when
// the caller opted in with `test_connection: true`, so the common provisioning
// path needs no second round trip. It is deliberately opt-in and always
// non-fatal: the row has already been written, and a target that is not
// reachable *yet* is a legitimate state.
func (s *Server) maybeInlineConnectionTest(c *gin.Context, requested bool, uid uuid.UUID) *ConnectionTestResponse {
	if !requested {
		return nil
	}

	srv, err := s.store.GetServerByUID(c.Request.Context(), uid)
	if err != nil {
		return nil
	}

	res := s.runConnectionCheck(c.Request.Context(), srv)
	s.auditConnectionCheck(c, srv, res)

	out := toConnectionTestResponse(res)

	return &out
}
