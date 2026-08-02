package postgresql

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// fakeHoldStore is the minimum store surface an approval hold touches. It
// keeps this suite free of a database while still driving the real session
// code, the real gate and a real TCP socket — the parts that actually decide
// whether a parked database connection ever wakes up.
type fakeHoldStore struct {
	mu       sync.Mutex
	pending  []*store.Query
	resolved []string
}

func (f *fakeHoldStore) CreatePendingQuery(_ context.Context, q *store.Query, pattern string) (*store.Query, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := *q
	out.UID = uuid.New()
	status := store.ApprovalPending
	out.ApprovalStatus = &status
	out.ApprovalPattern = &pattern
	f.pending = append(f.pending, &out)

	return &out, nil
}

func (f *fakeHoldStore) ResolveQueryApproval(_ context.Context, _ uuid.UUID, status string, _ *uuid.UUID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.resolved = append(f.resolved, status)

	return nil
}

func (f *fakeHoldStore) NotifyEvent(_ context.Context, _ string, _ store.EventNotification) error {
	return nil
}

func (f *fakeHoldStore) waitPending(t *testing.T) *store.Query {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.pending)

		var q *store.Query
		if n > 0 {
			q = f.pending[n-1]
		}
		f.mu.Unlock()

		if q != nil {
			return q
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("no pending approval row was persisted — the statement was never held")

	return nil
}

func (f *fakeHoldStore) resolutions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.resolved...)
}

// heldSession builds a Session wired for approval holds over a real TCP pair,
// so Park/Unpark and disconnect detection run for real.
func heldSession(t *testing.T, patterns []string) (*Session, net.Conn, *fakeHoldStore, *approval.Registry) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer func() { _ = ln.Close() }()

	type accepted struct {
		conn net.Conn
		err  error
	}

	acc := make(chan accepted, 1)

	go func() {
		c, err := ln.Accept()
		acc <- accepted{c, err}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	a := <-acc
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = a.conn.Close()
	})

	st := &fakeHoldStore{}
	reg := approval.NewRegistry()
	broker := events.New()

	watched := shared.NewWatchedConn(a.conn)

	grant := &store.Grant{
		UID:              uuid.New(),
		ApprovalPatterns: patterns,
		ExpiresAt:        time.Now().Add(time.Hour),
	}

	sess := &Session{
		clientConn: watched,
		watched:    watched,
		logger:     slog.New(slog.DiscardHandler),
		ctx:        context.Background(),
		grant:      grant,
		guard:      shared.NewLimitGuard(grant, nil, nil),
		extendedState: &extendedQueryState{
			preparedStatements: map[string]*preparedStatement{},
			portals:            map[string]*portalState{},
		},
		approvalDeps: shared.ApprovalDeps{
			Enabled:      true,
			Store:        st,
			Registry:     reg,
			Broker:       broker,
			PollInterval: 10 * time.Millisecond,
		},
	}

	sess.approvalGate = shared.NewApprovalGate(
		sess.approvalDeps, grant, uuid.New(), &store.User{UID: uuid.New(), Username: "alice"}, "prod",
	)

	return sess, client, st, reg
}

func TestSimpleQueryHeldThenApproved(t *testing.T) {
	t.Parallel()

	sess, _, st, reg := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	errc := make(chan error, 1)
	go func() { errc <- sess.handleQuery(&pgproto3.Query{String: "DELETE FROM users"}) }()

	pending := st.waitPending(t)

	// The held row is addressable by UID while the statement hangs.
	if pending.ApprovalStatus == nil || *pending.ApprovalStatus != store.ApprovalPending {
		t.Fatalf("row not persisted as pending: %+v", pending.ApprovalStatus)
	}

	select {
	case err := <-errc:
		t.Fatalf("statement was not held (returned %v)", err)
	case <-time.After(100 * time.Millisecond):
	}

	by := uuid.New()
	reg.Resolve(approval.Decision{QueryUID: pending.UID, Status: store.ApprovalApproved, By: &by, ByName: "bob"})

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("approved statement rejected: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approved statement never resumed")
	}

	if sess.currentQuery == nil || sess.currentQuery.approvalUID != pending.UID {
		t.Fatal("completion path did not inherit the held row's uid — it would insert a duplicate")
	}
}

func TestSimpleQueryHeldThenDenied(t *testing.T) {
	t.Parallel()

	sess, _, st, reg := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	errc := make(chan error, 1)
	go func() { errc <- sess.handleQuery(&pgproto3.Query{String: "DELETE FROM users"}) }()

	pending := st.waitPending(t)
	reg.Resolve(approval.Decision{
		QueryUID: pending.UID, Status: store.ApprovalDenied, ByName: "bob", Reason: "prod freeze",
	})

	select {
	case err := <-errc:
		if !errors.Is(err, shared.ErrApprovalDenied) {
			t.Fatalf("got %v, want a denial", err)
		}

		if !contains(err.Error(), "prod freeze") {
			t.Fatalf("client-visible error lost the approver's reason: %q", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("denied statement never returned")
	}

	if sess.currentQuery != nil {
		t.Fatal("a denied statement must not be queued for execution logging")
	}
}

func TestHeldQueryAbandonedOnClientDisconnect(t *testing.T) {
	t.Parallel()

	sess, client, st, _ := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	errc := make(chan error, 1)
	go func() { errc <- sess.handleQuery(&pgproto3.Query{String: "DELETE FROM users"}) }()

	st.waitPending(t)

	// The client gives up first — the common case in production, since the
	// client's own timeout is the only clock on a hold.
	_ = client.Close()

	select {
	case err := <-errc:
		if !errors.Is(err, shared.ErrApprovalAbandoned) {
			t.Fatalf("got %v, want ErrApprovalAbandoned", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client disconnect did not end the hold — the statement would park forever")
	}

	if got := st.resolutions(); len(got) == 0 || got[0] != store.ApprovalAbandoned {
		t.Fatalf("row not marked abandoned (distinct from denied): %v", got)
	}
}

func TestHeldQueryIgnoresForeignApproval(t *testing.T) {
	t.Parallel()

	sess, _, st, reg := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	errc := make(chan error, 1)
	go func() { errc <- sess.handleQuery(&pgproto3.Query{String: "DELETE FROM users"}) }()

	pending := st.waitPending(t)

	reg.Resolve(approval.Decision{QueryUID: uuid.New(), Status: store.ApprovalApproved, ByName: "mallory"})

	select {
	case err := <-errc:
		t.Fatalf("hold released by an approval for a different query (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	reg.Resolve(approval.Decision{QueryUID: pending.UID, Status: store.ApprovalApproved, ByName: "bob"})

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("legitimate approval failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("legitimate approval never released the hold")
	}
}

func TestPipelinedBytesSurviveTheHold(t *testing.T) {
	t.Parallel()

	sess, client, st, reg := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	errc := make(chan error, 1)
	go func() { errc <- sess.handleQuery(&pgproto3.Query{String: "DELETE FROM users"}) }()

	pending := st.waitPending(t)

	// A pipelining client sends the next statement while the first is parked.
	next, err := (&pgproto3.Query{String: "SELECT 1"}).Encode(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if _, err := client.Write(next); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for sess.watched.Buffered() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	reg.Resolve(approval.Decision{QueryUID: pending.UID, Status: store.ApprovalApproved, ByName: "bob"})

	if err := <-errc; err != nil {
		t.Fatalf("approval failed: %v", err)
	}

	// The pipelined bytes must still be readable, in order, after the hold.
	backend := pgproto3.NewBackend(sess.clientConn, sess.clientConn)

	_ = sess.clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))

	msg, err := backend.Receive()
	if err != nil {
		t.Fatalf("pipelined message lost after the hold: %v", err)
	}

	q, ok := msg.(*pgproto3.Query)
	if !ok || q.String != "SELECT 1" {
		t.Fatalf("replayed the wrong bytes: %#v", msg)
	}
}

func TestNoHoldWhenPatternDoesNotMatch(t *testing.T) {
	t.Parallel()

	sess, _, st, _ := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	if err := sess.handleQuery(&pgproto3.Query{String: "SELECT 1"}); err != nil {
		t.Fatalf("unrelated statement blocked: %v", err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if len(st.pending) != 0 {
		t.Fatal("a non-matching statement was held")
	}
}

func TestExecuteHoldsAtBindTimeNotParseTime(t *testing.T) {
	t.Parallel()

	sess, _, st, reg := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	// Parse must not hold: the bind parameters are not known yet and the SQL
	// that ultimately runs is the portal's.
	if err := sess.handleParse(&pgproto3.Parse{Name: "s1", Query: "DELETE FROM users WHERE id = $1"}); err != nil {
		t.Fatalf("parse blocked: %v", err)
	}

	st.mu.Lock()
	parsedHolds := len(st.pending)
	st.mu.Unlock()

	if parsedHolds != 0 {
		t.Fatal("Parse held the statement — bind parameters are not known at Parse time")
	}

	sess.handleBind(&pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "s1"})

	errc := make(chan error, 1)
	go func() { errc <- sess.handleExecute(&pgproto3.Execute{Portal: "p1"}) }()

	pending := st.waitPending(t)
	reg.Resolve(approval.Decision{QueryUID: pending.UID, Status: store.ApprovalApproved, ByName: "bob"})

	if err := <-errc; err != nil {
		t.Fatalf("approved execute failed: %v", err)
	}

	if n := len(sess.extendedState.pendingQueries); n != 1 {
		t.Fatalf("expected 1 queued query, got %d", n)
	}

	if sess.extendedState.pendingQueries[0].approvalUID != pending.UID {
		t.Fatal("execute completion path lost the held row's uid")
	}
}

func TestCancelRequestReleasesHold(t *testing.T) {
	t.Parallel()

	sess, _, st, _ := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	errc := make(chan error, 1)
	go func() { errc <- sess.handleQuery(&pgproto3.Query{String: "DELETE FROM users"}) }()

	st.waitPending(t)

	// A CancelRequest arrives on a separate TCP connection; all it has is the
	// backend key, which routes it to this session.
	if !sess.cancelHeldQuery() {
		t.Fatal("cancel found nothing parked")
	}

	select {
	case err := <-errc:
		if !errors.Is(err, shared.ErrApprovalAbandoned) {
			t.Fatalf("got %v, want ErrApprovalAbandoned", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a held query must remain cancellable")
	}
}

func TestGrantExpiryEndsHold(t *testing.T) {
	t.Parallel()

	sess, _, st, _ := heldSession(t, []string{`(?i)^DELETE\s+FROM`})

	sess.grant.ExpiresAt = time.Now().Add(80 * time.Millisecond)
	sess.guard = shared.NewLimitGuard(sess.grant, nil, nil)

	errc := make(chan error, 1)
	go func() { errc <- sess.handleQuery(&pgproto3.Query{String: "DELETE FROM users"}) }()

	st.waitPending(t)

	select {
	case err := <-errc:
		if !errors.Is(err, shared.ErrGrantExpired) {
			t.Fatalf("got %v, want ErrGrantExpired", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("quotas/expiry must keep running while parked")
	}
}

func TestCancelRegistryRoundTrip(t *testing.T) {
	t.Parallel()

	reg := newCancelRegistry()
	sess := &Session{cancels: reg}

	sess.noteCancelKey(&pgproto3.BackendKeyData{ProcessID: 42, SecretKey: []byte("secret")})

	got, ok := reg.lookup(cancelKey{processID: 42, secretKey: "secret"})
	if !ok || got != sess {
		t.Fatal("cancel key not routable back to its session")
	}

	sess.releaseCancelKey()

	if _, ok := reg.lookup(cancelKey{processID: 42, secretKey: "secret"}); ok {
		t.Fatal("cancel key leaked after session teardown")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}
