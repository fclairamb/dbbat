package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// crossGrantFixture stands up the shape of the leak this file exists to
// prevent: two unrelated grants, each with its own approver group, and a
// statement held on each.
//
// approverA is an approver on grant A and nothing else. That is the whole
// point — being an approver *somewhere* is enough to subscribe to
// approvals/pending, so the topic gate lets them in the door and only the
// per-event rule stops them reading grant B's SQL.
type crossGrantFixture struct {
	server *Server
	data   *store.Store

	admin     *store.User
	viewer    *store.User
	approverA *store.User

	connA  *store.Connection
	queryA *store.Query
	connB  *store.Connection
	queryB *store.Query
}

func newCrossGrantFixture(t *testing.T) *crossGrantFixture {
	t.Helper()

	server, data := setupTestServer(t)
	ctx := context.Background()

	mkUser := func(name string, roles ...string) *store.User {
		t.Helper()

		u, err := data.CreateUser(ctx, name, "x", roles)
		if err != nil {
			t.Fatalf("create user %s: %v", name, err)
		}

		return u
	}

	mkGroup := func(name string) *store.UserGroup {
		t.Helper()

		g, err := data.CreateUserGroup(ctx, &store.UserGroup{Name: name})
		if err != nil {
			t.Fatalf("create group %s: %v", name, err)
		}

		return g
	}

	mkServer := func(name string) *store.Server {
		t.Helper()

		srv, err := data.CreateServer(ctx, &store.Server{
			Name:         name,
			Host:         "127.0.0.1",
			Port:         5432,
			DatabaseName: name,
			Username:     "pg",
			Password:     "secret",
			Protocol:     store.ProtocolPostgreSQL,
			SSLMode:      "disable",
		}, approvalTestKey)
		if err != nil {
			t.Fatalf("create server %s: %v", name, err)
		}

		return srv
	}

	admin := mkUser("cg-admin", store.RoleAdmin)
	viewer := mkUser("cg-viewer", store.RoleViewer)
	approverA := mkUser("cg-approver-a", store.RoleConnector)
	ownerA := mkUser("cg-owner-a", store.RoleConnector)
	ownerB := mkUser("cg-owner-b", store.RoleConnector)

	groupA := mkGroup("cg-group-a")
	groupB := mkGroup("cg-group-b")

	if err := data.AddUserToGroup(ctx, groupA.UID, approverA.UID); err != nil {
		t.Fatalf("add approver to group A: %v", err)
	}

	dbA := mkServer("cg-db-a")
	dbB := mkServer("cg-db-b")

	mkGrant := func(owner *store.User, target *store.Server, group *store.UserGroup) {
		t.Helper()

		if _, err := data.CreateGrant(ctx, &store.Grant{
			UserID:            owner.UID,
			DatabaseID:        target.UID,
			GrantedBy:         admin.UID,
			StartsAt:          time.Now().Add(-time.Hour),
			ExpiresAt:         time.Now().Add(time.Hour),
			ApprovalPatterns:  []string{`(?i)^DELETE\s+FROM`},
			ApproverGroupUIDs: []uuid.UUID{group.UID},
		}); err != nil {
			t.Fatalf("create grant: %v", err)
		}
	}

	mkGrant(ownerA, dbA, groupA)
	mkGrant(ownerB, dbB, groupB)

	mkHeld := func(owner *store.User, target *store.Server, sql string) (*store.Connection, *store.Query) {
		t.Helper()

		conn, err := data.CreateConnection(ctx, owner.UID, target.UID, "127.0.0.1")
		if err != nil {
			t.Fatalf("create connection: %v", err)
		}

		query, err := data.CreatePendingQuery(ctx, &store.Query{
			ConnectionID: conn.UID,
			SQLText:      sql,
			ExecutedAt:   time.Now(),
		}, `(?i)^DELETE\s+FROM`)
		if err != nil {
			t.Fatalf("create pending query: %v", err)
		}

		return conn, query
	}

	connA, queryA := mkHeld(ownerA, dbA, "DELETE FROM invoices")
	connB, queryB := mkHeld(ownerB, dbB, "DELETE FROM salaries WHERE ssn = '123-45-6789'")

	return &crossGrantFixture{
		server: server, data: data,
		admin: admin, viewer: viewer, approverA: approverA,
		connA: connA, queryA: queryA, connB: connB, queryB: queryB,
	}
}

// pendingEvent renders the payload publishPending puts on the wire.
func pendingEvent(conn *store.Connection, query *store.Query) map[string]any {
	return map[string]any{
		"query_uid":         query.UID.String(),
		"connection_uid":    conn.UID.String(),
		"sql_text":          query.SQLText,
		"executed_at":       query.ExecutedAt,
		"approval_required": true,
		"approval_status":   store.ApprovalPending,
	}
}

// authorizedSQL runs publish (delivery into every subscriber's buffer is
// synchronous), then drains this subscriber exactly the way flushStream does,
// returning the sql_text of everything that survived the send-time
// authorization check.
func authorizedSQL(t *testing.T, sub *events.Subscriber, publish func()) []string {
	t.Helper()

	publish()

	got := []string{}

	drain := func(ev events.Event) {
		if sub.Authorized(ev) {
			sql, _ := ev.Data["sql_text"].(string)
			got = append(got, sql)
		}
	}

	for {
		select {
		case ev := <-sub.Events():
			drain(ev)

			continue
		default:
		}

		break
	}

	for _, ev := range sub.TakePriority() {
		drain(ev)
	}

	return got
}

// subscribeAs attaches a subscriber carrying the production authorizer for the
// given user, subscribed to approvals/pending.
func (f *crossGrantFixture) subscribeAs(t *testing.T, broker *events.Broker, user *store.User) *events.Subscriber {
	t.Helper()

	auth := &streamAuthorizer{server: f.server, user: user, cache: newAuthDecisionCache()}

	sub := broker.Subscribe(auth.authorizer(context.Background()), 64)
	t.Cleanup(sub.Close)

	if !sub.Subscribe(events.TopicApprovalsPending) {
		t.Fatalf("%s could not subscribe to approvals/pending", user.Username)
	}

	return sub
}

func TestApprovalsStreamDoesNotLeakOtherGrants(t *testing.T) {
	f := newCrossGrantFixture(t)
	ctx := context.Background()

	// The premise: being an approver on one grant opens the topic. That is
	// deliberate and unchanged — the containment is per event, not here.
	if !f.server.mayReadTopic(ctx, f.approverA, events.TopicApprovalsPending) {
		t.Fatal("an approver must still be able to subscribe to approvals/pending")
	}

	broker := events.New()

	approverSub := f.subscribeAs(t, broker, f.approverA)
	adminSub := f.subscribeAs(t, broker, f.admin)
	viewerSub := f.subscribeAs(t, broker, f.viewer)

	publish := func() {
		broker.Publish(events.TopicApprovalsPending, events.EventApprovalPending, pendingEvent(f.connA, f.queryA))
		broker.Publish(events.TopicApprovalsPending, events.EventApprovalPending, pendingEvent(f.connB, f.queryB))
	}

	adminGot := authorizedSQL(t, adminSub, publish)
	viewerGot := authorizedSQL(t, viewerSub, func() {})
	approverGot := authorizedSQL(t, approverSub, func() {})

	if len(adminGot) != 2 {
		t.Fatalf("admin saw %d of 2 held statements: %v", len(adminGot), adminGot)
	}

	if len(viewerGot) != 2 {
		t.Fatalf("viewer saw %d of 2 held statements: %v", len(viewerGot), viewerGot)
	}

	if len(approverGot) != 1 {
		t.Fatalf("approver of grant A saw %d statements, want only their own: %v", len(approverGot), approverGot)
	}

	if approverGot[0] != f.queryA.SQLText {
		t.Fatalf("approver of grant A received the wrong statement: %q", approverGot[0])
	}

	for _, sql := range approverGot {
		if sql == f.queryB.SQLText {
			t.Fatal("grant B's held SQL leaked to an approver with no relationship to it")
		}
	}
}

// TestApprovalsStreamFiltersCrossReplicaEvents covers the path the spec called
// out as "verify, do not assume": a hold parked on another replica arrives via
// LISTEN/NOTIFY and is republished locally, so it must meet the same filter.
func TestApprovalsStreamFiltersCrossReplicaEvents(t *testing.T) {
	f := newCrossGrantFixture(t)
	ctx := context.Background()

	broker := events.New()
	f.server.broker = broker

	approverSub := f.subscribeAs(t, broker, f.approverA)
	adminSub := f.subscribeAs(t, broker, f.admin)

	replicate := func() {
		f.server.republish(ctx, store.EventNotification{
			Topic:    events.TopicApprovalsPending,
			Type:     events.EventApprovalPending,
			QueryUID: f.queryB.UID,
			ConnUID:  f.connB.UID,
		})
	}

	adminGot := authorizedSQL(t, adminSub, replicate)
	approverGot := authorizedSQL(t, approverSub, func() {})

	if len(adminGot) != 1 || adminGot[0] != f.queryB.SQLText {
		t.Fatalf("the replica-forwarded hold never reached the admin: %v", adminGot)
	}

	if len(approverGot) != 0 {
		t.Fatalf("a replica-forwarded hold on grant B leaked to grant A's approver: %v", approverGot)
	}
}

// TestApprovalEventAuthorizationMatchesREST pins the stream's effective
// visibility to mayViewQuery's, row by row. If the REST rule ever changes, this
// fails rather than letting the two drift apart silently.
func TestApprovalEventAuthorizationMatchesREST(t *testing.T) {
	f := newCrossGrantFixture(t)
	ctx := context.Background()

	held, err := f.data.GetQueryWithOwner(ctx, f.queryB.UID)
	if err != nil {
		t.Fatalf("reload query B: %v", err)
	}

	ev := &events.Event{
		Topic: events.TopicApprovalsPending,
		Type:  events.EventApprovalPending,
		Data:  pendingEvent(f.connB, f.queryB),
	}

	for _, user := range []*store.User{f.admin, f.viewer, f.approverA} {
		auth := &streamAuthorizer{server: f.server, user: user, cache: newAuthDecisionCache()}

		stream := auth.mayReadApprovalEvent(ctx, ev)
		rest := f.server.mayViewQuery(ctx, user, held)

		if stream != rest {
			t.Fatalf("%s: stream visibility %v disagrees with REST %v", user.Username, stream, rest)
		}
	}
}

// TestApprovalEventAuthorizationIsMemoizedPerConnection checks the memo is
// keyed on the event's scope rather than the topic: a decision taken for one
// connection must not answer for another.
func TestApprovalEventAuthorizationIsMemoizedPerConnection(t *testing.T) {
	f := newCrossGrantFixture(t)
	ctx := context.Background()

	auth := &streamAuthorizer{server: f.server, user: f.approverA, cache: newAuthDecisionCache()}

	evA := &events.Event{Topic: events.TopicApprovalsPending, Data: pendingEvent(f.connA, f.queryA)}
	evB := &events.Event{Topic: events.TopicApprovalsPending, Data: pendingEvent(f.connB, f.queryB)}

	// Warm the allow decision first: a topic-keyed memo would then answer yes
	// for connection B too, which is precisely the leak.
	for range 3 {
		if !auth.mayReadApprovalEvent(ctx, evA) {
			t.Fatal("grant A's approver was denied grant A's own hold")
		}
	}

	if auth.mayReadApprovalEvent(ctx, evB) {
		t.Fatal("a cached allow for connection A answered for connection B")
	}

	if got := auth.cache.size(); got != 2 {
		t.Fatalf("memo holds %d entries, want one per connection", got)
	}
}
