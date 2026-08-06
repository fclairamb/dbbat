package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

func TestStreamAuthMiddlewarePromotesSubprotocol(t *testing.T) {
	t.Parallel()

	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "dbbat.auth.bearer.dbb_secret")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	s.streamAuthMiddleware()(c)

	if got := req.Header.Get("Authorization"); got != "Bearer dbb_secret" {
		t.Fatalf("Authorization = %q", got)
	}

	proto, ok := c.Get(contextKeyStreamSubprotocol)
	if !ok || proto != "dbbat.auth.bearer.dbb_secret" {
		t.Fatalf("subprotocol not recorded for echo: %v %v", proto, ok)
	}
}

func TestStreamAuthMiddlewareLeavesExistingHeader(t *testing.T) {
	t.Parallel()

	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer real")
	req.Header.Set("Sec-WebSocket-Protocol", "dbbat.auth.bearer.other")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	s.streamAuthMiddleware()(c)

	if got := req.Header.Get("Authorization"); got != "Bearer real" {
		t.Fatalf("existing header overwritten: %q", got)
	}
}

func TestMayReadTopicAuthorization(t *testing.T) {
	t.Parallel()

	s := &Server{}
	ctx := context.Background()

	admin := &store.User{UID: uuid.New(), Roles: []string{store.RoleAdmin}}
	viewer := &store.User{UID: uuid.New(), Roles: []string{store.RoleViewer}}
	connector := &store.User{UID: uuid.New(), Roles: []string{store.RoleConnector}}

	if !s.mayReadTopic(ctx, admin, events.TopicConnections) {
		t.Fatal("admin must read the connections topic")
	}

	if s.mayReadTopic(ctx, viewer, events.TopicConnections) {
		t.Fatal("connection lifecycle is admin-only")
	}

	if s.mayReadTopic(ctx, connector, events.TopicConnections) {
		t.Fatal("connector must not read the connections topic")
	}

	if !s.mayReadTopic(ctx, admin, events.TopicApprovalsPending) {
		t.Fatal("admin must read approvals/pending")
	}

	if !s.mayReadTopic(ctx, viewer, events.TopicApprovalsPending) {
		t.Fatal("viewers read every pending query over REST; the topic gate must agree")
	}

	if s.mayReadTopic(ctx, nil, events.TopicApprovalsPending) {
		t.Fatal("an unauthenticated subscriber must read nothing")
	}

	// Per-connection topics: admin/viewer always; anybody else needs the
	// store lookup, which is nil here — so it must fail closed rather than
	// panic or allow.
	topic := events.ConnectionQueriesTopic(uuid.New().String())

	if !s.mayReadTopic(ctx, admin, topic) || !s.mayReadTopic(ctx, viewer, topic) {
		t.Fatal("admin/viewer must be able to watch any connection")
	}

	if s.mayReadTopic(ctx, admin, "not-a-topic") {
		t.Fatal("unknown topics must be refused")
	}
}

func TestTopicAuthCacheMemoizesWithinTTL(t *testing.T) {
	t.Parallel()

	var calls int

	cache := newAuthDecisionCache()

	lookup := func() bool {
		calls++

		return true
	}

	for range 5 {
		if !cache.allowed("topic:connections", lookup) {
			t.Fatal("allowed = false")
		}
	}

	if calls != 1 {
		t.Fatalf("lookup ran %d times within the TTL, want 1 — every event would be a store round-trip", calls)
	}
}

func TestTopicAuthCacheRefreshesAfterTTL(t *testing.T) {
	t.Parallel()

	allow := true

	cache := newAuthDecisionCache()
	lookup := func() bool { return allow }

	if !cache.allowed("topic:connections", lookup) {
		t.Fatal("first lookup denied")
	}

	allow = false

	// Age the entry past the TTL: the re-check is the PII containment
	// guarantee, so a revoked reader must stop receiving within seconds.
	cache.mu.Lock()
	cache.entries["topic:connections"] = topicAuthEntry{allowed: true, at: time.Now().Add(-2 * topicAuthTTL)}
	cache.mu.Unlock()

	if cache.allowed("topic:connections", lookup) {
		t.Fatal("a stale entry outlived the TTL — a revoked reader would keep receiving")
	}
}

func TestTopicAuthCacheIsPerKey(t *testing.T) {
	t.Parallel()

	cache := newAuthDecisionCache()

	allowFor := func(key string) bool {
		return cache.allowed(key, func() bool { return key == "topic:connections" })
	}

	if !allowFor("topic:connections") {
		t.Fatal("allowed topic denied")
	}

	if allowFor("topic:approvals/pending") {
		t.Fatal("a decision leaked across topics")
	}

	// The per-event keys live in the same map; a connection the user may not
	// see must not inherit a topic-level yes.
	if allowFor("conn:" + uuid.New().String()) {
		t.Fatal("a decision leaked from a topic key onto a connection key")
	}
}

func TestTopicAuthCacheIsBounded(t *testing.T) {
	t.Parallel()

	cache := newAuthDecisionCache()

	// Denials are cached too, so an unbounded map is memory a client controls.
	for i := range maxTopicAuthEntries * 4 {
		cache.allowed(fmt.Sprintf("topic:connection/%d/queries", i), func() bool { return false })
	}

	if got := cache.size(); got > maxTopicAuthEntries {
		t.Fatalf("cache grew to %d entries, cap is %d", got, maxTopicAuthEntries)
	}
}

// streamReadLoopHarness stands up a real WebSocket wired to the production
// read loop, with no store behind it — everything under test here is string
// handling and bookkeeping.
func streamReadLoopHarness(t *testing.T, authorize events.Authorizer) (*websocket.Conn, *events.Subscriber) {
	t.Helper()

	srv := &Server{logger: slog.New(slog.DiscardHandler)}
	broker := events.New()
	sub := broker.Subscribe(authorize, 8)

	ready := make(chan struct{})

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := streamUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		close(ready)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		srv.streamReadLoop(ctx, cancel, conn, sub)
	}))

	client, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpSrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	<-ready

	t.Cleanup(func() {
		_ = client.Close()
		sub.Close()
		httpSrv.Close()
	})

	return client, sub
}

func TestSubscribeRejectsUnknownTopicsWithoutRemembering(t *testing.T) {
	t.Parallel()

	var lookups int

	cache := newAuthDecisionCache()

	authorize := func(topic string, _ *events.Event) bool {
		return cache.allowed("topic:"+topic, func() bool {
			lookups++

			return true
		})
	}

	client, sub := streamReadLoopHarness(t, authorize)

	const flood = 500

	for i := range flood {
		// Distinct junk, each near the read limit — the shape of the attack
		// that would otherwise grow the per-socket cache for the socket's
		// lifetime.
		topic := fmt.Sprintf("%s-%d", strings.Repeat("z", 200), i)

		if err := client.WriteJSON(map[string]string{"type": "subscribe", "topic": topic}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}

		var ack map[string]any
		if err := client.ReadJSON(&ack); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}

		if ack["error"] != "unknown topic" {
			t.Fatalf("junk topic %d was not rejected: %v", i, ack)
		}
	}

	if got := cache.size(); got != 0 {
		t.Fatalf("junk topics left %d authorization entries behind", got)
	}

	if lookups != 0 {
		t.Fatalf("junk topics reached the (store-backed) authorizer %d times", lookups)
	}

	if got := len(sub.Topics()); got != 0 {
		t.Fatalf("junk topics were subscribed: %d", got)
	}
}

func TestSubscribeIsCappedPerSocket(t *testing.T) {
	t.Parallel()

	client, sub := streamReadLoopHarness(t, func(string, *events.Event) bool { return true })

	refused := 0

	for i := range maxTopicsPerSocket + 10 {
		topic := events.ConnectionQueriesTopic(uuid.New().String())

		if err := client.WriteJSON(map[string]string{"type": "subscribe", "topic": topic}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}

		var ack map[string]any
		if err := client.ReadJSON(&ack); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}

		if ack["error"] == "too many subscriptions" {
			refused++
		}
	}

	if refused == 0 {
		t.Fatal("a socket could subscribe without limit")
	}

	if got := len(sub.Topics()); got > maxTopicsPerSocket {
		t.Fatalf("held %d topics, cap is %d", got, maxTopicsPerSocket)
	}
}

func TestSubscribeAcceptsAWellFormedTopic(t *testing.T) {
	t.Parallel()

	client, sub := streamReadLoopHarness(t, func(string, *events.Event) bool { return true })

	topic := events.ConnectionQueriesTopic(uuid.New().String())

	if err := client.WriteJSON(map[string]string{"type": "subscribe", "topic": topic}); err != nil {
		t.Fatalf("write: %v", err)
	}

	var ack map[string]any
	if err := client.ReadJSON(&ack); err != nil {
		t.Fatalf("read: %v", err)
	}

	if ack["error"] != nil {
		t.Fatalf("a legitimate topic was refused: %v", ack)
	}

	if len(sub.Topics()) != 1 {
		t.Fatalf("subscription not recorded: %v", sub.Topics())
	}
}

// newStreamAuthorizerFor builds the production authorizer a socket would carry
// for this user, so the tests exercise the real seam rather than a stand-in.
func newStreamAuthorizerFor(s *Server, user *store.User) *streamAuthorizer {
	return &streamAuthorizer{server: s, user: user, cache: newAuthDecisionCache()}
}

func TestApprovalEventAuthorizationShortCircuitsForAdminAndViewer(t *testing.T) {
	t.Parallel()

	// No store at all: admins and viewers must be decided without one (they
	// read every pending query over REST), and everybody else must fail closed
	// rather than panic or allow.
	s := &Server{}

	ev := &events.Event{
		Topic: events.TopicApprovalsPending,
		Type:  events.EventApprovalPending,
		Data: map[string]any{
			"query_uid":      uuid.New().String(),
			"connection_uid": uuid.New().String(),
			"sql_text":       "DELETE FROM salaries",
		},
	}

	admin := &store.User{UID: uuid.New(), Roles: []string{store.RoleAdmin}}
	viewer := &store.User{UID: uuid.New(), Roles: []string{store.RoleViewer}}
	connector := &store.User{UID: uuid.New(), Roles: []string{store.RoleConnector}}

	if !newStreamAuthorizerFor(s, admin).mayReadApprovalEvent(context.Background(), ev) {
		t.Fatal("admin must see every pending query, as mayViewQuery says")
	}

	if !newStreamAuthorizerFor(s, viewer).mayReadApprovalEvent(context.Background(), ev) {
		t.Fatal("viewer must see every pending query, as mayViewQuery says")
	}

	if newStreamAuthorizerFor(s, connector).mayReadApprovalEvent(context.Background(), ev) {
		t.Fatal("a connector was allowed another grant's held SQL with no store to check against")
	}

	if newStreamAuthorizerFor(s, nil).mayReadApprovalEvent(context.Background(), ev) {
		t.Fatal("an unauthenticated subscriber must read nothing")
	}
}

func TestApprovalEventAuthorizationFailsClosedOnMalformedPayloads(t *testing.T) {
	t.Parallel()

	s := &Server{}
	connector := &store.User{UID: uuid.New(), Roles: []string{store.RoleConnector}}

	payloads := map[string]map[string]any{
		"no query uid":       {"connection_uid": uuid.New().String()},
		"empty query uid":    {"query_uid": ""},
		"garbage query uid":  {"query_uid": "not-a-uuid"},
		"non-string uid":     {"query_uid": 42},
		"nothing at all":     nil,
		"only the sql lives": {"sql_text": "DELETE FROM salaries"},
	}

	for name, data := range payloads {
		ev := &events.Event{Topic: events.TopicApprovalsPending, Type: events.EventApprovalPending, Data: data}

		if newStreamAuthorizerFor(s, connector).mayReadApprovalEvent(context.Background(), ev) {
			t.Fatalf("%s: an unidentifiable event was allowed through — the topic carries SQL text", name)
		}
	}
}

func TestSubscribeTimeAuthorizationSkipsThePerEventRule(t *testing.T) {
	t.Parallel()

	// A viewer would pass either way, so use the one role whose two answers
	// differ: at subscribe time there is no event to judge, and the topic gate
	// (nil store here) is the whole answer.
	s := &Server{}
	admin := &store.User{UID: uuid.New(), Roles: []string{store.RoleAdmin}}

	auth := newStreamAuthorizerFor(s, admin)

	if !auth.allowed(context.Background(), events.TopicApprovalsPending, nil) {
		t.Fatal("admin refused at subscribe time")
	}

	// A topic denial still wins over any per-event answer.
	connector := newStreamAuthorizerFor(s, &store.User{UID: uuid.New(), Roles: []string{store.RoleConnector}})
	if connector.allowed(context.Background(), events.TopicConnections, &events.Event{Topic: events.TopicConnections}) {
		t.Fatal("the connections topic is admin-only, event or no event")
	}
}
