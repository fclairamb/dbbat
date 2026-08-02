// Package events implements the in-process publish/subscribe broker behind
// dbbat's live event stream.
//
// The broker is deliberately transport-agnostic: it knows about topics,
// sequence numbers and backpressure, and nothing about WebSockets, SSE, gin or
// HTTP. That separation is the mitigation for the transport trade-off recorded
// in docs/approvals.md — the stream currently ships over a WebSocket, but the
// envelope below would ride an SSE response body unchanged, so a later swap is
// confined to internal/api/stream.go.
//
// Two invariants matter more than anything else here:
//
//   - Publishing is non-blocking and never returns an error. Events originate
//     on the proxy hot path, and a slow or broken stream subscriber must never
//     be able to stall — let alone break — a live database connection.
//   - Authorization is *not* the broker's job. Subscribers carry an authorizer
//     supplied by the API layer, re-evaluated on every send, because sql_text
//     routinely carries PII and the stream must never be a wider read path
//     than GET /api/v1/queries.
package events

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Topic names and prefixes. The namespace is the extension point; nothing else
// about the transport is query-specific.
const (
	// TopicApprovalsPending carries every approval-pending query across all
	// connections. Low volume by construction, and exempt from drop-on-overflow.
	TopicApprovalsPending = "approvals/pending"
	// TopicConnections carries connection open/close events. Admin only.
	TopicConnections = "connections"
	// TopicConnectionPrefix is the prefix of the per-connection query topic:
	// connection/<uid>/queries.
	TopicConnectionPrefix = "connection/"
	// TopicConnectionSuffix closes the per-connection query topic.
	TopicConnectionSuffix = "/queries"
)

// Event types carried in the envelope's `type` field.
const (
	// EventQuery is a query observed on a connection.
	EventQuery = "query"
	// EventApprovalPending is a query parked awaiting a human decision.
	EventApprovalPending = "approval_pending"
	// EventApprovalResolved is the resolution of a hold — approved, denied or
	// abandoned — carrying who resolved it.
	EventApprovalResolved = "approval_resolved"
	// EventConnection is a connection open/close.
	EventConnection = "connection"
)

// DefaultBuffer is the per-subscriber send-buffer depth. Deep enough to ride
// out a GC pause or a slow browser tab, shallow enough that a genuinely stuck
// subscriber is detected in bounded memory.
const DefaultBuffer = 256

// ConnectionQueriesTopic builds the per-connection query topic name.
func ConnectionQueriesTopic(connectionUID string) string {
	return TopicConnectionPrefix + connectionUID + TopicConnectionSuffix
}

// ConnectionUIDFromTopic extracts the connection uid from a
// connection/<uid>/queries topic, reporting whether the topic had that shape.
func ConnectionUIDFromTopic(topic string) (string, bool) {
	if !strings.HasPrefix(topic, TopicConnectionPrefix) || !strings.HasSuffix(topic, TopicConnectionSuffix) {
		return "", false
	}

	uid := topic[len(TopicConnectionPrefix) : len(topic)-len(TopicConnectionSuffix)]
	if uid == "" {
		return "", false
	}

	return uid, true
}

// Event is one published message. Seq is assigned by the broker at publish
// time and is monotonic per broker instance (not across replicas — clients use
// it to detect local gaps, not to order globally).
type Event struct {
	Topic string         `json:"topic"`
	Seq   uint64         `json:"seq"`
	Type  string         `json:"type"`
	Data  map[string]any `json:"data"`
	At    time.Time      `json:"at"`
}

// Forwarder is called for every locally-originated publish so the caller can
// fan the event out to other replicas (dbbat runs multiple pods; the session
// holding a query is on replica A while the admin's socket is on replica B).
// It must not block; the broker calls it inline on the publish path.
type Forwarder func(ev Event)

// Broker is the in-process fan-out hub.
type Broker struct {
	mu     sync.RWMutex
	subs   map[int64]*Subscriber
	nextID atomic.Int64
	seq    atomic.Uint64

	fwdMu     sync.RWMutex
	forwarder Forwarder
}

// New creates an empty broker.
func New() *Broker {
	return &Broker{subs: make(map[int64]*Subscriber)}
}

// SetForwarder installs (or clears, with nil) the cross-replica forwarder.
func (b *Broker) SetForwarder(f Forwarder) {
	if b == nil {
		return
	}

	b.fwdMu.Lock()
	b.forwarder = f
	b.fwdMu.Unlock()
}

// Publish delivers an event to every local subscriber of the topic and hands
// it to the forwarder for other replicas. Never blocks, never errors: a nil
// broker is a no-op so proxy code can call it unconditionally.
func (b *Broker) Publish(topic, eventType string, data map[string]any) {
	if b == nil {
		return
	}

	ev := b.build(topic, eventType, data)

	b.deliver(ev)

	b.fwdMu.RLock()
	f := b.forwarder
	b.fwdMu.RUnlock()

	if f != nil {
		f(ev)
	}
}

// PublishLocal delivers an event to local subscribers only, without
// re-forwarding. This is the entry point for events arriving *from* another
// replica — forwarding them again would loop.
func (b *Broker) PublishLocal(topic, eventType string, data map[string]any) {
	if b == nil {
		return
	}

	b.deliver(b.build(topic, eventType, data))
}

func (b *Broker) build(topic, eventType string, data map[string]any) Event {
	return Event{
		Topic: topic,
		Seq:   b.seq.Add(1),
		Type:  eventType,
		Data:  data,
		At:    time.Now().UTC(),
	}
}

func (b *Broker) deliver(ev Event) {
	b.mu.RLock()
	subs := make([]*Subscriber, 0, len(b.subs))

	for _, sub := range b.subs {
		subs = append(subs, sub)
	}
	b.mu.RUnlock()

	for _, sub := range subs {
		sub.offer(ev)
	}
}

// Authorizer decides whether a subscriber may currently receive a topic. It is
// evaluated at subscribe time *and* again on every send, so a user whose role
// or ownership changed mid-stream stops receiving immediately rather than at
// the next reconnect.
type Authorizer func(topic string) bool

// Subscribe registers a new subscriber. The returned *Subscriber must be
// closed by the caller (typically with defer) to release broker resources.
func (b *Broker) Subscribe(authorize Authorizer, buffer int) *Subscriber {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}

	sub := &Subscriber{
		id:        b.nextID.Add(1),
		broker:    b,
		ch:        make(chan Event, buffer),
		topics:    make(map[string]struct{}),
		authorize: authorize,
	}

	b.mu.Lock()
	b.subs[sub.id] = sub
	b.mu.Unlock()

	return sub
}

// SubscriberCount reports how many subscribers are attached. Test/telemetry
// helper.
func (b *Broker) SubscriberCount() int {
	if b == nil {
		return 0
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.subs)
}

// Subscriber is one client's view of the broker: a topic set plus a bounded
// delivery channel.
type Subscriber struct {
	id     int64
	broker *Broker
	ch     chan Event

	mu     sync.RWMutex
	topics map[string]struct{}
	closed bool

	// priority holds events for drop-exempt topics that did not fit in ch.
	// approvals/pending must never be lost: a dropped pending event means an
	// admin never learns a database connection is parked on them.
	priority []Event

	authorize Authorizer

	dropped atomic.Int64
}

// Events is the delivery channel. It is closed when the subscriber is closed.
func (s *Subscriber) Events() <-chan Event {
	return s.ch
}

// Subscribe adds a topic after re-checking authorization. Returns false when
// the subscriber may not read that topic.
func (s *Subscriber) Subscribe(topic string) bool {
	if s.authorize != nil && !s.authorize(topic) {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return false
	}

	s.topics[topic] = struct{}{}

	return true
}

// Unsubscribe removes a topic.
func (s *Subscriber) Unsubscribe(topic string) {
	s.mu.Lock()
	delete(s.topics, topic)
	s.mu.Unlock()
}

// Topics returns the currently subscribed topic names.
func (s *Subscriber) Topics() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]string, 0, len(s.topics))
	for t := range s.topics {
		out = append(out, t)
	}

	return out
}

// Dropped returns and resets the number of events dropped due to overflow.
// The transport turns a non-zero value into {"type":"lagged","dropped":N} so
// the client knows to refetch from REST rather than assume continuity.
func (s *Subscriber) Dropped() int64 {
	return s.dropped.Swap(0)
}

// offer attempts a non-blocking delivery. This is the whole backpressure
// policy: never block the publisher, drop-and-count on overflow, except for
// drop-exempt topics which spill into an unbounded priority list.
func (s *Subscriber) offer(ev Event) {
	s.mu.RLock()
	closed := s.closed
	_, subscribed := s.topics[ev.Topic]
	s.mu.RUnlock()

	if closed || !subscribed {
		return
	}

	// Re-check on send: a subscription authorized a minute ago may no longer
	// be (role change, grant revoked, connection reassigned).
	if s.authorize != nil && !s.authorize(ev.Topic) {
		return
	}

	select {
	case s.ch <- ev:
		return
	default:
	}

	if dropExempt(ev.Topic) {
		s.mu.Lock()
		if !s.closed {
			s.priority = append(s.priority, ev)
		}
		s.mu.Unlock()

		return
	}

	s.dropped.Add(1)
}

// TakePriority drains any drop-exempt events that overflowed the main channel.
// The transport calls this alongside each channel read so pending-approval
// events are delivered even when the client is behind on everything else.
func (s *Subscriber) TakePriority() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.priority) == 0 {
		return nil
	}

	out := s.priority
	s.priority = nil

	return out
}

// Close detaches the subscriber from the broker and closes its channel.
// Idempotent.
func (s *Subscriber) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return
	}

	s.closed = true
	s.priority = nil
	s.mu.Unlock()

	s.broker.mu.Lock()
	delete(s.broker.subs, s.id)
	s.broker.mu.Unlock()

	close(s.ch)
}

// dropExempt reports whether events on a topic must never be dropped.
func dropExempt(topic string) bool {
	return topic == TopicApprovalsPending
}
