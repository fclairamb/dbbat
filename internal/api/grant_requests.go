package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/notify"
	"github.com/fclairamb/dbbat/internal/store"
)

// slackNotifyTimeout caps how long we'll spend posting to Slack before
// the goroutine gets canceled. Slack typically responds in <1s but we
// don't want a slow Slack to leak goroutines forever.
const slackNotifyTimeout = 5 * time.Second

// notifyAsync fires a Slack notification in the background. The notifier
// is a graceful no-op when nil (feature disabled), so callers don't need
// to gate on configuration. We pass a fresh context so request
// cancellation doesn't kill the notify in-flight.
func (s *Server) notifyAsync(ev notify.GrantRequestEvent) {
	if s.notifier == nil {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), slackNotifyTimeout)
		defer cancel()

		s.notifier.NotifyGrantRequest(ctx, ev)
	}()
}

// loadEventContext gathers the related rows the notifier needs to render a
// message. Errors are logged and the partial event is returned — the
// notifier can render with nil pointers, just less informatively.
func (s *Server) loadEventContext(ctx context.Context, req *store.GrantRequest, decider *store.User) notify.GrantRequestEvent {
	ev := notify.GrantRequestEvent{Request: req, Decider: decider}

	if def, err := s.store.GetGrantDefinition(ctx, req.GrantDefinitionID); err == nil {
		ev.Definition = def
	}

	if db, err := s.store.GetDatabaseByUID(ctx, req.DatabaseID); err == nil {
		ev.Database = db
	}

	if u, err := s.store.GetUserByUID(ctx, req.UserID); err == nil {
		ev.Requester = u
	}

	// Interactive rendering (buttons + @-mentions) only applies when the
	// notifier has a signing secret. Gate all the extra lookups on it so
	// non-interactive deployments keep exactly today's behavior.
	if s.notifier.Interactive() {
		ev.Interactive = true
		ev.RequesterSlackID = s.slackIDForUser(ctx, req.UserID)
		ev.DeciderSlackID = s.deciderSlackID(ctx, decider)

		if admins, err := s.store.ListAdminSlackUserIDs(ctx); err == nil {
			ev.AdminSlackIDs = admins
		} else {
			s.logger.WarnContext(ctx, "list admin slack ids failed", slog.Any("error", err))
		}
	}

	return ev
}

// slackIDForUser returns the given user's linked Slack provider_id, or ""
// if they have no Slack identity. Best-effort: lookup errors yield "".
func (s *Server) slackIDForUser(ctx context.Context, userID uuid.UUID) string {
	identities, err := s.store.GetUserIdentities(ctx, userID)
	if err != nil {
		return ""
	}

	for i := range identities {
		if identities[i].Provider == store.IdentityTypeSlack {
			return identities[i].ProviderID
		}
	}

	return ""
}

// deciderSlackID resolves the decider's Slack ID for thread-reply mentions.
func (s *Server) deciderSlackID(ctx context.Context, decider *store.User) string {
	if decider == nil {
		return ""
	}

	return s.slackIDForUser(ctx, decider.UID)
}

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
		"grant_request_uid":   created.UID,
		"grant_definition_id": created.GrantDefinitionID,
		"database_id":         created.DatabaseID,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "grant_request.created",
		UserID:      &currentUser.UID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	ev := s.loadEventContext(ctx, created, nil)
	ev.Action = notify.GrantActionCreated
	s.notifyAsync(ev)

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

// decisionSource records where a grant decision originated, recorded in the
// audit event `details.via` field so Slack- and UI-driven decisions can be
// told apart. The web UI omits it (via is only set when non-default).
type decisionSource string

const (
	decisionSourceWeb   decisionSource = "web"
	decisionSourceSlack decisionSource = "slack"
)

// decideOutcome is the result of a shared approve/deny decision. It carries
// enough for both the HTTP handlers (response body) and the Slack handler
// (thread reply text + message coordinates) without re-querying.
type decideOutcome struct {
	Request *store.GrantRequest
	Grant   *store.Grant             // nil for deny
	Event   notify.GrantRequestEvent // the event already fired to the notifier
	Action  notify.GrantAction       // approved | denied
}

// approveGrantRequest runs the approve decision: store transition, audit
// event (with source), and async notification. It returns the raw store
// error unmapped so each caller can translate it to its own transport
// (HTTP status vs Slack ephemeral). Mirrors the deny path below.
func (s *Server) approveGrantRequest(ctx context.Context, uid uuid.UUID, decider *store.User, source decisionSource) (*decideOutcome, error) {
	grant, request, err := s.store.ApproveGrantRequest(ctx, uid, decider.UID)
	if err != nil {
		return nil, err
	}

	details := decisionDetails(map[string]any{
		"grant_request_uid":  request.UID,
		"resulting_grant_id": grant.UID,
	}, source)

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "grant_request.approved",
		UserID:      &request.UserID,
		PerformedBy: &decider.UID,
		Details:     details,
	})

	ev := s.loadEventContext(ctx, request, decider)
	ev.Action = notify.GrantActionApproved
	s.notifyAsync(ev)

	return &decideOutcome{Request: request, Grant: grant, Event: ev, Action: notify.GrantActionApproved}, nil
}

// denyGrantRequest runs the deny decision: store transition, audit event
// (with source), and async notification. Returns the raw store error
// unmapped, like approveGrantRequest.
func (s *Server) denyGrantRequest(ctx context.Context, uid uuid.UUID, decider *store.User, reason string, source decisionSource) (*decideOutcome, error) {
	updated, err := s.store.DenyGrantRequest(ctx, uid, decider.UID, reason)
	if err != nil {
		return nil, err
	}

	details := decisionDetails(map[string]any{
		"grant_request_uid": updated.UID,
		"reason":            reason,
	}, source)

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "grant_request.denied",
		UserID:      &updated.UserID,
		PerformedBy: &decider.UID,
		Details:     details,
	})

	ev := s.loadEventContext(ctx, updated, decider)
	ev.Action = notify.GrantActionDenied
	s.notifyAsync(ev)

	return &decideOutcome{Request: updated, Event: ev, Action: notify.GrantActionDenied}, nil
}

// decisionDetails marshals audit details, adding `via` only for non-web
// sources so existing UI-driven audit rows are unchanged.
func decisionDetails(base map[string]any, source decisionSource) json.RawMessage {
	if source != decisionSourceWeb {
		base["via"] = string(source)
	}

	details, _ := json.Marshal(base)

	return details
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

	outcome, err := s.approveGrantRequest(c.Request.Context(), uid, currentUser, decisionSourceWeb)
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

	successResponse(c, gin.H{"grant_request": outcome.Request, "grant": outcome.Grant})
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

	outcome, err := s.denyGrantRequest(c.Request.Context(), uid, currentUser, body.Reason, decisionSourceWeb)
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

	successResponse(c, outcome.Request)
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

	ev := s.loadEventContext(ctx, updated, currentUser)
	ev.Action = notify.GrantActionCancelled
	s.notifyAsync(ev)

	successResponse(c, updated)
}
