package events

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func allow(string, *Event) bool { return true }

func TestPublishDeliversToSubscribedTopicOnly(t *testing.T) {
	t.Parallel()

	b := New()

	sub := b.Subscribe(allow, 8)
	defer sub.Close()

	if !sub.Subscribe("connections") {
		t.Fatal("subscribe refused")
	}

	b.Publish("connections", EventConnection, map[string]any{"x": 1})
	b.Publish("approvals/pending", EventApprovalPending, map[string]any{"x": 2})

	select {
	case ev := <-sub.Events():
		if ev.Topic != "connections" {
			t.Fatalf("got topic %q", ev.Topic)
		}
		if ev.Seq == 0 {
			t.Fatal("seq not assigned")
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}

	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected event on unsubscribed topic: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishNeverBlocksAndDropsOnOverflow(t *testing.T) {
	t.Parallel()

	b := New()

	sub := b.Subscribe(allow, 2)
	defer sub.Close()
	sub.Subscribe("connections")

	done := make(chan struct{})

	go func() {
		defer close(done)

		for i := 0; i < 100; i++ {
			b.Publish("connections", EventConnection, map[string]any{"i": i})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked — the proxy hot path would have stalled")
	}

	if got := sub.Dropped(); got == 0 {
		t.Fatal("expected drops on an overflowed subscriber")
	}

	// Dropped resets.
	if got := sub.Dropped(); got != 0 {
		t.Fatalf("Dropped did not reset: %d", got)
	}
}

func TestApprovalsPendingIsExemptFromDropping(t *testing.T) {
	t.Parallel()

	b := New()

	sub := b.Subscribe(allow, 1)
	defer sub.Close()
	sub.Subscribe(TopicApprovalsPending)

	const n = 20
	for i := 0; i < n; i++ {
		b.Publish(TopicApprovalsPending, EventApprovalPending, map[string]any{"i": i})
	}

	if got := sub.Dropped(); got != 0 {
		t.Fatalf("approvals/pending must never drop, got %d drops", got)
	}

	seen := 0

	for {
		select {
		case <-sub.Events():
			seen++

			continue
		default:
		}

		break
	}

	seen += len(sub.TakePriority())

	if seen != n {
		t.Fatalf("lost pending events: saw %d of %d", seen, n)
	}
}

func TestAuthorizedReflectsRevocation(t *testing.T) {
	t.Parallel()

	b := New()

	var mu sync.Mutex
	allowed := true

	sub := b.Subscribe(func(string, *Event) bool {
		mu.Lock()
		defer mu.Unlock()

		return allowed
	}, 8)
	defer sub.Close()

	if !sub.Subscribe("connections") {
		t.Fatal("subscribe refused while authorized")
	}

	if !sub.Authorized(Event{Topic: "connections"}) {
		t.Fatal("Authorized disagreed with Subscribe")
	}

	mu.Lock()
	allowed = false
	mu.Unlock()

	// The transport asks again immediately before each write; a reader whose
	// access was revoked mid-stream must stop receiving at once, not at the
	// next reconnect.
	if sub.Authorized(Event{Topic: "connections"}) {
		t.Fatal("Authorized still true after access was revoked")
	}
}

func TestPublishNeverCallsTheAuthorizer(t *testing.T) {
	t.Parallel()

	b := New()

	calls := make(chan struct{}, 16)

	sub := b.Subscribe(func(string, *Event) bool {
		// The API's authorizer reads the store. If the broker called it on
		// the publish path, a slow database would stall the proxy session
		// that published — including one parked on an approval hold.
		calls <- struct{}{}

		return true
	}, 8)
	defer sub.Close()

	sub.Subscribe("connections")

	// Drain the subscribe-time call.
	<-calls

	b.Publish("connections", EventConnection, nil)
	b.Publish("connections", EventConnection, nil)

	select {
	case <-calls:
		t.Fatal("Publish called the authorizer — publishing must be purely in-memory")
	case <-time.After(50 * time.Millisecond):
	}

	// The events are still queued for the transport, which does the re-check.
	if len(sub.Events()) != 2 {
		t.Fatalf("queued %d events, want 2", len(sub.Events()))
	}
}

func TestSubscribeRefusedWhenUnauthorized(t *testing.T) {
	t.Parallel()

	b := New()

	sub := b.Subscribe(func(string, *Event) bool { return false }, 4)
	defer sub.Close()

	if sub.Subscribe("connections") {
		t.Fatal("subscribe should have been refused")
	}
}

func TestForwarderSeesLocalPublishesOnly(t *testing.T) {
	t.Parallel()

	b := New()

	var (
		mu  sync.Mutex
		got []Event
	)

	b.SetForwarder(func(ev Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})

	b.Publish("connections", EventConnection, nil)
	b.PublishLocal("connections", EventConnection, nil)

	mu.Lock()
	defer mu.Unlock()

	if len(got) != 1 {
		t.Fatalf("forwarder saw %d events, want 1 (PublishLocal must not re-forward)", len(got))
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	b := New()
	sub := b.Subscribe(allow, 4)
	sub.Close()
	sub.Close()

	if b.SubscriberCount() != 0 {
		t.Fatal("subscriber not detached")
	}
}

func TestTopicHelpers(t *testing.T) {
	t.Parallel()

	topic := ConnectionQueriesTopic("abc")
	if topic != "connection/abc/queries" {
		t.Fatalf("got %q", topic)
	}

	uid, ok := ConnectionUIDFromTopic(topic)
	if !ok || uid != "abc" {
		t.Fatalf("got %q ok=%v", uid, ok)
	}

	if _, ok := ConnectionUIDFromTopic("connections"); ok {
		t.Fatal("connections must not parse as a per-connection topic")
	}

	if _, ok := ConnectionUIDFromTopic("connection//queries"); ok {
		t.Fatal("empty uid must not parse")
	}
}

func TestValidTopic(t *testing.T) {
	t.Parallel()

	valid := []string{
		TopicApprovalsPending,
		TopicConnections,
		ConnectionQueriesTopic("6f1b0a52-4e0d-4a6e-9f38-2c4a1b7d9e01"),
	}

	for _, topic := range valid {
		if !ValidTopic(topic) {
			t.Fatalf("rejected a real topic: %q", topic)
		}
	}

	invalid := []string{
		"",
		"nonsense",
		"connection//queries",
		"connection/not-a-uuid/queries",
		"connection/6f1b0a52-4e0d-4a6e-9f38-2c4a1b7d9e01/rows",
		"approvals/pending ", // trailing space
		strings.Repeat("x", MaxTopicLength+1),
	}

	for _, topic := range invalid {
		if ValidTopic(topic) {
			t.Fatalf("accepted junk as a topic: %q", topic)
		}
	}
}
