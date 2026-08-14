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
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// slackNotifyTimeout caps how long we'll spend posting to Slack before
// the goroutine gets canceled. Slack typically responds in <1s but we
// don't want a slow Slack to leak goroutines forever.
const slackNotifyTimeout = 5 * time.Second

// goroutineNameGrantNotify is what a panic in the detached notify is logged
// under.
const goroutineNameGrantNotify = "slack grant request notification"

// notifyAsync fires a Slack notification in the background. The notifier
// is a graceful no-op when nil (feature disabled), so callers don't need
// to gate on configuration. We pass a fresh context so request
// cancellation doesn't kill the notify in-flight.
func (s *Server) notifyAsync(ev notify.GrantRequestEvent) {
	if s.notifier == nil {
		return
	}

	// Guarded for the same reason the proxies' detached writes are: this
	// goroutine outlives the request that spawned it, so the HTTP recover never
	// sees it and a panic in the notifier would end the *process* — every live
	// proxy session included. Nothing waits on it, so recovering costs one
	// unsent Slack message.
	go shared.RunGuarded(context.Background(), s.logger, goroutineNameGrantNotify, func() {
		ctx, cancel := context.WithTimeout(context.Background(), slackNotifyTimeout)
		defer cancel()

		s.notifier.NotifyGrantRequest(ctx, ev)
	})
}

// loadEventContext gathers the related rows the notifier needs to render a
// message. Errors are logged and the partial event is returned — the
// notifier can render with nil pointers, just less informatively.
func (s *Server) loadEventContext(ctx context.Context, req *store.GrantRequest, decider *store.User) notify.GrantRequestEvent {
	ev := notify.GrantRequestEvent{Request: req, Decider: decider}

	// The store attaches the live version of the request's definition on every
	// read path; use it so a Slack message names the same version the web UI
	// shows. The by-uid lookup stays as a fallback for the few callers that
	// hand-build a request (tests, and the Slack interaction path, which
	// re-reads by uid anyway).
	if req.Definition != nil {
		ev.Definition = req.Definition
	} else if def, err := s.store.GetGrantDefinition(ctx, req.GrantDefinitionID); err == nil {
		ev.Definition = def
	}

	if db, err := s.store.GetServerByUID(ctx, req.DatabaseID); err == nil {
		ev.Server = db
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

		ev.ApproverSlackIDs = s.approverSlackIDsForRequest(ctx, req)
	}

	return ev
}

// approverSlackIDsForRequest returns the Slack user IDs to @-mention on a
// pending request: every admin, plus every member of the access-approver groups
// resolved for the target database (its own list, else the union of its server
// groups'). The two sets are unioned and de-duplicated, so an admin who is also
// in an approver group is mentioned once.
//
// The requester is deliberately *not* filtered out: they may well be a member
// of the approving group, and the mention is informational — the decision
// itself refuses self-approval on every path.
//
// Best-effort throughout: this decorates a notification, and a lookup failure
// must not stop it going out.
func (s *Server) approverSlackIDsForRequest(ctx context.Context, req *store.GrantRequest) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)

	add := func(candidates []string) {
		for _, id := range candidates {
			if id == "" {
				continue
			}

			if _, dup := seen[id]; dup {
				continue
			}

			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	if admins, err := s.store.ListAdminSlackUserIDs(ctx); err == nil {
		add(admins)
	} else {
		s.logger.WarnContext(ctx, "list admin slack ids failed", slog.Any("error", err))
	}

	groupUIDs, err := s.store.ResolveServerApproverGroups(ctx, req.DatabaseID, store.ApproverKindAccess)
	if err != nil {
		s.logger.WarnContext(ctx, "resolve access approver groups failed", slog.Any("error", err))

		return ids
	}

	for _, groupUID := range groupUIDs {
		members, err := s.store.ListUserGroupMemberUIDs(ctx, groupUID)
		if err != nil {
			s.logger.WarnContext(ctx, "list approver group members failed", slog.Any("error", err))

			continue
		}

		for _, memberUID := range members {
			add([]string{s.slackIDForUser(ctx, memberUID)})
		}
	}

	return ids
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
	// GrantDefinitionID identifies the definition being requested — either
	// its uid or its slug. Widened from uid-only to accept a slug too,
	// rather than adding a sibling field, since this is the one place the
	// API takes a bare definition reference as a request-body value (every
	// other reference is a path param, resolved the same uid-or-slug way).
	GrantDefinitionID string    `json:"grant_definition_id" binding:"required"`
	DatabaseID        uuid.UUID `json:"database_id" binding:"required"`
	Justification     string    `json:"justification"`
}

// DenyGrantRequestRequest is the body for POST /grant-requests/:uid/deny.
type DenyGrantRequestRequest struct {
	Reason string `json:"reason"`
}

const maxJustificationLen = 1000

// enforceRequestScope writes an error response and returns false when the
// definition's user-group / server-group scope does not cover this requester
// and target. This is the security-critical gate: it also covers the
// auto-approve path, where no human ever reviews the request.
//
// The database side resolves the target's *current* server-group membership,
// so a server added to a scoped group becomes requestable immediately, with no
// edit to any definition.
func (s *Server) enforceRequestScope(
	c *gin.Context,
	def *store.GrantDefinition,
	requester *store.User,
	databaseID uuid.UUID,
) bool {
	ctx := c.Request.Context()

	groupUIDs, err := s.store.ListUserGroupUIDs(ctx, requester.UID)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list user groups")

		return false
	}

	if !def.AppliesToUserGroups(groupUIDs) {
		writeError(c, http.StatusForbidden, ErrCodeForbidden,
			"this grant definition is not available to your user groups")

		return false
	}

	serverGroupUIDs, err := s.store.ListServerGroupUIDsForServer(ctx, databaseID)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list server groups")

		return false
	}

	if !def.AppliesToServerGroups(serverGroupUIDs) {
		writeError(c, http.StatusForbidden, ErrCodeForbidden,
			"this grant definition cannot be requested for this database")

		return false
	}

	return true
}

// resolveLiveGrantDefinition resolves a uid-or-slug reference to the **live**
// version of that definition's lineage, writing the error response itself and
// returning ok=false when it cannot.
//
// Naming an archived version by uid still yields the live one: issuing (or
// requesting) a superseded policy is exactly what versioning exists to
// prevent, and pinning the live row keeps uid- and slug-based callers
// comparing like with like.
func (s *Server) resolveLiveGrantDefinition(c *gin.Context, idOrSlug string) (*store.GrantDefinition, bool) {
	ctx := c.Request.Context()

	def, err := s.resolveGrantDefinition(ctx, idOrSlug)
	if err != nil {
		if errors.Is(err, store.ErrGrantDefinitionNotFound) {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "grant_definition_id does not exist")

			return nil, false
		}

		writeInternalError(c, s.logger, err, "failed to load grant definition")

		return nil, false
	}

	if def.IsLive() {
		return def, true
	}

	live, err := s.store.GetLiveGrantDefinition(ctx, def.UID)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to resolve the live grant definition")

		return nil, false
	}

	return live, true
}

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

	def, ok := s.resolveLiveGrantDefinition(c, req.GrantDefinitionID)
	if !ok {
		return
	}

	if !def.IsActive {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "grant definition is not active")

		return
	}

	if def.AutoApprove && req.Justification == "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			"justification is required for auto-approved grant definitions")

		return
	}

	if !s.enforceRequestScope(c, def, currentUser, req.DatabaseID) {
		return
	}

	// The target must be a database, never a tunnel row (a dial path).
	if target, err := s.store.GetServerByUID(ctx, req.DatabaseID); err == nil && target.IsTunnel() {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "cannot request access to a "+target.Protocol+" tunnel server")

		return
	}

	pending, err := s.store.HasPendingRequest(ctx, currentUser.UID, def.UID, req.DatabaseID)
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
		GrantDefinitionID: def.UID,
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

	if def.AutoApprove {
		outcome, err := s.autoApproveGrantRequest(ctx, created, currentUser)
		if err != nil {
			// The request row already exists as pending; degrade to the
			// normal flow (log + notify as pending) rather than failing the
			// whole HTTP request over what's likely a rare race (e.g. the
			// definition was deactivated a moment after we checked).
			s.logger.ErrorContext(ctx, "auto-approve grant request failed",
				slog.String("grant_request_uid", created.UID.String()), slog.Any("error", err))

			ev := s.loadEventContext(ctx, created, nil)
			ev.Action = notify.GrantActionCreated
			s.notifyAsync(ev)

			successResponse(c, created)

			return
		}

		successResponse(c, outcome.Request)

		return
	}

	ev := s.loadEventContext(ctx, created, nil)
	ev.Action = notify.GrantActionCreated
	s.notifyAsync(ev)

	successResponse(c, created)
}

// handleListGrantRequests — role-aware. Admins see all (filterable);
// everybody else sees their own, plus the pending requests they are an access
// approver for. Without that second half the Slack deep-link and the
// pending-requests badge would send a delegated approver to an empty page.
func (s *Server) handleListGrantRequests(c *gin.Context) {
	currentUser := getCurrentUser(c)
	filter := store.GrantRequestFilter{}

	if !currentUser.IsAdmin() {
		s.listGrantRequestsForNonAdmin(c, currentUser)

		return
	}

	if userID := c.Query("user_id"); userID != "" {
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

	// Admins wear the admin hat on every row and never consult the chain, so
	// this path needs no prefetch.
	successResponse(c, gin.H{
		"grant_requests": s.withApproverHats(c.Request.Context(), currentUser, requests, nil),
	})
}

// grantRequestResponse is a request plus the hat the *current* viewer would
// wear to decide it — "admin", "server_approver", or empty for somebody who may
// see it but not resolve it (their own request, most often).
type grantRequestResponse struct {
	store.GrantRequest

	ApproverRole string `json:"approver_role"`
}

// withApproverHats decorates a request list for one viewer. The hat is computed
// per request because the answer is per *server*: an ops lead may decide the
// staging rows in a list and none of the production ones.
//
// approvers is the page-wide resolution when the caller has one, and nil when
// it does not (an admin listing, where the chain is never reached). Passing it
// changes only how many round trips the decoration costs, never a hat.
func (s *Server) withApproverHats(
	ctx context.Context, user *store.User, requests []store.GrantRequest, approvers *listingApprovers,
) []grantRequestResponse {
	out := make([]grantRequestResponse, 0, len(requests))

	for i := range requests {
		out = append(out, grantRequestResponse{
			GrantRequest: requests[i],
			ApproverRole: s.approverHatForRequestWith(ctx, user, &requests[i], approvers),
		})
	}

	return out
}

// listGrantRequestsForNonAdmin returns the caller's own requests plus the
// *pending* ones they may decide as an access approver.
//
// The pending restriction is about **enumeration**, not about secrecy. A
// listing is a bulk read, and an unfiltered one would hand an ops group the
// entire decision history of every colleague who ever requested access to their
// servers, in one page, as a browsing surface. Answering what is waiting needs
// none of that.
//
// It is deliberately *not* the same rule as handleGetGrantRequest, which
// resolves a single uid the caller already holds and therefore does not
// status-scope — see the comment there. Enumerating a history and re-reading
// one row of it are different exposures, so they get different rules; what they
// share is who counts as an approver.
//
// The approver side is resolved per request rather than by a store-side filter
// because the chain (server list, else its groups' union) is live and lives in
// one function — a second, SQL-shaped implementation is exactly the drift the
// single resolver exists to prevent. The pending set is small by nature.
func (s *Server) listGrantRequestsForNonAdmin(c *gin.Context, currentUser *store.User) {
	ctx := c.Request.Context()

	own, err := s.store.ListGrantRequests(ctx, store.GrantRequestFilter{UserID: &currentUser.UID})
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list grant requests")

		return
	}

	pendingStatus := store.GrantRequestPending

	pending, err := s.store.ListGrantRequests(ctx, store.GrantRequestFilter{Status: &pendingStatus})
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list grant requests")

		return
	}

	seen := make(map[uuid.UUID]struct{}, len(own))
	for i := range own {
		seen[own[i].UID] = struct{}{}
	}

	// Resolve the access approvers of every distinct database on this page once,
	// then decide each row against that. The chain is the same one
	// mayDecideGrantRequest walks row by row — it is simply asked about the
	// whole page in one go, and the hats below reuse the very same answer
	// instead of walking it a second time.
	databases := make([]*uuid.UUID, 0, len(pending)+len(own))
	for i := range pending {
		databases = append(databases, &pending[i].DatabaseID)
	}

	// The caller's own rows never reach the chain (self-approval is refused
	// before it), but they are decorated from the same map, so keeping their
	// databases in it means the map covers every row it is ever asked about.
	for i := range own {
		databases = append(databases, &own[i].DatabaseID)
	}

	approvers := s.prefetchApprovers(ctx, currentUser, store.ApproverKindAccess, distinctUIDs(databases))

	out := own

	for i := range pending {
		if _, dup := seen[pending[i].UID]; dup {
			continue
		}

		if s.mayDecideGrantRequestWith(ctx, currentUser, &pending[i], approvers) {
			out = append(out, pending[i])
		}
	}

	successResponse(c, gin.H{"grant_requests": s.withApproverHats(ctx, currentUser, out, approvers)})
}

// handleGetGrantRequest — role-aware: requesters fetch their own, admins fetch
// anyone's, and an access approver fetches the ones they may decide.
//
// **Not status-scoped, on purpose**, unlike listGrantRequestsForNonAdmin above.
// An approver may re-read a request after it has been decided — including one
// they decided themselves — because hiding the outcome of your own decision is
// a worse answer than the marginal exposure of a row you were entitled to
// resolve a moment earlier. The UI refetches a row straight after approving it,
// and a pending-only rule would 403 on exactly that.
//
// The two paths differ only in what they scope, and each scopes the thing that
// is actually risky about it: a listing enumerates, a lookup needs a uid the
// caller already holds. `mayDecideGrantRequest` — the *who* — is the same
// function on both, so they cannot drift on the part that matters.
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

	if !currentUser.IsAdmin() && req.UserID != currentUser.UID &&
		!s.mayDecideGrantRequest(c.Request.Context(), currentUser, req) {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "no access to this grant request")

		return
	}

	successResponse(c, grantRequestResponse{
		GrantRequest: *req,
		ApproverRole: s.approverHatForRequest(c.Request.Context(), currentUser, req),
	})
}

// decisionSource records where a grant decision originated, recorded in the
// audit event `details.via` field so Slack- and UI-driven decisions can be
// told apart. The web UI omits it (via is only set when non-default).
type decisionSource string

const (
	decisionSourceWeb         decisionSource = "web"
	decisionSourceSlack       decisionSource = "slack"
	decisionSourceAutoApprove decisionSource = "auto_approve"
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

// ErrRequestOutOfScope is returned when a pending request no longer matches
// its definition's scope — typically because an admin tightened the scope
// after the request was filed. It hard-blocks the approval rather than
// silently granting access the current policy no longer allows; an admin who
// still wants to grant it can always create a direct grant.
var ErrRequestOutOfScope = errors.New("grant request is out of the definition's scope")

// checkRequestInScope re-validates a pending request against its definition's
// current group/database scope. A missing request or definition is left to the
// store transition to report, so error mapping stays in one place.
func (s *Server) checkRequestInScope(ctx context.Context, uid uuid.UUID) error {
	request, err := s.store.GetGrantRequest(ctx, uid)
	if err != nil {
		return nil //nolint:nilerr // the store transition reports not-found
	}

	// The live version of the lineage, since that is the one the approval will
	// actually materialize — scope has to be judged against the policy being
	// issued, not against the version the request was filed under.
	def, err := s.store.GetLiveGrantDefinition(ctx, request.GrantDefinitionID)
	if err != nil {
		return nil //nolint:nilerr // the store transition reports not-found
	}

	groupUIDs, err := s.store.ListUserGroupUIDs(ctx, request.UserID)
	if err != nil {
		return err
	}

	// The target's server-group membership is read now, not at request time:
	// scope has to be judged against the fleet as it stands when the grant is
	// issued.
	serverGroupUIDs, err := s.store.ListServerGroupUIDsForServer(ctx, request.DatabaseID)
	if err != nil {
		return err
	}

	if !def.AppliesTo(groupUIDs, serverGroupUIDs) {
		return ErrRequestOutOfScope
	}

	return nil
}

// approveGrantRequest runs the approve decision: store transition, audit
// event (with source), and async notification. It returns the raw store
// error unmapped so each caller can translate it to its own transport
// (HTTP status vs Slack ephemeral). Mirrors the deny path below.
func (s *Server) approveGrantRequest(ctx context.Context, uid uuid.UUID, decider *store.User, source decisionSource) (*decideOutcome, error) {
	if err := s.checkRequestInScope(ctx, uid); err != nil {
		return nil, err
	}

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
	s.notifyAsync(ev) //nolint:contextcheck // notifyAsync detaches by design

	return &decideOutcome{Request: request, Grant: grant, Event: ev, Action: notify.GrantActionApproved}, nil
}

// autoApproveGrantRequest runs the auto-approve decision for a just-created
// request whose definition has AutoApprove set: store transition (no human
// decider — decided_by stays nil), audit event marked `via: auto_approve`,
// and async notification (rendered as an already-approved message, so no
// Approve/Deny buttons ever appear for it).
func (s *Server) autoApproveGrantRequest(ctx context.Context, request *store.GrantRequest, requester *store.User) (*decideOutcome, error) {
	grant, updated, err := s.store.AutoApproveGrantRequest(ctx, request.UID, requester.UID)
	if err != nil {
		return nil, err
	}

	details := decisionDetails(map[string]any{
		"grant_request_uid":  updated.UID,
		"resulting_grant_id": grant.UID,
	}, decisionSourceAutoApprove)

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType: "grant_request.approved",
		UserID:    &updated.UserID,
		Details:   details,
	})

	ev := s.loadEventContext(ctx, updated, nil)
	ev.Action = notify.GrantActionApproved
	s.notifyAsync(ev) //nolint:contextcheck // notifyAsync detaches by design

	return &decideOutcome{Request: updated, Grant: grant, Event: ev, Action: notify.GrantActionApproved}, nil
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
	s.notifyAsync(ev) //nolint:contextcheck // notifyAsync detaches by design

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

// authorizeGrantRequestDecision loads the request named by the URL and gates
// the decision on it, writing the error response itself. It is the sole
// authorization for approve and deny: the routes are no longer behind
// requireAdmin, because an access approver resolved off the target server is
// now a legitimate decider.
//
// The 403 messages separate "you're the requester" from "you're not an
// approver" — unlike the Slack ephemeral, a signed-in user looking at their own
// request deserves to know which rule stopped them.
func (s *Server) authorizeGrantRequestDecision(c *gin.Context, uid uuid.UUID) bool {
	ctx := c.Request.Context()

	req, err := s.store.GetGrantRequest(ctx, uid)
	if err != nil {
		if errors.Is(err, store.ErrGrantRequestNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant request not found")

			return false
		}

		writeInternalError(c, s.logger, err, "failed to get grant request")

		return false
	}

	currentUser := getCurrentUser(c)

	if s.mayDecideGrantRequest(ctx, currentUser, req) {
		return true
	}

	if currentUser != nil && req.UserID == currentUser.UID {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "you cannot decide your own grant request")

		return false
	}

	writeError(c, http.StatusForbidden, ErrCodeForbidden, "you are not an approver for this database")

	return false
}

// handleApproveGrantRequest — admin or access approver; flips pending →
// approved and materializes the grant in the same transaction.
func (s *Server) handleApproveGrantRequest(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant request UID")

		return
	}

	if !s.authorizeGrantRequestDecision(c, uid) {
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
		case errors.Is(err, ErrRequestOutOfScope):
			writeError(c, http.StatusConflict, ErrCodeConflict,
				"the grant definition's scope no longer covers this user or database")
		default:
			writeInternalError(c, s.logger, err, "failed to approve grant request")
		}

		return
	}

	successResponse(c, gin.H{"grant_request": outcome.Request, "grant": outcome.Grant})
}

// handleDenyGrantRequest — admin or access approver, same gate as approve.
func (s *Server) handleDenyGrantRequest(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant request UID")

		return
	}

	if !s.authorizeGrantRequestDecision(c, uid) {
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
