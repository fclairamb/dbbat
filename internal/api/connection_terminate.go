package api

import (
	"errors"
	"log/slog"
	"net/http"

	"encoding/json"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

// terminateConnectionRequest is the body of POST /connections/{uid}/terminate.
//
// The reason is optional and goes nowhere near the client whose session is
// ending: it lands on the request row and in the connection.terminated audit
// entry, which is where somebody asking "why did my query die?" a week later
// will look.
type terminateConnectionRequest struct {
	Reason string `json:"reason"`
}

// maxTerminateReasonLength bounds the free text. Generous enough for a sentence
// and an incident link, short enough that the column is not a place to paste a
// log.
const maxTerminateReasonLength = 1000

// handleTerminateConnection ends one live proxied session.
//
// POST rather than DELETE /connections/{uid}: the session is not the row.
// DELETE would read as deleting the ledger entry, which retention owns and
// which the audit chain's connection.opened/closed pair exists to protect.
//
// 202, not 200: this replica does not necessarily serve the session. It writes
// the request row, signals its own registry in case it does, and the replica
// that actually owns the session (connections.run_id) acts on it within
// store.TerminationPollInterval. "Accepted" is the honest status for that —
// the session is live and the termination was requested.
func (s *Server) handleTerminateConnection(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")

		return
	}

	var req terminateConnectionRequest

	// An absent or empty body is a termination with no stated reason, not a
	// malformed request: ShouldBindJSON rejects "", so only a body that is
	// present and unparseable is refused.
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request body")

			return
		}
	}

	if len(req.Reason) > maxTerminateReasonLength {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			"reason is too long")

		return
	}

	currentUser := getCurrentUser(c)
	ctx := c.Request.Context()

	conn, err := s.store.RequestConnectionTermination(ctx, uid, currentUser.UID, req.Reason)

	switch {
	case errors.Is(err, store.ErrConnectionAlreadyClosed):
		writeError(c, http.StatusConflict, ErrCodeConflict, "connection is already closed")

		return
	case errors.Is(err, store.ErrConnectionNotFound):
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "connection not found")

		return
	case err != nil:
		writeInternalError(c, s.logger, err, "failed to request the connection termination")

		return
	}

	// The local fast path. When this replica is the one serving the session the
	// admin should not wait for a poll tick that is about to tell us what we
	// already know — and when it is not, this finds nothing and the owner picks
	// the request row up on its next tick. false also covers "here, but already
	// tearing down", which wants the same answer: nothing new to do.
	local := s.store.Sessions().Terminate(uid, cache.TerminationRequest{
		Reason: store.TerminationAdminTerminated,
		By:     currentUser.Username,
		Detail: req.Reason,
	})

	s.logger.InfoContext(ctx, "Session termination requested",
		slog.String("connection_uid", uid.String()),
		slog.String("requested_by", currentUser.Username),
		slog.Bool("local", local))

	s.auditTerminationRequest(c, conn, req.Reason, local)

	c.JSON(http.StatusAccepted, gin.H{
		"message": "session termination requested",
		// Whether this replica owned the session, which is the difference
		// between "already gone" and "gone within a couple of seconds".
		"local": local,
	})
}

// auditTerminationRequest records the *request*, separately from the
// connection.terminated entry the session itself writes when it actually ends.
//
// Both are needed and neither is redundant: this one names the admin and
// happens whether or not the session is still there to be ended (it can close
// on its own between the two), while that one is written by the process that
// really tore the session down and seals the query chain alongside it. An admin
// action that left no trace unless it succeeded would be the wrong way round.
func (s *Server) auditTerminationRequest(c *gin.Context, conn *store.Connection, reason string, local bool) {
	currentUser := getCurrentUser(c)

	details, err := json.Marshal(map[string]any{
		"connection_uid": conn.UID.String(),
		"user_id":        conn.UserID.String(),
		"database_id":    conn.DatabaseID.String(),
		"instance_id":    conn.InstanceID,
		"run_id":         derefString(conn.RunID),
		"reason":         reason,
		"local":          local,
	})
	if err != nil {
		s.logger.ErrorContext(c.Request.Context(), "failed to encode the termination request audit entry",
			slog.Any("error", err))

		return
	}

	userID := conn.UserID

	if err := s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   store.AuditEventConnectionTerminateRequested,
		UserID:      &userID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	}); err != nil {
		s.logger.ErrorContext(c.Request.Context(), "failed to record the termination request",
			slog.Any("error", err))
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
