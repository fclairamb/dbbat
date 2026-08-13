package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// serverApproverFixture is a target server, a user group, and one member of
// that group who is *not* an admin — the whole point of the feature being that
// such a person can decide things.
type serverApproverFixture struct {
	server    *Server
	dataStore *store.Store
	ctx       context.Context //nolint:containedctx // fixture convenience, test-only

	target    *store.Server
	group     *store.UserGroup
	approver  *store.User
	outsider  *store.User
	requester *store.User
	admin     *store.User
}

func newServerApproverFixture(t *testing.T, suffix string) *serverApproverFixture {
	t.Helper()

	srv, dataStore := setupTestServer(t)
	ctx := context.Background()

	target, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "target-" + suffix,
		Host:         "127.0.0.1",
		Port:         5432,
		DatabaseName: "prod",
		Username:     "pg",
		Password:     "secret",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
	}, approvalTestKey)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	group, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "ops-" + suffix})
	if err != nil {
		t.Fatalf("create user group: %v", err)
	}

	approver := createTestUser(t, dataStore, "approver-"+suffix, "approverpass123", []string{store.RoleConnector})
	outsider := createTestUser(t, dataStore, "outsider-"+suffix, "outsiderpass123", []string{store.RoleConnector})
	requester := createTestUser(t, dataStore, "requester-"+suffix, "requesterpass123", []string{store.RoleConnector})
	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})

	if err := dataStore.AddUserToUserGroup(ctx, group.UID, approver.UID); err != nil {
		t.Fatalf("add user to group: %v", err)
	}

	return &serverApproverFixture{
		server: srv, dataStore: dataStore, ctx: ctx,
		target: target, group: group,
		approver: approver, outsider: outsider, requester: requester, admin: admin,
	}
}

// setServerApprovers writes one of the server's two approver lists.
func (f *serverApproverFixture) setServerApprovers(t *testing.T, kind store.ApproverKind, groups ...uuid.UUID) {
	t.Helper()

	list := groups
	if list == nil {
		list = []uuid.UUID{}
	}

	update := store.ServerUpdate{}
	if kind == store.ApproverKindAccess {
		update.AccessApproverUserGroupUIDs = &list
	} else {
		update.QueryApproverUserGroupUIDs = &list
	}

	if err := f.dataStore.UpdateServer(f.ctx, f.target.UID, update, approvalTestKey); err != nil {
		t.Fatalf("UpdateServer(): %v", err)
	}
}

// putTargetInGroupWithApprovers creates a server group holding the target and
// naming the given approver groups for one kind.
func (f *serverApproverFixture) putTargetInGroupWithApprovers(
	t *testing.T, name string, kind store.ApproverKind, groups ...uuid.UUID,
) *store.ServerGroup {
	t.Helper()

	sg := &store.ServerGroup{Name: name}
	if kind == store.ApproverKindAccess {
		sg.AccessApproverUserGroupUIDs = groups
	} else {
		sg.QueryApproverUserGroupUIDs = groups
	}

	created, err := f.dataStore.CreateServerGroup(f.ctx, sg)
	if err != nil {
		t.Fatalf("CreateServerGroup(): %v", err)
	}

	if err := f.dataStore.AddServerToGroup(f.ctx, created.UID, f.target.UID); err != nil {
		t.Fatalf("AddServerToGroup(): %v", err)
	}

	return created
}

// newGrantRequest files a pending request from the given user for the target.
func (f *serverApproverFixture) newGrantRequest(t *testing.T, suffix string, requester *store.User) *store.GrantRequest {
	t.Helper()

	def := createTestGrantDefinition(t, f.dataStore, *f.admin, "def-"+suffix, false)

	created, err := f.dataStore.CreateGrantRequest(f.ctx, &store.GrantRequest{
		UserID:            requester.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        f.target.UID,
		Justification:     "because",
	})
	if err != nil {
		t.Fatalf("CreateGrantRequest(): %v", err)
	}

	return created
}

// decide runs the approve handler for one request as one user.
func (f *serverApproverFixture) decide(t *testing.T, user *store.User, req *store.GrantRequest) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/grant-requests/x/approve", nil)
	c.Params = gin.Params{{Key: "uid", Value: req.UID.String()}}
	c.Set(contextKeyUser, user)

	f.server.handleApproveGrantRequest(c)

	return w
}

// TestGrantRequestDecision_ServerAccessApprover walks the delegation: an
// ordinary user in the server's access-approver group decides, and nobody else
// does.
func TestGrantRequestDecision_ServerAccessApprover(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "gr-access")

	// Nothing configured: the pre-existing world, where only admins decide.
	req := f.newGrantRequest(t, "gr-access-1", f.requester)
	if w := f.decide(t, f.approver, req); w.Code != http.StatusForbidden {
		t.Fatalf("unconfigured approver got %d, want 403", w.Code)
	}

	f.setServerApprovers(t, store.ApproverKindAccess, f.group.UID)

	// The same person, now named on the server, decides.
	if w := f.decide(t, f.approver, req); w.Code != http.StatusOK {
		t.Fatalf("named access approver got %d, want 200: %s", w.Code, w.Body.String())
	}

	// Somebody in no approver group still cannot.
	other := f.newGrantRequest(t, "gr-access-2", f.requester)
	if w := f.decide(t, f.outsider, other); w.Code != http.StatusForbidden {
		t.Fatalf("outsider got %d, want 403", w.Code)
	}

	// And the query kind buys nothing here: no hierarchy between the two.
	f.setServerApprovers(t, store.ApproverKindAccess)
	f.setServerApprovers(t, store.ApproverKindQuery, f.group.UID)

	if w := f.decide(t, f.approver, other); w.Code != http.StatusForbidden {
		t.Fatalf("query approver deciding a grant request got %d, want 403", w.Code)
	}
}

// TestGrantRequestDecision_SelfApprovalRefusedForApprover is the security
// invariant: being named as an approver never lets you decide your own request.
func TestGrantRequestDecision_SelfApprovalRefusedForApprover(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "gr-self")
	f.setServerApprovers(t, store.ApproverKindAccess, f.group.UID)

	// The approver files the request themselves.
	own := f.newGrantRequest(t, "gr-self-1", f.approver)

	w := f.decide(t, f.approver, own)
	if w.Code != http.StatusForbidden {
		t.Fatalf("self-approval got %d, want 403", w.Code)
	}

	if !strings.Contains(w.Body.String(), "your own grant request") {
		t.Errorf("self-approval message = %s, want it to name the rule", w.Body.String())
	}

	// The request is untouched: an admin can still decide it.
	if w := f.decide(t, f.admin, own); w.Code != http.StatusOK {
		t.Fatalf("admin deciding the approver's own request got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestGrantRequestDecision_ServerGroupFallbackAndUnion covers the two remaining
// steps of the chain: the group-level fallback, and the union when several
// groups hold the same server.
func TestGrantRequestDecision_ServerGroupFallbackAndUnion(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "gr-group")

	secondGroup, err := f.dataStore.CreateUserGroup(f.ctx, &store.UserGroup{Name: "leads-gr-group"})
	if err != nil {
		t.Fatalf("create user group: %v", err)
	}

	if err := f.dataStore.AddUserToUserGroup(f.ctx, secondGroup.UID, f.outsider.UID); err != nil {
		t.Fatalf("add user to group: %v", err)
	}

	// Group A names the ops group; nothing is set on the server itself.
	f.putTargetInGroupWithApprovers(t, "sg-a-gr-group", store.ApproverKindAccess, f.group.UID)

	req := f.newGrantRequest(t, "gr-group-1", f.requester)
	if w := f.decide(t, f.approver, req); w.Code != http.StatusOK {
		t.Fatalf("group-level approver got %d, want 200: %s", w.Code, w.Body.String())
	}

	// Group B adds a second approver group for the same server. Both work.
	f.putTargetInGroupWithApprovers(t, "sg-b-gr-group", store.ApproverKindAccess, secondGroup.UID)

	fromA := f.newGrantRequest(t, "gr-group-2", f.requester)
	if w := f.decide(t, f.approver, fromA); w.Code != http.StatusOK {
		t.Fatalf("union member A got %d, want 200: %s", w.Code, w.Body.String())
	}

	fromB := f.newGrantRequest(t, "gr-group-3", f.requester)
	if w := f.decide(t, f.outsider, fromB); w.Code != http.StatusOK {
		t.Fatalf("union member B got %d, want 200: %s", w.Code, w.Body.String())
	}

	// The server's own list overrides the groups outright: naming only the ops
	// group there takes the rights away from group B's members.
	f.setServerApprovers(t, store.ApproverKindAccess, f.group.UID)

	overridden := f.newGrantRequest(t, "gr-group-4", f.requester)
	if w := f.decide(t, f.outsider, overridden); w.Code != http.StatusForbidden {
		t.Fatalf("group-B member after a server-level override got %d, want 403", w.Code)
	}

	if w := f.decide(t, f.approver, overridden); w.Code != http.StatusOK {
		t.Fatalf("server-level approver got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// queryHoldFor parks a statement run by `runner` against the fixture's target
// and returns it.
func (f *serverApproverFixture) queryHoldFor(t *testing.T, runner *store.User) *store.Query {
	t.Helper()

	return f.queryHoldOn(t, runner, f.target.UID)
}

// queryHoldOn is queryHoldFor against an explicit server — what the batched
// listing test needs, since the whole point is a page spanning several
// databases with different approver configurations.
func (f *serverApproverFixture) queryHoldOn(t *testing.T, runner *store.User, databaseID uuid.UUID) *store.Query {
	t.Helper()

	conn, err := f.dataStore.CreateConnection(f.ctx, runner.UID, databaseID, "127.0.0.1")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	query, err := f.dataStore.CreatePendingQuery(f.ctx, &store.Query{
		ConnectionID: conn.UID,
		SQLText:      "DELETE FROM users",
		ExecutedAt:   time.Now(),
	}, `(?i)^DELETE\s+FROM`)
	if err != nil {
		t.Fatalf("create pending query: %v", err)
	}

	return query
}

// resolveHold runs the approve handler for one held query as one user.
func (f *serverApproverFixture) resolveHold(t *testing.T, user *store.User, query *store.Query) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/queries/x/approve", nil)
	c.Params = gin.Params{{Key: "uid", Value: query.UID.String()}}
	c.Set(contextKeyUser, user)

	f.server.handleApproveQuery(c)

	return w
}

// TestQueryHold_ServerQueryApproverFallback proves mayApproveQuery falls
// through to the server chain when the grant carries no approver groups — and
// stops at admins when nothing is configured at all.
func TestQueryHold_ServerQueryApproverFallback(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "qh-fallback")

	// Nothing configured: fail closed, exactly as before this feature.
	held := f.queryHoldFor(t, f.requester)
	if w := f.resolveHold(t, f.approver, held); w.Code != http.StatusForbidden {
		t.Fatalf("unconfigured approver got %d, want 403", w.Code)
	}

	// Named on the server's query-approver list: through.
	f.setServerApprovers(t, store.ApproverKindQuery, f.group.UID)

	if w := f.resolveHold(t, f.approver, held); w.Code != http.StatusOK {
		t.Fatalf("server query approver got %d, want 200: %s", w.Code, w.Body.String())
	}

	// An access approver gains nothing on this side.
	f.setServerApprovers(t, store.ApproverKindQuery)
	f.setServerApprovers(t, store.ApproverKindAccess, f.group.UID)

	second := f.queryHoldFor(t, f.requester)
	if w := f.resolveHold(t, f.approver, second); w.Code != http.StatusForbidden {
		t.Fatalf("access approver resolving a hold got %d, want 403", w.Code)
	}
}

// TestQueryHold_ServerGroupFallback covers the group level of the same chain.
func TestQueryHold_ServerGroupFallback(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "qh-group")
	f.putTargetInGroupWithApprovers(t, "sg-qh-group", store.ApproverKindQuery, f.group.UID)

	held := f.queryHoldFor(t, f.requester)
	if w := f.resolveHold(t, f.approver, held); w.Code != http.StatusOK {
		t.Fatalf("server-group query approver got %d, want 200: %s", w.Code, w.Body.String())
	}

	// Somebody in no approver group is still refused.
	other := f.queryHoldFor(t, f.requester)
	if w := f.resolveHold(t, f.outsider, other); w.Code != http.StatusForbidden {
		t.Fatalf("outsider got %d, want 403", w.Code)
	}
}

// TestQueryHold_SelfApprovalRefusedForServerApprover is the hold-side half of
// the security invariant.
func TestQueryHold_SelfApprovalRefusedForServerApprover(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "qh-self")
	f.setServerApprovers(t, store.ApproverKindQuery, f.group.UID)

	// The approver's own statement is held.
	own := f.queryHoldFor(t, f.approver)

	if w := f.resolveHold(t, f.approver, own); w.Code != http.StatusForbidden {
		t.Fatalf("self-approval got %d, want 403", w.Code)
	}

	// The hat reported to the UI agrees with the gate: no hat on your own hold.
	if hat := f.server.approverHatForQuery(f.ctx, f.approver, mustQueryWithOwner(t, f, own)); hat != ApproverHatNone {
		t.Errorf("approverHatForQuery(own hold) = %q, want empty", hat)
	}

	// Somebody else in the same group resolves it, so the hold is not stuck.
	if err := f.dataStore.AddUserToUserGroup(f.ctx, f.group.UID, f.outsider.UID); err != nil {
		t.Fatalf("add user to group: %v", err)
	}

	if w := f.resolveHold(t, f.outsider, own); w.Code != http.StatusOK {
		t.Fatalf("second group member got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestApproverHats checks the labels the UI renders match the gate that
// actually decides — a label promising a right the gate refuses would be worse
// than no label.
func TestApproverHats(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "hats")
	f.setServerApprovers(t, store.ApproverKindQuery, f.group.UID)

	held := mustQueryWithOwner(t, f, f.queryHoldFor(t, f.requester))

	if hat := f.server.approverHatForQuery(f.ctx, f.admin, held); hat != ApproverHatAdmin {
		t.Errorf("admin hat = %q, want %q", hat, ApproverHatAdmin)
	}

	if hat := f.server.approverHatForQuery(f.ctx, f.approver, held); hat != ApproverHatServer {
		t.Errorf("server approver hat = %q, want %q", hat, ApproverHatServer)
	}

	if hat := f.server.approverHatForQuery(f.ctx, f.outsider, held); hat != ApproverHatNone {
		t.Errorf("outsider hat = %q, want empty", hat)
	}

	req := f.newGrantRequest(t, "hats-1", f.requester)
	f.setServerApprovers(t, store.ApproverKindAccess, f.group.UID)

	if hat := f.server.approverHatForRequest(f.ctx, f.approver, req); hat != ApproverHatServer {
		t.Errorf("request approver hat = %q, want %q", hat, ApproverHatServer)
	}

	if hat := f.server.approverHatForRequest(f.ctx, f.requester, req); hat != ApproverHatNone {
		t.Errorf("requester hat on their own request = %q, want empty", hat)
	}
}

func mustQueryWithOwner(t *testing.T, f *serverApproverFixture, q *store.Query) *store.Query {
	t.Helper()

	full, err := f.dataStore.GetQueryWithOwner(f.ctx, q.UID)
	if err != nil {
		t.Fatalf("GetQueryWithOwner(): %v", err)
	}

	return full
}

// newTargetServer adds a second (or third) database target to the fixture, for
// the tests that need a server the approver has no say over.
func (f *serverApproverFixture) newTargetServer(t *testing.T, name string) *store.Server {
	t.Helper()

	created, err := f.dataStore.CreateServer(f.ctx, &store.Server{
		Name:         name,
		Host:         "127.0.0.1",
		Port:         5432,
		DatabaseName: "other",
		Username:     "pg",
		Password:     "secret",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
	}, approvalTestKey)
	if err != nil {
		t.Fatalf("create server %s: %v", name, err)
	}

	return created
}

// newGrantRequestFor is newGrantRequest against an explicit server.
func (f *serverApproverFixture) newGrantRequestFor(
	t *testing.T, suffix string, requester *store.User, databaseID uuid.UUID,
) *store.GrantRequest {
	t.Helper()

	def := createTestGrantDefinition(t, f.dataStore, *f.admin, "def-"+suffix, false)

	created, err := f.dataStore.CreateGrantRequest(f.ctx, &store.GrantRequest{
		UserID:            requester.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        databaseID,
		Justification:     "because",
	})
	if err != nil {
		t.Fatalf("CreateGrantRequest(): %v", err)
	}

	return created
}

// listRequestsAs runs the listing handler as one user and returns the uids it
// answered with, plus each row's reported approver_role.
func (f *serverApproverFixture) listRequestsAs(t *testing.T, user *store.User) map[string]string {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/grant-requests", nil)
	c.Set(contextKeyUser, user)

	f.server.handleListGrantRequests(c)

	if w.Code != http.StatusOK {
		t.Fatalf("list got %d, want 200: %s", w.Code, w.Body.String())
	}

	var body struct {
		GrantRequests []struct {
			UID          string `json:"uid"`
			ApproverRole string `json:"approver_role"`
		} `json:"grant_requests"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode listing: %v (%s)", err, w.Body.String())
	}

	out := make(map[string]string, len(body.GrantRequests))
	for _, r := range body.GrantRequests {
		out[r.UID] = r.ApproverRole
	}

	return out
}

// TestListGrantRequests_NonAdminScoping pins the one data-exposure path this
// change widened: what a non-admin sees in the listing.
//
// An ordinary user sees their own requests and nothing else. An access approver
// additionally sees the *pending* requests for the servers they approve — not
// the decided ones (a listing is an enumeration, and their history is not the
// approver's business), and not other servers' at all.
func TestListGrantRequests_NonAdminScoping(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "list-scope")
	elsewhere := f.newTargetServer(t, "elsewhere-list-scope")

	mine := f.newGrantRequestFor(t, "list-scope-mine", f.approver, elsewhere.UID)
	pendingHere := f.newGrantRequestFor(t, "list-scope-here", f.requester, f.target.UID)
	pendingElsewhere := f.newGrantRequestFor(t, "list-scope-away", f.requester, elsewhere.UID)
	decidedHere := f.newGrantRequestFor(t, "list-scope-done", f.requester, f.target.UID)

	if _, err := f.dataStore.DenyGrantRequest(f.ctx, decidedHere.UID, f.admin.UID, "no"); err != nil {
		t.Fatalf("DenyGrantRequest(): %v", err)
	}

	// Before any delegation: the approver is an ordinary user and sees only
	// the one request they filed themselves.
	got := f.listRequestsAs(t, f.approver)
	if len(got) != 1 {
		t.Fatalf("unconfigured listing = %v, want only the caller's own request", got)
	}

	if _, ok := got[mine.UID.String()]; !ok {
		t.Errorf("unconfigured listing = %v, missing the caller's own request", got)
	}

	// Delegated: the pending request for their server joins the list.
	f.setServerApprovers(t, store.ApproverKindAccess, f.group.UID)

	got = f.listRequestsAs(t, f.approver)

	if _, ok := got[mine.UID.String()]; !ok {
		t.Errorf("approver listing = %v, missing the caller's own request", got)
	}

	if role, ok := got[pendingHere.UID.String()]; !ok {
		t.Errorf("approver listing = %v, missing the pending request for their server", got)
	} else if role != ApproverHatServer {
		t.Errorf("approver_role on the decidable row = %q, want %q", role, ApproverHatServer)
	}

	if _, ok := got[decidedHere.UID.String()]; ok {
		t.Errorf("approver listing = %v, leaked a *decided* request for their server", got)
	}

	if _, ok := got[pendingElsewhere.UID.String()]; ok {
		t.Errorf("approver listing = %v, leaked a request for a server they do not approve", got)
	}

	// Somebody in no approver group still sees nothing but their own — and
	// they filed none, so nothing at all.
	if got := f.listRequestsAs(t, f.outsider); len(got) != 0 {
		t.Errorf("outsider listing = %v, want empty", got)
	}

	// The row the caller filed themselves carries no hat: nobody self-approves.
	if role := f.listRequestsAs(t, f.approver)[mine.UID.String()]; role != ApproverHatNone {
		t.Errorf("approver_role on the caller's own request = %q, want empty", role)
	}

	// An admin still sees everything, unfiltered.
	if got := f.listRequestsAs(t, f.admin); len(got) != 4 {
		t.Errorf("admin listing = %v, want all four requests", got)
	}
}

// TestSlackMayDecideRequest exercises the *real* slackDecider gate, not the
// stub the Slack transport tests use. Both Slack paths — the inbound endpoint
// and Socket Mode — reach processSlackDecision, which authorizes solely through
// this method, so "a delegated approver clicks Approve in Slack" hinges
// entirely on it.
func TestSlackMayDecideRequest(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "slack-gate")

	req := f.newGrantRequest(t, "slack-gate-1", f.requester)
	own := f.newGrantRequest(t, "slack-gate-2", f.approver)

	// Nothing delegated yet: admins only, exactly as before the feature.
	if f.server.mayDecideRequest(f.ctx, f.approver, req.UID) {
		t.Error("mayDecideRequest(unconfigured approver) = true, want false")
	}

	if !f.server.mayDecideRequest(f.ctx, f.admin, req.UID) {
		t.Error("mayDecideRequest(admin) = false, want true")
	}

	f.setServerApprovers(t, store.ApproverKindAccess, f.group.UID)

	if !f.server.mayDecideRequest(f.ctx, f.approver, req.UID) {
		t.Error("mayDecideRequest(named access approver) = false, want true")
	}

	// The security invariant, over Slack too: never your own request.
	if f.server.mayDecideRequest(f.ctx, f.approver, own.UID) {
		t.Error("mayDecideRequest(own request) = true, want false")
	}

	// Somebody in no approver group.
	if f.server.mayDecideRequest(f.ctx, f.outsider, req.UID) {
		t.Error("mayDecideRequest(outsider) = true, want false")
	}

	// No hierarchy: a query approver has no say over a grant request.
	f.setServerApprovers(t, store.ApproverKindAccess)
	f.setServerApprovers(t, store.ApproverKindQuery, f.group.UID)

	if f.server.mayDecideRequest(f.ctx, f.approver, req.UID) {
		t.Error("mayDecideRequest(query approver) = true, want false")
	}

	// A request that no longer exists lets an admin through — so the decision
	// that follows reports "no longer exists" rather than a misleading refusal
	// — and stops everybody else.
	missing := uuid.New()

	if !f.server.mayDecideRequest(f.ctx, f.admin, missing) {
		t.Error("mayDecideRequest(admin, missing) = false, want true")
	}

	if f.server.mayDecideRequest(f.ctx, f.approver, missing) {
		t.Error("mayDecideRequest(non-admin, missing) = true, want false")
	}
}

// TestQueryHold_DefinitionApproversWinOverServerChain pins the precedence: a
// non-empty approver list on the grant definition is an explicit policy choice,
// so the server's query approvers never get a say under it.
func TestQueryHold_DefinitionApproversWinOverServerChain(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "qh-prec")

	// The server names the ops group as its query approvers…
	f.setServerApprovers(t, store.ApproverKindQuery, f.group.UID)

	// …but the definition the session's grant came from names a different one.
	defGroup, err := f.dataStore.CreateUserGroup(f.ctx, &store.UserGroup{Name: "def-approvers-qh-prec"})
	if err != nil {
		t.Fatalf("create user group: %v", err)
	}

	if err := f.dataStore.AddUserToUserGroup(f.ctx, defGroup.UID, f.outsider.UID); err != nil {
		t.Fatalf("add user to group: %v", err)
	}

	def, err := f.dataStore.CreateGrantDefinition(f.ctx, &store.GrantDefinition{
		Name:                  "qh-prec-def",
		Slug:                  "qh-prec-def",
		DurationSeconds:       3600,
		ApprovalPatterns:      []string{`(?i)^DELETE\s+FROM`},
		ApproverUserGroupUIDs: []uuid.UUID{defGroup.UID},
		CreatedBy:             f.admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition(): %v", err)
	}

	grant, err := f.dataStore.CreateGrant(f.ctx, &store.Grant{
		UserID:            f.requester.UID,
		DatabaseID:        f.target.UID,
		GrantDefinitionID: def.UID,
		GrantedBy:         f.admin.UID,
		StartsAt:          time.Now().Add(-time.Minute),
		ExpiresAt:         time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant(): %v", err)
	}

	conn, err := f.dataStore.CreateConnection(
		f.ctx, f.requester.UID, f.target.UID, "127.0.0.1", store.WithGrantUID(grant.UID),
	)
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	held, err := f.dataStore.CreatePendingQuery(f.ctx, &store.Query{
		ConnectionID: conn.UID,
		SQLText:      "DELETE FROM users",
		ExecutedAt:   time.Now(),
	}, `(?i)^DELETE\s+FROM`)
	if err != nil {
		t.Fatalf("create pending query: %v", err)
	}

	// The server's query approver is shut out — the definition names somebody.
	if w := f.resolveHold(t, f.approver, held); w.Code != http.StatusForbidden {
		t.Fatalf("server approver under a definition-scoped hold got %d, want 403", w.Code)
	}

	full := mustQueryWithOwner(t, f, held)
	if hat := f.server.approverHatForQuery(f.ctx, f.approver, full); hat != ApproverHatNone {
		t.Errorf("server approver hat = %q, want empty", hat)
	}

	if hat := f.server.approverHatForQuery(f.ctx, f.outsider, full); hat != ApproverHatDefinition {
		t.Errorf("definition approver hat = %q, want %q", hat, ApproverHatDefinition)
	}

	if w := f.resolveHold(t, f.outsider, held); w.Code != http.StatusOK {
		t.Fatalf("definition approver got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// listPendingAs runs the pending-approvals listing as one user and returns the
// query uids it answered with, each mapped to its reported approver_role.
func (f *serverApproverFixture) listPendingAs(t *testing.T, user *store.User) map[string]string {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/queries/pending", nil)
	c.Set(contextKeyUser, user)

	f.server.handleListPendingApprovals(c)

	if w.Code != http.StatusOK {
		t.Fatalf("pending listing got %d, want 200: %s", w.Code, w.Body.String())
	}

	var body struct {
		Queries []struct {
			UID          string `json:"uid"`
			ApproverRole string `json:"approver_role"`
		} `json:"queries"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode pending listing: %v (%s)", err, w.Body.String())
	}

	out := make(map[string]string, len(body.Queries))
	for _, q := range body.Queries {
		out[q.UID] = q.ApproverRole
	}

	return out
}

// perRowPending is what the pending listing would answer if it still walked the
// chain one row at a time: it calls mayViewQuery/approverHatForQuery — the
// unbatched wrappers, which resolve a single server each — over the same set.
//
// It is the oracle TestListPendingApprovals_BatchedMatchesPerRow compares
// against, so a batched listing that ever disagreed with the per-row chain
// would be a failure and not a silently different authorization answer.
func (f *serverApproverFixture) perRowPending(t *testing.T, user *store.User) map[string]string {
	t.Helper()

	pending, err := f.dataStore.ListPendingApprovalQueries(f.ctx)
	if err != nil {
		t.Fatalf("ListPendingApprovalQueries(): %v", err)
	}

	out := make(map[string]string, len(pending))

	for i := range pending {
		if !f.server.mayViewQuery(f.ctx, user, &pending[i]) {
			continue
		}

		out[pending[i].UID.String()] = f.server.approverHatForQuery(f.ctx, user, &pending[i])
	}

	return out
}

// TestListPendingApprovals_BatchedMatchesPerRow is the safety half of batching
// the approver resolution: the listing now resolves every distinct database
// once and decides the whole page against that map, and it must answer exactly
// what the per-row chain answers — same visibility, same hats, for every role.
//
// The page deliberately spans three servers configured differently (server-level
// approvers, server-group fallback, nothing at all) plus the viewer's own held
// statement, because a single-server page could not catch a batched map keyed
// or scoped wrongly.
func TestListPendingApprovals_BatchedMatchesPerRow(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "pending-batch")

	grouped := f.newTargetServer(t, "grouped-pending-batch")
	bare := f.newTargetServer(t, "bare-pending-batch")

	// Level 1: the target names the ops group directly.
	f.setServerApprovers(t, store.ApproverKindQuery, f.group.UID)

	// Level 2: a second server inherits the same group through a server group.
	sg, err := f.dataStore.CreateServerGroup(f.ctx, &store.ServerGroup{
		Name:                       "sg-pending-batch",
		QueryApproverUserGroupUIDs: []uuid.UUID{f.group.UID},
	})
	if err != nil {
		t.Fatalf("CreateServerGroup(): %v", err)
	}

	if err := f.dataStore.AddServerToGroup(f.ctx, sg.UID, grouped.UID); err != nil {
		t.Fatalf("AddServerToGroup(): %v", err)
	}

	// Level 3: `bare` configures nothing, so it stays admin-only.
	onTarget := f.queryHoldOn(t, f.requester, f.target.UID)
	onGrouped := f.queryHoldOn(t, f.requester, grouped.UID)
	onBare := f.queryHoldOn(t, f.requester, bare.UID)
	ownHold := f.queryHoldOn(t, f.approver, f.target.UID)

	got := f.listPendingAs(t, f.approver)

	if role, ok := got[onTarget.UID.String()]; !ok {
		t.Errorf("approver pending listing = %v, missing the hold on their own server", got)
	} else if role != ApproverHatServer {
		t.Errorf("hat on the server-level hold = %q, want %q", role, ApproverHatServer)
	}

	if role, ok := got[onGrouped.UID.String()]; !ok {
		t.Errorf("approver pending listing = %v, missing the hold reached through a server group", got)
	} else if role != ApproverHatServer {
		t.Errorf("hat on the group-level hold = %q, want %q", role, ApproverHatServer)
	}

	if _, ok := got[onBare.UID.String()]; ok {
		t.Errorf("approver pending listing = %v, leaked a hold on a server they do not approve", got)
	}

	// Self-approval survives batching: the viewer sees their own held statement
	// (they may watch it) and wears no hat over it.
	if role, ok := got[ownHold.UID.String()]; !ok {
		t.Errorf("approver pending listing = %v, missing their own held statement", got)
	} else if role != ApproverHatNone {
		t.Errorf("hat on their own hold = %q, want empty", role)
	}

	// Every role, against the per-row chain: same rows, same hats.
	for _, user := range []*store.User{f.admin, f.approver, f.outsider, f.requester} {
		batched := f.listPendingAs(t, user)

		perRow := f.perRowPending(t, user)
		if len(batched) != len(perRow) {
			t.Fatalf("batched listing for %s = %v, per-row chain = %v", user.Username, batched, perRow)
		}

		for uid, hat := range perRow {
			if batched[uid] != hat {
				t.Errorf("hat for %s on %s: batched = %q, per-row = %q", user.Username, uid, batched[uid], hat)
			}
		}
	}

	// The admin fallback, spelled out: with nothing configured anywhere an admin
	// still decides every row, and a user in no group decides none.
	if admin := f.listPendingAs(t, f.admin); len(admin) != 4 {
		t.Errorf("admin pending listing = %v, want all four holds", admin)
	} else {
		for uid, role := range admin {
			if role != ApproverHatAdmin {
				t.Errorf("admin hat on %s = %q, want %q", uid, role, ApproverHatAdmin)
			}
		}
	}

	if outsider := f.listPendingAs(t, f.outsider); len(outsider) != 0 {
		t.Errorf("outsider pending listing = %v, want empty", outsider)
	}
}

// TestListGrantRequests_BatchedMatchesPerRow is the grant-request half of the
// same guarantee, across a page spanning several servers.
func TestListGrantRequests_BatchedMatchesPerRow(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "gr-batch")

	grouped := f.newTargetServer(t, "grouped-gr-batch")
	bare := f.newTargetServer(t, "bare-gr-batch")

	f.setServerApprovers(t, store.ApproverKindAccess, f.group.UID)

	sg, err := f.dataStore.CreateServerGroup(f.ctx, &store.ServerGroup{
		Name:                        "sg-gr-batch",
		AccessApproverUserGroupUIDs: []uuid.UUID{f.group.UID},
	})
	if err != nil {
		t.Fatalf("CreateServerGroup(): %v", err)
	}

	if err := f.dataStore.AddServerToGroup(f.ctx, sg.UID, grouped.UID); err != nil {
		t.Fatalf("AddServerToGroup(): %v", err)
	}

	onTarget := f.newGrantRequestFor(t, "gr-batch-target", f.requester, f.target.UID)
	onGrouped := f.newGrantRequestFor(t, "gr-batch-grouped", f.requester, grouped.UID)
	onBare := f.newGrantRequestFor(t, "gr-batch-bare", f.requester, bare.UID)
	ownRequest := f.newGrantRequestFor(t, "gr-batch-own", f.approver, f.target.UID)

	got := f.listRequestsAs(t, f.approver)

	if role := got[onTarget.UID.String()]; role != ApproverHatServer {
		t.Errorf("hat on the server-level request = %q, want %q", role, ApproverHatServer)
	}

	if role := got[onGrouped.UID.String()]; role != ApproverHatServer {
		t.Errorf("hat on the group-level request = %q, want %q", role, ApproverHatServer)
	}

	if _, ok := got[onBare.UID.String()]; ok {
		t.Errorf("approver listing = %v, leaked a request for a server they do not approve", got)
	}

	// Their own request is listed (it always was) and carries no hat.
	if role, ok := got[ownRequest.UID.String()]; !ok {
		t.Errorf("approver listing = %v, missing their own request", got)
	} else if role != ApproverHatNone {
		t.Errorf("hat on their own request = %q, want empty", role)
	}

	// Same rows and hats as deciding each row on its own would give.
	pendingStatus := store.GrantRequestPending

	pending, err := f.dataStore.ListGrantRequests(f.ctx, store.GrantRequestFilter{Status: &pendingStatus})
	if err != nil {
		t.Fatalf("ListGrantRequests(): %v", err)
	}

	for _, user := range []*store.User{f.admin, f.approver, f.outsider, f.requester} {
		batched := f.listRequestsAs(t, user)

		for i := range pending {
			uid := pending[i].UID.String()

			mayDecide := f.server.mayDecideGrantRequest(f.ctx, user, &pending[i])
			hat := f.server.approverHatForRequest(f.ctx, user, &pending[i])

			role, listed := batched[uid]
			if mayDecide && !listed {
				t.Errorf("%s may decide %s per-row but the listing omitted it", user.Username, uid)
			}

			if listed && role != hat {
				t.Errorf("hat for %s on %s: batched = %q, per-row = %q", user.Username, uid, role, hat)
			}
		}
	}
}

// newTargetServersInGroup creates n fresh database targets, all held by one new
// server group that names `approvers` for `kind`. Each server is a distinct
// database_id, which is what makes a per-row resolver's cost visible.
func (f *serverApproverFixture) newTargetServersInGroup(
	t *testing.T, prefix string, n int, kind store.ApproverKind, approvers ...uuid.UUID,
) []*store.Server {
	t.Helper()

	sg := &store.ServerGroup{Name: "sg-" + prefix}
	if kind == store.ApproverKindAccess {
		sg.AccessApproverUserGroupUIDs = approvers
	} else {
		sg.QueryApproverUserGroupUIDs = approvers
	}

	created, err := f.dataStore.CreateServerGroup(f.ctx, sg)
	if err != nil {
		t.Fatalf("CreateServerGroup(): %v", err)
	}

	servers := make([]*store.Server, 0, n)

	for i := range n {
		srv := f.newTargetServer(t, fmt.Sprintf("%s-%d", prefix, i))

		if err := f.dataStore.AddServerToGroup(f.ctx, created.UID, srv.UID); err != nil {
			t.Fatalf("AddServerToGroup(): %v", err)
		}

		servers = append(servers, srv)
	}

	return servers
}

// approverResolutionQueryHook counts the queries the approver chain itself
// issues. Both of them — the server row read and the server-group join — name
// `access_approver_user_group_uids`, and nothing else on either listing path
// does, so this counts the resolver's round trips and only those.
//
// It is attached to the store the handlers actually use, the way
// TestListGrantDefinitions_ScopedDatabaseUIDs pins its own batching.
func (f *serverApproverFixture) approverResolutionQueryHook() *apiQueryCountHook {
	hook := &apiQueryCountHook{substring: "access_approver_user_group_uids"}
	f.dataStore.DB().AddQueryHook(hook)

	return hook
}

// approverResolutionQueries is the count both listings must hold flat: one read
// of the server rows plus one of their server groups, per listing, whatever the
// page size. It is derived from ResolveServerApproverGroupsByServers' two
// queries — the same contract TestResolveServerApproverGroupsByServers pins at
// the store level.
const approverResolutionQueries = 2

// TestListPendingApprovals_ApproverResolutionIsFlat is the point of batching,
// asserted where the regression would actually happen: in the handler.
//
// The store-level count test proves the batched resolver is batched; it cannot
// notice a handler that goes back to calling it once per row. So this runs the
// *same* listing at two page sizes — 2 holds on 2 databases, then 6 on 6 — and
// requires the approver-resolution query count to be identical. The assertion
// is about the shape being flat, not about a magic number; the absolute value
// is checked too, with its provenance in approverResolutionQueries.
func TestListPendingApprovals_ApproverResolutionIsFlat(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "pending-flat")

	servers := f.newTargetServersInGroup(t, "pending-flat", 6, store.ApproverKindQuery, f.group.UID)

	// A small page first.
	for _, srv := range servers[:2] {
		f.queryHoldOn(t, f.requester, srv.UID)
	}

	hook := f.approverResolutionQueryHook()

	small := f.listPendingAs(t, f.approver)
	if len(small) != 2 {
		t.Fatalf("small pending listing = %v, want 2 holds", small)
	}

	smallCount := hook.count.Load()

	if smallCount != approverResolutionQueries {
		t.Errorf("2-row pending listing took %d approver queries, want %d",
			smallCount, approverResolutionQueries)
	}

	// Three times the rows, on three times as many distinct databases.
	for _, srv := range servers[2:] {
		f.queryHoldOn(t, f.requester, srv.UID)
	}

	hook.count.Store(0)

	large := f.listPendingAs(t, f.approver)
	if len(large) != 6 {
		t.Fatalf("large pending listing = %v, want 6 holds", large)
	}

	largeCount := hook.count.Load()

	if largeCount != smallCount {
		t.Errorf("approver resolution grew with the page: %d queries for 2 rows, %d for 6 — it must be flat",
			smallCount, largeCount)
	}
}

// TestListGrantRequests_ApproverResolutionIsFlat is the grant-request half of
// the same handler-level guarantee, on the listing the spec was written about.
func TestListGrantRequests_ApproverResolutionIsFlat(t *testing.T) {
	t.Parallel()

	f := newServerApproverFixture(t, "gr-flat")

	servers := f.newTargetServersInGroup(t, "gr-flat", 6, store.ApproverKindAccess, f.group.UID)

	for i, srv := range servers[:2] {
		f.newGrantRequestFor(t, fmt.Sprintf("gr-flat-small-%d", i), f.requester, srv.UID)
	}

	hook := f.approverResolutionQueryHook()

	small := f.listRequestsAs(t, f.approver)
	if len(small) != 2 {
		t.Fatalf("small request listing = %v, want 2 requests", small)
	}

	smallCount := hook.count.Load()

	if smallCount != approverResolutionQueries {
		t.Errorf("2-row grant-request listing took %d approver queries, want %d",
			smallCount, approverResolutionQueries)
	}

	for i, srv := range servers[2:] {
		f.newGrantRequestFor(t, fmt.Sprintf("gr-flat-large-%d", i), f.requester, srv.UID)
	}

	hook.count.Store(0)

	large := f.listRequestsAs(t, f.approver)
	if len(large) != 6 {
		t.Fatalf("large request listing = %v, want 6 requests", large)
	}

	largeCount := hook.count.Load()

	if largeCount != smallCount {
		t.Errorf("approver resolution grew with the page: %d queries for 2 rows, %d for 6 — it must be flat",
			smallCount, largeCount)
	}
}
