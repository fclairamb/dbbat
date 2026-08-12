package api

import (
	"context"
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

	conn, err := f.dataStore.CreateConnection(f.ctx, runner.UID, f.target.UID, "127.0.0.1")
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
