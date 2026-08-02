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

	gin.SetMode(gin.TestMode)

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

	gin.SetMode(gin.TestMode)

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

	cache := newTopicAuthCache(func(string) bool {
		calls++

		return true
	})

	for range 5 {
		if !cache.allowed("connections") {
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

	cache := newTopicAuthCache(func(string) bool { return allow })

	if !cache.allowed("connections") {
		t.Fatal("first lookup denied")
	}

	allow = false

	// Age the entry past the TTL: the re-check is the PII containment
	// guarantee, so a revoked reader must stop receiving within seconds.
	cache.mu.Lock()
	cache.entries["connections"] = topicAuthEntry{allowed: true, at: time.Now().Add(-2 * topicAuthTTL)}
	cache.mu.Unlock()

	if cache.allowed("connections") {
		t.Fatal("a stale entry outlived the TTL — a revoked reader would keep receiving")
	}
}

func TestTopicAuthCacheIsPerTopic(t *testing.T) {
	t.Parallel()

	cache := newTopicAuthCache(func(topic string) bool {
		return topic == "connections"
	})

	if !cache.allowed("connections") {
		t.Fatal("allowed topic denied")
	}

	if cache.allowed("approvals/pending") {
		t.Fatal("a decision leaked across topics")
	}
}

func TestTopicAuthCacheIsBounded(t *testing.T) {
	t.Parallel()

	cache := newTopicAuthCache(func(string) bool { return false })

	// Denials are cached too, so an unbounded map is memory a client controls.
	for i := range maxTopicAuthEntries * 4 {
		cache.allowed(fmt.Sprintf("connection/%d/queries", i))
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

	cache := newTopicAuthCache(func(string) bool {
		lookups++

		return true
	})

	client, sub := streamReadLoopHarness(t, cache.allowed)

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

	client, sub := streamReadLoopHarness(t, func(string) bool { return true })

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

	client, sub := streamReadLoopHarness(t, func(string) bool { return true })

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
