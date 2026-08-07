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

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// denyRequest is the body of POST /queries/{uid}/deny.
type denyRequest struct {
	Reason string `json:"reason"`
}

// handleListPendingApprovals returns every query currently parked awaiting a
// decision. This is the authoritative "what is pending now" view: the stream
// is best-effort for history and clients refetch this on every reconnect.
func (s *Server) handleListPendingApprovals(c *gin.Context) {
	pending, err := s.store.ListPendingApprovalQueries(c.Request.Context())
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list pending approvals")

		return
	}

	currentUser := getCurrentUser(c)

	visible := make([]store.Query, 0, len(pending))

	for i := range pending {
		if s.mayViewQuery(c.Request.Context(), currentUser, &pending[i]) {
			visible = append(visible, pending[i])
		}
	}

	successResponse(c, gin.H{"queries": visible})
}

// handleApproveQuery releases a parked statement.
func (s *Server) handleApproveQuery(c *gin.Context) {
	s.resolveQuery(c, store.ApprovalApproved, "")
}

// handleDenyQuery rejects a parked statement, carrying the approver's reason
// back to the client as a protocol-native error.
func (s *Server) handleDenyQuery(c *gin.Context) {
	var body denyRequest
	// A missing/empty body is fine — the reason is optional.
	_ = c.ShouldBindJSON(&body)

	s.resolveQuery(c, store.ApprovalDenied, body.Reason)
}

// resolveQuery is the shared approve/deny path.
//
// Approval is *by query uid*: the approver decides on exactly the statement
// they were shown, and the waiting session independently asserts the resolved
// uid is its own before acting. Neither check is sufficient alone.
func (s *Server) resolveQuery(c *gin.Context, status, reason string) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid query UID")

		return
	}

	ctx := c.Request.Context()
	currentUser := getCurrentUser(c)

	query, err := s.store.GetQueryWithOwner(ctx, uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "query not found")

		return
	}

	if query.ApprovalStatus == nil || *query.ApprovalStatus != store.ApprovalPending {
		writeError(c, http.StatusConflict, ErrCodeConflict, "query is not awaiting approval")

		return
	}

	// Four-eyes means four eyes. Self-approval is rejected unconditionally —
	// including for admins, who would otherwise be able to wave their own
	// statements through and make the whole control decorative.
	if query.UserID != nil && *query.UserID == currentUser.UID {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "you cannot resolve your own query")

		return
	}

	if !s.mayApproveQuery(ctx, currentUser, query) {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "you are not an approver for this query")

		return
	}

	if err := s.applyQueryDecision(ctx, query, currentUser, status, reason); err != nil {
		if errors.Is(err, store.ErrQueryNotPending) {
			// Somebody (or the client's own disconnect) got there first.
			writeError(c, http.StatusConflict, ErrCodeConflict, "query is no longer awaiting approval")

			return
		}

		writeInternalError(c, s.logger, err, "failed to resolve approval")

		return
	}

	successResponse(c, gin.H{
		"query_uid":         uid,
		"approval_status":   status,
		"resolution_reason": reason,
		"resolved_by":       resolverPayload(currentUser),
	})
}

// handleDenyAllPending is the bulk safety valve. With no approval timeout, an
// operator needs one action that clears every parked statement — for a bad
// deploy, an incident, or simply a shift ending with holds nobody will answer.
func (s *Server) handleDenyAllPending(c *gin.Context) {
	var body denyRequest
	_ = c.ShouldBindJSON(&body)

	reason := body.Reason
	if reason == "" {
		reason = "bulk denial by an administrator"
	}

	ctx := c.Request.Context()
	currentUser := getCurrentUser(c)

	pending, err := s.store.ListPendingApprovalQueries(ctx)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list pending approvals")

		return
	}

	denied := 0
	skipped := 0

	for i := range pending {
		q := &pending[i]

		// Self-approval is rejected even in bulk; denying one's own statement
		// is harmless but the rule stays uniform and auditable.
		if q.UserID != nil && *q.UserID == currentUser.UID {
			skipped++

			continue
		}

		if !s.mayApproveQuery(ctx, currentUser, q) {
			skipped++

			continue
		}

		if err := s.applyQueryDecision(ctx, q, currentUser, store.ApprovalDenied, reason); err != nil {
			skipped++

			continue
		}

		denied++
	}

	successResponse(c, gin.H{"denied": denied, "skipped": skipped})
}

// applyQueryDecision persists the decision and announces it. Shared by the
// REST handlers and the Slack Approve/Deny buttons so the two paths cannot
// drift on audit, fan-out or message updating.
func (s *Server) applyQueryDecision(ctx context.Context, query *store.Query, by *store.User, status, reason string) error {
	if err := s.store.ResolveQueryApproval(ctx, query.UID, status, &by.UID, reason); err != nil {
		return err
	}

	s.broadcastResolution(ctx, query, status, by, reason)

	return nil
}

// ResolveQueryApprovalAs authorizes and applies a decision for a user that did
// not come through the HTTP handlers (today: a Slack button click). It repeats
// every check the REST path makes — pending state, self-approval, approver
// membership — because "it came from Slack" is not an authorization.
func (s *Server) ResolveQueryApprovalAs(ctx context.Context, user *store.User, queryUID uuid.UUID, status, reason string) error {
	query, err := s.store.GetQueryWithOwner(ctx, queryUID)
	if err != nil {
		return err
	}

	if query.ApprovalStatus == nil || *query.ApprovalStatus != store.ApprovalPending {
		return store.ErrQueryNotPending
	}

	if query.UserID != nil && *query.UserID == user.UID {
		return ErrSelfApproval
	}

	if !s.mayApproveQuery(ctx, user, query) {
		return ErrNotAnApprover
	}

	return s.applyQueryDecision(ctx, query, user, status, reason)
}

// Approval authorization errors surfaced outside the HTTP handlers.
var (
	// ErrSelfApproval is returned when the requester tries to resolve their
	// own held statement. Four-eyes means four eyes, admin or not.
	ErrSelfApproval = errors.New("you cannot resolve your own query")
	// ErrNotAnApprover is returned when the user is neither an admin nor a
	// member of the grant's approver groups.
	ErrNotAnApprover = errors.New("you are not an approver for this query")
)

// broadcastResolution writes the audit trail, wakes the parked session
// (locally and on every other replica) and publishes the resolution event.
func (s *Server) broadcastResolution(ctx context.Context, query *store.Query, status string, by *store.User, reason string) {
	now := time.Now()

	details, _ := json.Marshal(map[string]any{
		"query_uid":         query.UID.String(),
		"connection_uid":    query.ConnectionID.String(),
		"approval_status":   status,
		"resolution_reason": reason,
		"approval_pattern":  derefStr(query.ApprovalPattern),
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "query.approval." + status,
		UserID:      query.UserID,
		PerformedBy: &by.UID,
		Details:     details,
	})

	// Wake a session parked on this replica.
	s.approvals.Resolve(approval.Decision{
		QueryUID: query.UID,
		Status:   status,
		By:       &by.UID,
		ByName:   by.Username,
		Reason:   reason,
		At:       now,
	})

	data := map[string]any{
		"query_uid":         query.UID.String(),
		"connection_uid":    query.ConnectionID.String(),
		"sql_text":          query.SQLText,
		"executed_at":       query.ExecutedAt,
		"approval_required": true,
		"approval_status":   status,
		"resolution_reason": reason,
		"resolved_at":       now,
		"resolved_by":       resolverPayload(by),
	}

	s.broker.Publish(events.ConnectionQueriesTopic(query.ConnectionID.String()), events.EventApprovalResolved, data)
	s.broker.Publish(events.TopicApprovalsPending, events.EventApprovalResolved, data)

	// Wake a session parked on a *different* replica.
	if err := s.store.NotifyEvent(ctx, store.NotifyChannelApprovals, store.EventNotification{
		Topic:    events.TopicApprovalsPending,
		Type:     events.EventApprovalResolved,
		QueryUID: query.UID,
		ConnUID:  query.ConnectionID,
	}); err != nil {
		s.logger.WarnContext(ctx, "failed to fan approval decision to other replicas", slog.Any("error", err))
	}

	if s.approvalNotifier != nil {
		s.approvalNotifier.Resolved(ctx, query.UID, status, by.Username, reason)
	}
}

// resolverPayload renders the approver for the API response and the stream, so
// every watcher sees *who* unblocked a query.
func resolverPayload(u *store.User) map[string]any {
	if u == nil {
		return nil
	}

	return map[string]any{
		"uid":          u.UID.String(),
		"username":     u.Username,
		"display_name": u.Username,
	}
}

// mayApproveQuery reports whether the user may resolve holds on this query:
// any admin, plus members of the grant's approver groups.
//
// The grant is the one resolveApprovalGrant names — the connection's stamped
// grant_uid when there is one, otherwise the (legacy) active grant for the
// connection's user/database pair. Either way, if it is gone (deleted, or
// — on the legacy path — revoked or expired since), only admins can still
// resolve: that is the fail-closed direction.
func (s *Server) mayApproveQuery(ctx context.Context, user *store.User, query *store.Query) bool {
	if user == nil {
		return false
	}

	if user.IsAdmin() {
		return true
	}

	grant, err := s.resolveApprovalGrant(ctx, query)
	if err != nil || grant == nil || len(grant.ApproverGroupUIDs()) == 0 {
		return false
	}

	groups, err := s.store.ListUserGroupUIDs(ctx, user.UID)
	if err != nil {
		return false
	}

	return grant.MayApprove(groups)
}

// resolveApprovalGrant finds the grant whose approver_group_uids govern this
// query's hold.
//
// Preferred: the grant the query's connection was stamped with at auth time
// (connections.grant_uid). That is the grant whose approval_patterns actually
// triggered the hold, so it is also the grant whose approver groups get to
// resolve it. Getting this wrong is a real bug this replaces: without the
// stamp, the check re-resolves "the active grant" at approval time via
// GetActiveGrant, which — if a newer, higher-priority grant was created for
// the same user/database while the query sat on hold — returns that *newer*
// grant, whose approver_group_uids have nothing to do with why the hold
// exists.
//
// A stamped grant that fails to resolve (deleted) is reported as an error,
// not silently swapped for the legacy lookup: that would reintroduce the same
// wrong-grant risk the stamp exists to close. The legacy
// GetActiveGrant(user, database) path is used only when there is no stamp to
// trust in the first place — a NULL grant_uid (a connection predating this
// column) or a connection lookup failure.
func (s *Server) resolveApprovalGrant(ctx context.Context, query *store.Query) (*store.Grant, error) {
	if conn, err := s.store.GetConnectionByUID(ctx, query.ConnectionID); err == nil && conn.GrantUID != nil {
		return s.store.GetGrantByUID(ctx, *conn.GrantUID)
	}

	if query.UserID == nil || query.DatabaseID == nil {
		return nil, store.ErrNoActiveGrant
	}

	return s.store.GetActiveGrant(ctx, *query.UserID, *query.DatabaseID)
}

// mayViewQuery reports whether the user may *see* a pending query. Reading is
// wider than resolving — viewers already read every query through
// GET /api/v1/queries, so denying them the pending list would be theater — but
// it is never wider than that: an ordinary connector sees only holds they are
// an approver for.
func (s *Server) mayViewQuery(ctx context.Context, user *store.User, query *store.Query) bool {
	if user == nil {
		return false
	}

	if user.IsAdmin() || user.IsViewer() {
		return true
	}

	return s.mayApproveQuery(ctx, user, query)
}

// approvalEscalator is the slice of the Slack escalator the API needs, kept as
// a local interface so the API package doesn't have to import the proxy.
type approvalEscalator interface {
	Resolved(ctx context.Context, queryUID uuid.UUID, status, byName, reason string)
}
