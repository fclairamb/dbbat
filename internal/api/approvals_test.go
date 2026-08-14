package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// approvalTestKey is a fixed 32-byte AES key for the fixture's encrypted
// server credentials. Test-only; never used outside this file.
var approvalTestKey = []byte("01234567890123456789012345678901")

// approvalFixture is one held statement plus the cast around it: the user who
// ran it, an admin who may resolve it, and the connection it belongs to.
type approvalFixture struct {
	server    *Server
	dataStore *store.Store
	requester *store.User
	admin     *store.User
	query     *store.Query
}

func newApprovalFixture(t *testing.T) *approvalFixture {
	t.Helper()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	requester, err := dataStore.CreateUser(ctx, "requester", "x", []string{store.RoleConnector})
	if err != nil {
		t.Fatalf("create requester: %v", err)
	}

	// The requester is also an admin — that is the interesting case: an admin
	// must still not be able to approve their own statement.
	admin, err := dataStore.CreateUser(ctx, "approver", "x", []string{store.RoleAdmin})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	target, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "prod",
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

	conn, err := dataStore.CreateConnection(ctx, requester.UID, target.UID, "127.0.0.1")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	query, err := dataStore.CreatePendingQuery(ctx, &store.Query{
		ConnectionID: conn.UID,
		SQLText:      "DELETE FROM users",
		ExecutedAt:   time.Now(),
	}, `(?i)^DELETE\s+FROM`)
	if err != nil {
		t.Fatalf("create pending query: %v", err)
	}

	return &approvalFixture{
		server:    server,
		dataStore: dataStore,
		requester: requester,
		admin:     admin,
		query:     query,
	}
}

// call runs one approve/deny decision as the given user.
func (f *approvalFixture) call(t *testing.T, user *store.User, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	req := httptest.NewRequest(http.MethodPost, path, nil)
	if body != "" {
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}

	c.Request = req
	c.Params = gin.Params{{Key: "uid", Value: f.query.UID.String()}}
	c.Set(contextKeyUser, user)

	switch {
	case strings.HasSuffix(path, "/approve"):
		f.server.handleApproveQuery(c)
	case strings.HasSuffix(path, "/deny"):
		f.server.handleDenyQuery(c)
	default:
		t.Fatalf("unsupported path %s", path)
	}

	return w
}

func TestSelfApprovalIsRejectedEvenForAdmins(t *testing.T) {
	t.Parallel()

	f := newApprovalFixture(t)
	ctx := context.Background()

	// Promote the requester to admin: four eyes must still mean four eyes.
	if err := f.dataStore.UpdateUser(ctx, f.requester.UID, store.UserUpdate{
		Roles: []string{store.RoleAdmin},
	}); err != nil {
		t.Fatalf("promote requester: %v", err)
	}

	promoted, err := f.dataStore.GetUserByUID(ctx, f.requester.UID)
	if err != nil {
		t.Fatalf("reload requester: %v", err)
	}

	if !promoted.IsAdmin() {
		t.Fatal("fixture setup: requester is not admin")
	}

	w := f.call(t, promoted, "/api/v1/queries/x/approve", "")

	if w.Code != http.StatusForbidden {
		t.Fatalf("self-approval returned %d, want 403 — the control would be decorative", w.Code)
	}

	// And the row is untouched.
	reloaded, err := f.dataStore.GetQuery(ctx, f.query.UID)
	if err != nil {
		t.Fatalf("reload query: %v", err)
	}

	if reloaded.ApprovalStatus == nil || *reloaded.ApprovalStatus != store.ApprovalPending {
		t.Fatalf("query left in state %v after a refused self-approval", reloaded.ApprovalStatus)
	}
}

func TestAdminApprovesSomebodyElsesQuery(t *testing.T) {
	t.Parallel()

	f := newApprovalFixture(t)
	ctx := context.Background()

	w := f.call(t, f.admin, "/api/v1/queries/x/approve", "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve returned %d: %s", w.Code, w.Body.String())
	}

	reloaded, err := f.dataStore.GetQuery(ctx, f.query.UID)
	if err != nil {
		t.Fatalf("reload query: %v", err)
	}

	if reloaded.ApprovalStatus == nil || *reloaded.ApprovalStatus != store.ApprovalApproved {
		t.Fatalf("status = %v, want approved", reloaded.ApprovalStatus)
	}

	if reloaded.ResolvedBy == nil || *reloaded.ResolvedBy != f.admin.UID {
		t.Fatal("approver not persisted — nobody could tell who unblocked it")
	}

	if reloaded.ResolvedAt == nil {
		t.Fatal("resolved_at not persisted")
	}

	// The decision is auditable.
	events, err := f.dataStore.ListAuditEvents(ctx, store.AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	found := false

	for _, ev := range events {
		if ev.EventType == "query.approval.approved" {
			found = true

			var details map[string]any
			_ = json.Unmarshal(ev.Details, &details)

			if details["query_uid"] != f.query.UID.String() {
				t.Fatalf("audit event names the wrong query: %v", details["query_uid"])
			}
		}
	}

	if !found {
		t.Fatal("no audit event written for the approval")
	}
}

func TestDenyRecordsTheReason(t *testing.T) {
	t.Parallel()

	f := newApprovalFixture(t)
	ctx := context.Background()

	w := f.call(t, f.admin, "/api/v1/queries/x/deny", `{"reason":"prod freeze"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("deny returned %d: %s", w.Code, w.Body.String())
	}

	reloaded, err := f.dataStore.GetQuery(ctx, f.query.UID)
	if err != nil {
		t.Fatalf("reload query: %v", err)
	}

	if reloaded.ApprovalStatus == nil || *reloaded.ApprovalStatus != store.ApprovalDenied {
		t.Fatalf("status = %v, want denied", reloaded.ApprovalStatus)
	}

	if reloaded.ResolutionReason == nil || *reloaded.ResolutionReason != "prod freeze" {
		t.Fatalf("reason = %v", reloaded.ResolutionReason)
	}
}

func TestSecondDecisionConflicts(t *testing.T) {
	t.Parallel()

	f := newApprovalFixture(t)

	if w := f.call(t, f.admin, "/api/v1/queries/x/approve", ""); w.Code != http.StatusOK {
		t.Fatalf("first approve returned %d", w.Code)
	}

	w := f.call(t, f.admin, "/api/v1/queries/x/deny", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("second decision returned %d, want 409 — a resolved hold must not be re-resolved", w.Code)
	}
}

func TestNonApproverIsRefused(t *testing.T) {
	t.Parallel()

	f := newApprovalFixture(t)
	ctx := context.Background()

	// A plain connector, unrelated to the query and in no approver group.
	stranger, err := f.dataStore.CreateUser(ctx, "stranger", "x", []string{store.RoleConnector})
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}

	w := f.call(t, stranger, "/api/v1/queries/x/approve", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("stranger approve returned %d, want 403", w.Code)
	}
}

// TestApprovalUsesStampedGrantNotNewerOverlappingGrant covers the approval
// half of spec 2026-08-06-03: a query held under grant A's approval_patterns
// must stay resolvable by grant A's approver group even after a newer,
// higher-priority grant B is created for the same user/database — the exact
// situation where the pre-stamp code (re-resolving GetActiveGrant at approval
// time) picked B's approver_user_group_uids instead of A's.
func TestApprovalUsesStampedGrantNotNewerOverlappingGrant(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	requester, err := dataStore.CreateUser(ctx, "stamprequester", "x", []string{store.RoleConnector})
	if err != nil {
		t.Fatalf("create requester: %v", err)
	}

	admin, err := dataStore.CreateUser(ctx, "stampadmin", "x", []string{store.RoleAdmin})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	groupA, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "approvers-a"})
	if err != nil {
		t.Fatalf("create group A: %v", err)
	}
	groupB, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "approvers-b"})
	if err != nil {
		t.Fatalf("create group B: %v", err)
	}

	memberA, err := dataStore.CreateUser(ctx, "member-a", "x", []string{store.RoleConnector})
	if err != nil {
		t.Fatalf("create member A: %v", err)
	}
	if err := dataStore.AddUserToUserGroup(ctx, groupA.UID, memberA.UID); err != nil {
		t.Fatalf("add member A: %v", err)
	}

	memberB, err := dataStore.CreateUser(ctx, "member-b", "x", []string{store.RoleConnector})
	if err != nil {
		t.Fatalf("create member B: %v", err)
	}
	if err := dataStore.AddUserToUserGroup(ctx, groupB.UID, memberB.UID); err != nil {
		t.Fatalf("add member B: %v", err)
	}

	target, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "stampprod",
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

	now := time.Now()

	// Grant A: what the connection is actually stamped with, approver group A.
	grantA := persistGrantWithShape(t, dataStore, store.GrantDefinition{
		Controls:              []string{},
		ApprovalPatterns:      []string{`(?i)^DELETE\s+FROM`},
		ApproverUserGroupUIDs: []uuid.UUID{groupA.UID},
	}, requester.UID, target.UID, admin.UID, now.Add(-time.Hour), now.Add(time.Hour), 0)

	// Grant B: created afterwards, higher priority, overlapping window,
	// approver group B — exactly the grant GetActiveGrant now picks.
	persistGrantWithShape(t, dataStore, store.GrantDefinition{
		Controls:              []string{},
		ApprovalPatterns:      []string{`(?i)^DELETE\s+FROM`},
		ApproverUserGroupUIDs: []uuid.UUID{groupB.UID},
	}, requester.UID, target.UID, admin.UID, now.Add(-time.Hour), now.Add(2*time.Hour), store.PriorityFullWrite+10)

	active, err := dataStore.GetActiveGrant(ctx, requester.UID, target.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant() error = %v", err)
	}
	if active.UID == grantA.UID {
		t.Fatal("fixture setup: GetActiveGrant must pick grant B (the higher-priority one), not A")
	}

	// The connection is stamped with grant A — the grant it actually
	// authenticated under — not grant B.
	conn, err := dataStore.CreateConnection(ctx, requester.UID, target.UID, "127.0.0.1", store.WithGrantUID(grantA.UID))
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	query, err := dataStore.CreatePendingQuery(ctx, &store.Query{
		ConnectionID: conn.UID,
		SQLText:      "DELETE FROM users",
		ExecutedAt:   time.Now(),
	}, `(?i)^DELETE\s+FROM`)
	if err != nil {
		t.Fatalf("create pending query: %v", err)
	}

	f := &approvalFixture{server: server, dataStore: dataStore, requester: requester, admin: admin, query: query}

	if w := f.call(t, memberB, "/api/v1/queries/x/approve", ""); w.Code != http.StatusForbidden {
		t.Fatalf("group B member approve returned %d, want 403 (their group has nothing to do with this hold)", w.Code)
	}

	if w := f.call(t, memberA, "/api/v1/queries/x/approve", ""); w.Code != http.StatusOK {
		t.Fatalf("group A member approve returned %d, want 200: %s", w.Code, w.Body.String())
	}

	reloaded, err := dataStore.GetQuery(ctx, query.UID)
	if err != nil {
		t.Fatalf("reload query: %v", err)
	}
	if reloaded.ApprovalStatus == nil || *reloaded.ApprovalStatus != store.ApprovalApproved {
		t.Fatalf("status = %v, want approved", reloaded.ApprovalStatus)
	}
}

func TestPendingListIsBackedByThePartialIndex(t *testing.T) {
	t.Parallel()

	f := newApprovalFixture(t)
	ctx := context.Background()

	pending, err := f.dataStore.ListPendingApprovalQueries(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}

	if len(pending) != 1 || pending[0].UID != f.query.UID {
		t.Fatalf("pending = %d rows", len(pending))
	}

	// The pending row carries the connection's owner, which is what the
	// self-approval and approver-group checks need.
	if pending[0].UserID == nil || *pending[0].UserID != f.requester.UID {
		t.Fatal("pending row lost its owner")
	}

	if w := f.call(t, f.admin, "/api/v1/queries/x/approve", ""); w.Code != http.StatusOK {
		t.Fatalf("approve returned %d", w.Code)
	}

	after, err := f.dataStore.ListPendingApprovalQueries(ctx)
	if err != nil {
		t.Fatalf("list pending after: %v", err)
	}

	if len(after) != 0 {
		t.Fatalf("resolved hold still listed as pending: %d", len(after))
	}
}

func TestResolveQueryApprovalAsRejectsSelfApproval(t *testing.T) {
	t.Parallel()

	f := newApprovalFixture(t)
	ctx := context.Background()

	// The Slack path goes through here; "it came from Slack" is not an
	// authorization, so the same checks must apply.
	err := f.server.ResolveQueryApprovalAs(ctx, f.requester, f.query.UID, store.ApprovalApproved, "")
	if err == nil {
		t.Fatal("self-approval accepted through the Slack path")
	}

	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("got %v, want ErrSelfApproval", err)
	}
}

func TestResolveUnknownQueryFails(t *testing.T) {
	t.Parallel()

	f := newApprovalFixture(t)

	err := f.server.ResolveQueryApprovalAs(context.Background(), f.admin, uuid.New(), store.ApprovalApproved, "")
	if err == nil {
		t.Fatal("resolving a query that does not exist must fail")
	}
}
