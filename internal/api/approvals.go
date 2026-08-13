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

// Approver hats — *why* the current user may resolve something. Returned
// alongside each pending item so the UI can say which authority the viewer is
// acting under instead of just enabling a button, which is the difference
// between a delegated approver trusting the screen and guessing.
const (
	// ApproverHatNone means the viewer may see the item but not resolve it.
	ApproverHatNone = ""
	// ApproverHatAdmin — the admin role, which decides everything.
	ApproverHatAdmin = "admin"
	// ApproverHatDefinition — a member of the grant definition's
	// approver_user_group_uids. Query holds only; it wins over the server chain.
	ApproverHatDefinition = "definition_approver"
	// ApproverHatServer — a member of a group resolved off the target server or
	// its server groups (query approvers for a hold, access approvers for a
	// grant request).
	ApproverHatServer = "server_approver"
)

// pendingQueryResponse is a held query plus the hat the *current* viewer would
// wear to resolve it.
type pendingQueryResponse struct {
	*store.Query

	ApproverRole string `json:"approver_role"`
}

// listingApprovers is everything the approver chain needs to judge a whole
// page, fetched once instead of once per row: the viewer's user groups (one
// value for the entire page — they cannot change mid-page) and the approver
// groups resolved for every distinct database the page names.
//
// It holds no rule of its own. Every decision is still
// store.ServerApprovers.MayApprove over what store.ResolveServerApproverGroups
// would have returned for that server, so a batched listing and a single-row
// lookup cannot disagree — which matters, because this is an authorization
// path and a second implementation of the chain would be the bug.
//
// A nil *listingApprovers means "nothing prefetched"; the row-at-a-time callers
// pass it and the decision functions resolve just their own server. A non-nil
// one whose lookup failed is empty, which is the same refusal the per-row error
// path produced.
type listingApprovers struct {
	userGroups []uuid.UUID
	byServer   store.ServerApprovers
}

// mayApproveFor is the single decision expression both the batched and the
// row-at-a-time paths end at.
func (p *listingApprovers) mayApproveFor(serverUID uuid.UUID) bool {
	if p == nil {
		return false
	}

	return p.byServer.MayApprove(serverUID, p.userGroups)
}

// prefetchApprovers resolves the approver picture for a set of databases in a
// fixed number of queries — one for the viewer's user groups, two for the
// servers and their groups — however many rows the listing holds.
//
// Admins short-circuit every chain before it is consulted, so they get an empty
// prefetch and cost nothing extra. Any lookup failure also yields an empty one:
// refusing is the fail-closed direction, and it is exactly what the per-row
// path did with the same error.
func (s *Server) prefetchApprovers(
	ctx context.Context, user *store.User, kind store.ApproverKind, serverUIDs []uuid.UUID,
) *listingApprovers {
	if user == nil || user.IsAdmin() {
		return &listingApprovers{}
	}

	groups, err := s.store.ListUserGroupUIDs(ctx, user.UID)
	if err != nil || len(groups) == 0 {
		return &listingApprovers{}
	}

	byServer, err := s.store.ResolveServerApproverGroupsByServers(ctx, serverUIDs, kind)
	if err != nil {
		return &listingApprovers{userGroups: groups}
	}

	return &listingApprovers{userGroups: groups, byServer: byServer}
}

// distinctUIDs collapses a list of (possibly optional, possibly repeated)
// database uids into the set a prefetch should ask about. The same handful of
// servers recurs across a pending page, which is the whole reason batching
// pays.
func distinctUIDs(uids []*uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(uids))
	out := make([]uuid.UUID, 0, len(uids))

	for _, uid := range uids {
		if uid == nil {
			continue
		}

		if _, dup := seen[*uid]; dup {
			continue
		}

		seen[*uid] = struct{}{}
		out = append(out, *uid)
	}

	return out
}

// handleListPendingApprovals returns every query currently parked awaiting a
// decision. This is the authoritative "what is pending now" view: the stream
// is best-effort for history and clients refetch this on every reconnect.
func (s *Server) handleListPendingApprovals(c *gin.Context) {
	ctx := c.Request.Context()

	pending, err := s.store.ListPendingApprovalQueries(ctx)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list pending approvals")

		return
	}

	currentUser := getCurrentUser(c)

	// One resolution for the whole page, reused by both the visibility gate and
	// the hat — which used to walk the same chain twice per row.
	databases := make([]*uuid.UUID, 0, len(pending))
	for i := range pending {
		databases = append(databases, pending[i].DatabaseID)
	}

	approvers := s.prefetchApprovers(ctx, currentUser, store.ApproverKindQuery, distinctUIDs(databases))

	visible := make([]pendingQueryResponse, 0, len(pending))

	for i := range pending {
		if !s.mayViewQueryWith(ctx, currentUser, &pending[i], approvers) {
			continue
		}

		visible = append(visible, pendingQueryResponse{
			Query:        &pending[i],
			ApproverRole: s.approverHatForQueryWith(ctx, currentUser, &pending[i], approvers),
		})
	}

	successResponse(c, gin.H{"queries": visible})
}

// approverHatForQuery names the authority under which the user may resolve this
// hold, following the same order mayApproveQuery does — so the label can never
// claim a right the gate would refuse.
//
// Self-approval yields no hat at all: the viewer can watch their own held
// statement, but nothing lets them release it.
func (s *Server) approverHatForQuery(ctx context.Context, user *store.User, query *store.Query) string {
	return s.approverHatForQueryWith(ctx, user, query, nil)
}

// approverHatForQueryWith is approverHatForQuery against an optional
// per-listing prefetch. nil means "resolve just this query's database", which
// is what every single-row caller wants; the pending listing passes the map it
// already built for the visibility gate.
func (s *Server) approverHatForQueryWith(
	ctx context.Context, user *store.User, query *store.Query, approvers *listingApprovers,
) string {
	if user == nil {
		return ApproverHatNone
	}

	if query.UserID != nil && *query.UserID == user.UID {
		return ApproverHatNone
	}

	if user.IsAdmin() {
		return ApproverHatAdmin
	}

	if approvers == nil {
		approvers = s.prefetchApprovers(
			ctx, user, store.ApproverKindQuery, distinctUIDs([]*uuid.UUID{query.DatabaseID}),
		)
	}

	if len(approvers.userGroups) == 0 {
		return ApproverHatNone
	}

	grant, err := s.resolveApprovalGrant(ctx, query)
	if err == nil && grant != nil && len(grant.ApproverUserGroupUIDs()) > 0 {
		if grant.MayApprove(approvers.userGroups) {
			return ApproverHatDefinition
		}

		return ApproverHatNone
	}

	if query.DatabaseID == nil {
		return ApproverHatNone
	}

	if approvers.mayApproveFor(*query.DatabaseID) {
		return ApproverHatServer
	}

	return ApproverHatNone
}

// approverHatForRequest is approverHatForQuery's grant-request counterpart.
// There is no definition-level hat here: grant-request approval never had one.
func (s *Server) approverHatForRequest(ctx context.Context, user *store.User, req *store.GrantRequest) string {
	return s.approverHatForRequestWith(ctx, user, req, nil)
}

// approverHatForRequestWith is approverHatForRequest against an optional
// per-listing prefetch, on the same nil-means-resolve-one-server rule.
func (s *Server) approverHatForRequestWith(
	ctx context.Context, user *store.User, req *store.GrantRequest, approvers *listingApprovers,
) string {
	if user == nil || req == nil {
		return ApproverHatNone
	}

	if user.IsAdmin() {
		return ApproverHatAdmin
	}

	if s.mayDecideGrantRequestWith(ctx, user, req, approvers) {
		return ApproverHatServer
	}

	return ApproverHatNone
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

// mayApproveQuery reports whether the user may resolve holds on this query.
//
// The chain, most specific first:
//
//  1. any admin;
//  2. a member of the **grant definition's** approver groups, when that list is
//     non-empty — an explicit policy choice, so it wins outright;
//  3. otherwise a member of the groups ResolveServerApproverGroups yields for
//     the target database and ApproverKindQuery: the server's own query
//     approvers, else the union of its server groups';
//  4. otherwise nobody — admin-only, which is exactly the behavior of an
//     instance that configures none of this.
//
// The grant in step 2 is the one resolveApprovalGrant names — the connection's
// stamped grant_uid when there is one, otherwise the (legacy) active grant for
// the connection's user/database pair. A grant that no longer resolves (deleted,
// or revoked/expired on the legacy path) carries no readable approver list, so
// it falls through to step 3 rather than to a hard deny: the server-level lists
// are a property of the database, not of the grant, and an instance that
// configures none of them still ends up exactly where it used to — admins only.
//
// Self-approval is *not* checked here. It is refused by every caller before
// this function runs, and no membership can override it.
func (s *Server) mayApproveQuery(ctx context.Context, user *store.User, query *store.Query) bool {
	return s.mayApproveQueryWith(ctx, user, query, nil)
}

// mayApproveQueryWith is mayApproveQuery against an optional per-listing
// prefetch. nil resolves just this query's database, which is the behavior
// every single-row caller had; a non-nil one is the same answer, read out of a
// map the caller resolved for the whole page.
func (s *Server) mayApproveQueryWith(
	ctx context.Context, user *store.User, query *store.Query, approvers *listingApprovers,
) bool {
	if user == nil {
		return false
	}

	if user.IsAdmin() {
		return true
	}

	if approvers == nil {
		approvers = s.prefetchApprovers(
			ctx, user, store.ApproverKindQuery, distinctUIDs([]*uuid.UUID{query.DatabaseID}),
		)
	}

	if len(approvers.userGroups) == 0 {
		return false
	}

	grant, err := s.resolveApprovalGrant(ctx, query)
	if err == nil && grant != nil && len(grant.ApproverUserGroupUIDs()) > 0 {
		return grant.MayApprove(approvers.userGroups)
	}

	if query.DatabaseID == nil {
		return false
	}

	return approvers.mayApproveFor(*query.DatabaseID)
}

// derefUUIDs reads an optional uid list, yielding nil when the field was
// omitted. Used by the PATCH-shaped request bodies, where nil and [] mean
// different things.
func derefUUIDs(in *[]uuid.UUID) []uuid.UUID {
	if in == nil {
		return nil
	}

	return *in
}

// validateApproverUserGroups checks that every uid in the given approver lists
// names a real user group, returning the message for a 400 otherwise. A list
// pointing at a group that does not exist would fail closed and read as a bug,
// exactly like a mis-scoped grant definition.
func (s *Server) validateApproverUserGroups(ctx context.Context, lists ...[]uuid.UUID) string {
	for _, list := range lists {
		for _, groupUID := range list {
			if _, err := s.store.GetUserGroup(ctx, groupUID); err != nil {
				return "approver user group does not exist: " + groupUID.String()
			}
		}
	}

	return ""
}

// mayDecideGrantRequest reports whether the user may approve or deny a grant
// request: any admin, plus whoever ResolveServerApproverGroups names as an
// **access** approver for the target database — the server's own list, else the
// union of its server groups'. Configuring neither leaves the decision
// admin-only, which is exactly today's behavior.
//
// Self-approval is refused for everyone but an admin, and deliberately so on
// both counts. A non-admin approver deciding their own request would defeat the
// point of delegating: an ops group that can approve its members' requests to
// its own servers must not be able to approve its *own*. An admin already has
// POST /grants — refusing them here would be theater, not a control — so that
// long-standing behavior is left alone.
func (s *Server) mayDecideGrantRequest(ctx context.Context, user *store.User, req *store.GrantRequest) bool {
	return s.mayDecideGrantRequestWith(ctx, user, req, nil)
}

// mayDecideGrantRequestWith is mayDecideGrantRequest against an optional
// per-listing prefetch. nil resolves just this request's database — what every
// single-row caller (including both Slack transports) does; the non-admin
// listing passes a map covering its whole page. The rule below is the same
// either way, self-approval refusal included.
func (s *Server) mayDecideGrantRequestWith(
	ctx context.Context, user *store.User, req *store.GrantRequest, approvers *listingApprovers,
) bool {
	if user == nil || req == nil {
		return false
	}

	if user.IsAdmin() {
		return true
	}

	if req.UserID == user.UID {
		return false
	}

	if approvers == nil {
		approvers = s.prefetchApprovers(
			ctx, user, store.ApproverKindAccess, []uuid.UUID{req.DatabaseID},
		)
	}

	return approvers.mayApproveFor(req.DatabaseID)
}

// resolveApprovalGrant finds the grant whose approver_user_group_uids govern this
// query's hold.
//
// Preferred: the grant the query's connection was stamped with at auth time
// (connections.grant_uid). That is the grant whose approval_patterns actually
// triggered the hold, so it is also the grant whose approver groups get to
// resolve it. Getting this wrong is a real bug this replaces: without the
// stamp, the check re-resolves "the active grant" at approval time via
// GetActiveGrant, which — if a newer, higher-priority grant was created for
// the same user/database while the query sat on hold — returns that *newer*
// grant, whose approver_user_group_uids have nothing to do with why the hold
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
	return s.mayViewQueryWith(ctx, user, query, nil)
}

// mayViewQueryWith is mayViewQuery against an optional per-listing prefetch.
func (s *Server) mayViewQueryWith(
	ctx context.Context, user *store.User, query *store.Query, approvers *listingApprovers,
) bool {
	if user == nil {
		return false
	}

	if user.IsAdmin() || user.IsViewer() {
		return true
	}

	return s.mayApproveQueryWith(ctx, user, query, approvers)
}

// approvalEscalator is the slice of the Slack escalator the API needs, kept as
// a local interface so the API package doesn't have to import the proxy.
type approvalEscalator interface {
	Resolved(ctx context.Context, queryUID uuid.UUID, status, byName, reason string)
}
