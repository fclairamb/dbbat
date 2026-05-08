package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// CreateGrantRequestRequest is the body for POST /grant-requests.
type CreateGrantRequestRequest struct {
	GrantDefinitionID uuid.UUID `json:"grant_definition_id" binding:"required"`
	DatabaseID        uuid.UUID `json:"database_id" binding:"required"`
	Justification     string    `json:"justification"`
}

// DenyGrantRequestRequest is the body for POST /grant-requests/:uid/deny.
type DenyGrantRequestRequest struct {
	Reason string `json:"reason"`
}

const maxJustificationLen = 1000

// handleCreateGrantRequest — any authenticated user can request access.
func (s *Server) handleCreateGrantRequest(c *gin.Context) {
	var req CreateGrantRequestRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	if len(req.Justification) > maxJustificationLen {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "justification too long")

		return
	}

	currentUser := getCurrentUser(c)
	ctx := c.Request.Context()

	def, err := s.store.GetGrantDefinition(ctx, req.GrantDefinitionID)
	if err != nil {
		if errors.Is(err, store.ErrGrantDefinitionNotFound) {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "grant_definition_id does not exist")

			return
		}

		writeInternalError(c, s.logger, err, "failed to load grant definition")

		return
	}

	if !def.IsActive {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "grant definition is not active")

		return
	}

	pending, err := s.store.HasPendingRequest(ctx, currentUser.UID, req.GrantDefinitionID, req.DatabaseID)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to check pending requests")

		return
	}

	if pending {
		writeError(c, http.StatusConflict, ErrCodeConflict, "a pending request already exists for this database and definition")

		return
	}

	created, err := s.store.CreateGrantRequest(ctx, &store.GrantRequest{
		UserID:            currentUser.UID,
		GrantDefinitionID: req.GrantDefinitionID,
		DatabaseID:        req.DatabaseID,
		Justification:     req.Justification,
	})
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to create grant request")

		return
	}

	details, _ := json.Marshal(map[string]any{
		"grant_request_uid":    created.UID,
		"grant_definition_id":  created.GrantDefinitionID,
		"database_id":          created.DatabaseID,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "grant_request.created",
		UserID:      &currentUser.UID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, created)
}

// handleListGrantRequests — role-aware. Admins see all (filterable);
// non-admins see only their own.
func (s *Server) handleListGrantRequests(c *gin.Context) {
	currentUser := getCurrentUser(c)
	filter := store.GrantRequestFilter{}

	if !currentUser.IsAdmin() {
		filter.UserID = &currentUser.UID
	} else if userID := c.Query("user_id"); userID != "" {
		if uid, err := uuid.Parse(userID); err == nil {
			filter.UserID = &uid
		}
	}

	if status := c.Query("status"); status != "" {
		s := store.GrantRequestStatus(status)
		filter.Status = &s
	}

	if databaseID := c.Query("database_id"); databaseID != "" {
		if uid, err := uuid.Parse(databaseID); err == nil {
			filter.DatabaseID = &uid
		}
	}

	requests, err := s.store.ListGrantRequests(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list grant requests")

		return
	}

	successResponse(c, gin.H{"grant_requests": requests})
}

// handleGetGrantRequest — role-aware: requesters can fetch their own,
// admins fetch anyone's.
func (s *Server) handleGetGrantRequest(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant request UID")

		return
	}

	req, err := s.store.GetGrantRequest(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrGrantRequestNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant request not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get grant request")

		return
	}

	currentUser := getCurrentUser(c)

	if !currentUser.IsAdmin() && req.UserID != currentUser.UID {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "no access to this grant request")

		return
	}

	successResponse(c, req)
}

// handleApproveGrantRequest — admin-only; flips pending → approved and
// materializes the grant in the same transaction.
func (s *Server) handleApproveGrantRequest(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant request UID")

		return
	}

	currentUser := getCurrentUser(c)

	grant, request, err := s.store.ApproveGrantRequest(c.Request.Context(), uid, currentUser.UID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrGrantRequestNotFound):
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant request not found")
		case errors.Is(err, store.ErrInvalidTransition):
			writeError(c, http.StatusConflict, ErrCodeConflict, "grant request is not pending")
		case errors.Is(err, store.ErrDefinitionInactive):
			writeError(c, http.StatusConflict, ErrCodeConflict, "grant definition is no longer active")
		default:
			writeInternalError(c, s.logger, err, "failed to approve grant request")
		}

		return
	}

	details, _ := json.Marshal(map[string]any{
		"grant_request_uid":  request.UID,
		"resulting_grant_id": grant.UID,
	})

	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "grant_request.approved",
		UserID:      &request.UserID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"grant_request": request, "grant": grant})
}

// handleDenyGrantRequest — admin-only.
func (s *Server) handleDenyGrantRequest(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant request UID")

		return
	}

	var body DenyGrantRequestRequest
	_ = c.ShouldBindJSON(&body) // Reason is optional; ignore parse errors on empty body

	currentUser := getCurrentUser(c)

	updated, err := s.store.DenyGrantRequest(c.Request.Context(), uid, currentUser.UID, body.Reason)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrGrantRequestNotFound):
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant request not found")
		case errors.Is(err, store.ErrInvalidTransition):
			writeError(c, http.StatusConflict, ErrCodeConflict, "grant request is not pending")
		default:
			writeInternalError(c, s.logger, err, "failed to deny grant request")
		}

		return
	}

	details, _ := json.Marshal(map[string]any{
		"grant_request_uid": updated.UID,
		"reason":            body.Reason,
	})

	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "grant_request.denied",
		UserID:      &updated.UserID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, updated)
}

// handleCancelGrantRequest — requester (or admin) only, while pending.
func (s *Server) handleCancelGrantRequest(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant request UID")

		return
	}

	ctx := c.Request.Context()
	currentUser := getCurrentUser(c)

	existing, err := s.store.GetGrantRequest(ctx, uid)
	if err != nil {
		if errors.Is(err, store.ErrGrantRequestNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant request not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get grant request")

		return
	}

	if !currentUser.IsAdmin() && existing.UserID != currentUser.UID {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "only the requester or an admin can cancel")

		return
	}

	updated, err := s.store.CancelGrantRequest(ctx, uid, currentUser.UID)
	if err != nil {
		if errors.Is(err, store.ErrInvalidTransition) {
			writeError(c, http.StatusConflict, ErrCodeConflict, "grant request is not pending")

			return
		}

		writeInternalError(c, s.logger, err, "failed to cancel grant request")

		return
	}

	details, _ := json.Marshal(map[string]any{
		"grant_request_uid": updated.UID,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "grant_request.cancelled",
		UserID:      &updated.UserID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, updated)
}
