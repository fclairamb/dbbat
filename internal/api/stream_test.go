package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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
