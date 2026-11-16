package shared

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// errTestStoreDown stands in for a store outage in the fail-closed test.
var errTestStoreDown = errors.New("db down")

// fakeApprovalStore records what the gate persisted without needing a DB.
type fakeApprovalStore struct {
	mu       sync.Mutex
	created  []*store.Query
	resolved []struct {
		uid    uuid.UUID
		status string
		reason string
	}
	createErr error
	notifies  atomic.Int64
}

func (f *fakeApprovalStore) CreatePendingQuery(_ context.Context, q *store.Query, pattern string) (*store.Query, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	out := *q
	out.UID = uuid.New()
	status := store.ApprovalPending
	out.ApprovalStatus = &status
	out.ApprovalPattern = &pattern

	if out.ExecutedAt.IsZero() {
		out.ExecutedAt = time.Now()
	}

	f.created = append(f.created, &out)

	return &out, nil
}

func (f *fakeApprovalStore) ResolveQueryApproval(_ context.Context, uid uuid.UUID, status string, _ *uuid.UUID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.resolved = append(f.resolved, struct {
		uid    uuid.UUID
		status string
		reason string
	}{uid, status, reason})

	return nil
}

func (f *fakeApprovalStore) NotifyEvent(_ context.Context, _ string, _ store.EventNotification) error {
	f.notifies.Add(1)

	return nil
}

func (f *fakeApprovalStore) lastCreated() *store.Query {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.created) == 0 {
		return nil
	}

	return f.created[len(f.created)-1]
}

func (f *fakeApprovalStore) resolutions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.resolved))
	for _, r := range f.resolved {
		out = append(out, r.status)
	}

	return out
}

func testGate(t *testing.T, patterns []string) (*ApprovalGate, *fakeApprovalStore, *approval.Registry) {
	t.Helper()

	st := &fakeApprovalStore{}
	reg := approval.NewRegistry()
	broker := events.New()

	grant := &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: patterns}}
	user := &store.User{UID: uuid.New(), Username: "alice"}

	gate := NewApprovalGate(ApprovalDeps{
		Enabled:      true,
		Store:        st,
		Registry:     reg,
		Broker:       broker,
		PollInterval: 10 * time.Millisecond,
	}, ApprovalDialectStandard, grant, uuid.New(), user, "prod")

	return gate, st, reg
}

func TestGateInactiveWithoutPatternsOrFlag(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: []string{"(?i)DELETE"}}}

	off := NewApprovalGate(ApprovalDeps{Enabled: false}, ApprovalDialectStandard, grant, uuid.New(), nil, "")
	if off.Active() {
		t.Fatal("gate must be inert when the feature flag is off")
	}

	if _, ok := off.Match("DELETE FROM users"); ok {
		t.Fatal("disabled gate matched")
	}

	empty := NewApprovalGate(ApprovalDeps{Enabled: true}, ApprovalDialectStandard, &store.Grant{Definition: &store.GrantDefinition{}}, uuid.New(), nil, "")
	if empty.Active() {
		t.Fatal("gate with no patterns must be inert")
	}
}

func TestGateMatchesNormalizedSQL(t *testing.T) {
	t.Parallel()

	gate, _, _ := testGate(t, []string{`(?i)^DELETE\s+FROM\s+users`})

	pattern, ok := gate.Match("   DELETE FROM users WHERE 1=1  ")
	if !ok {
		t.Fatal("leading whitespace defeated the pattern — normalization is wrong")
	}

	if pattern != `(?i)^DELETE\s+FROM\s+users` {
		t.Fatalf("wrong pattern reported: %q", pattern)
	}

	if _, ok := gate.Match("SELECT 1"); ok {
		t.Fatal("unrelated statement matched")
	}
}

func TestGateMatchingIsCaseSensitiveWithoutTheInlineFlag(t *testing.T) {
	t.Parallel()

	// Documented sharp edge: NormalizeSQL only trims, so a pattern without
	// (?i) misses lower-case SQL. Pinned here because a pattern that misses
	// is a hold that never happens.
	strict, _, _ := testGate(t, []string{`^DELETE\s+FROM`})

	if _, ok := strict.Match("DELETE FROM users"); !ok {
		t.Fatal("upper-case statement did not match")
	}

	if _, ok := strict.Match("delete from users"); ok {
		t.Fatal("matching is documented as case-sensitive; this test pins that contract")
	}

	// …and (?i), which the docs and the UI placeholder both teach, fixes it.
	lenient, _, _ := testGate(t, []string{`(?i)^DELETE\s+FROM`})

	if _, ok := lenient.Match("delete from users"); !ok {
		t.Fatal("(?i) failed to make the pattern case-insensitive")
	}
}

func TestGateSkipsUncompilablePattern(t *testing.T) {
	t.Parallel()

	gate := NewApprovalGate(ApprovalDeps{Enabled: true}, ApprovalDialectStandard, &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: []string{"(unclosed", `(?i)DROP`}}}, uuid.New(), nil, "")

	if !gate.Active() {
		t.Fatal("one bad pattern must not disable the gate")
	}

	if _, ok := gate.Match("DROP TABLE t"); !ok {
		t.Fatal("valid pattern lost")
	}
}

func TestHoldApproved(t *testing.T) {
	t.Parallel()

	gate, st, reg := testGate(t, []string{`(?i)DELETE`})

	type result struct {
		uid uuid.UUID
		err error
	}

	res := make(chan result, 1)

	go func() {
		uid, err := gate.Hold(context.Background(), HoldRequest{
			SQL:     "DELETE FROM users",
			Pattern: `(?i)DELETE`,
			Guard:   NewLimitGuard(nil, nil, nil),
		})
		res <- result{uid, err}
	}()

	pending := waitForPending(t, st)
	by := uuid.New()

	if !reg.Resolve(approval.Decision{
		QueryUID: pending.UID, Status: store.ApprovalApproved, By: &by, ByName: "bob",
	}) {
		t.Fatal("resolve found no hold")
	}

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("approved hold returned %v", r.err)
		}
		if r.uid != pending.UID {
			t.Fatal("hold returned a different query uid")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approved hold never released")
	}
}

func TestHoldDeniedCarriesReasonAndApprover(t *testing.T) {
	t.Parallel()

	gate, st, reg := testGate(t, []string{`(?i)DELETE`})

	errc := make(chan error, 1)

	go func() {
		_, err := gate.Hold(context.Background(), HoldRequest{
			SQL: "DELETE FROM users", Pattern: `(?i)DELETE`, Guard: NewLimitGuard(nil, nil, nil),
		})
		errc <- err
	}()

	pending := waitForPending(t, st)
	reg.Resolve(approval.Decision{
		QueryUID: pending.UID, Status: store.ApprovalDenied, ByName: "bob", Reason: "not during business hours",
	})

	select {
	case err := <-errc:
		if !errors.Is(err, ErrApprovalDenied) {
			t.Fatalf("got %v, want ErrApprovalDenied", err)
		}

		var denied *ApprovalDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("got %T, want *ApprovalDeniedError", err)
		}

		if denied.Reason != "not during business hours" || denied.By != "bob" {
			t.Fatalf("lost approver context: %+v", denied)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("denied hold never released")
	}
}

func TestHoldAbandonedOnClientDisconnect(t *testing.T) {
	t.Parallel()

	gate, st, _ := testGate(t, []string{`(?i)DELETE`})

	gone := make(chan struct{})
	errc := make(chan error, 1)

	go func() {
		_, err := gate.Hold(context.Background(), HoldRequest{
			SQL: "DELETE FROM users", Pattern: `(?i)DELETE`,
			ClientGone: gone, Guard: NewLimitGuard(nil, nil, nil),
		})
		errc <- err
	}()

	waitForPending(t, st)
	close(gone)

	select {
	case err := <-errc:
		if !errors.Is(err, ErrApprovalAbandoned) {
			t.Fatalf("got %v, want ErrApprovalAbandoned", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hold did not end on client disconnect")
	}

	if got := st.resolutions(); len(got) == 0 || got[0] != store.ApprovalAbandoned {
		t.Fatalf("row not marked abandoned: %v", got)
	}
}

func TestRegistryDeliveredAbandonIsPersisted(t *testing.T) {
	t.Parallel()

	gate, st, reg := testGate(t, []string{`(?i)DELETE`})

	errc := make(chan error, 1)

	go func() {
		_, err := gate.Hold(context.Background(), HoldRequest{
			SQL: "DELETE FROM users", Pattern: `(?i)DELETE`, Guard: NewLimitGuard(nil, nil, nil),
		})
		errc <- err
	}()

	pending := waitForPending(t, st)

	// This is the shape of an out-of-band cancel and of the shutdown drain:
	// the decision arrives through the registry, and nothing else writes the
	// row. If the gate does not persist it, the hold stays 'pending' forever.
	reg.Resolve(approval.Decision{
		QueryUID: pending.UID, Status: store.ApprovalAbandoned, Reason: "canceled by the client",
	})

	select {
	case err := <-errc:
		if !errors.Is(err, ErrApprovalAbandoned) {
			t.Fatalf("got %v, want ErrApprovalAbandoned", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("registry-delivered abandon never released the hold")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := st.resolutions(); len(got) > 0 {
			if got[0] != store.ApprovalAbandoned {
				t.Fatalf("persisted %q, want abandoned", got[0])
			}

			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("terminal state never persisted — the row would haunt /queries/pending")
}

func TestHoldIgnoresDecisionForAnotherQuery(t *testing.T) {
	t.Parallel()

	gate, st, reg := testGate(t, []string{`(?i)DELETE`})

	errc := make(chan error, 1)

	go func() {
		_, err := gate.Hold(context.Background(), HoldRequest{
			SQL: "DELETE FROM users", Pattern: `(?i)DELETE`, Guard: NewLimitGuard(nil, nil, nil),
		})
		errc <- err
	}()

	pending := waitForPending(t, st)

	// Deliver a decision naming a *different* query straight into this
	// hold's channel — the substitution a timing-influencing client would
	// need for a TOCTOU attack.
	reg.Resolve(approval.Decision{QueryUID: uuid.New(), Status: store.ApprovalApproved, ByName: "mallory"})

	select {
	case err := <-errc:
		t.Fatalf("hold released on a foreign approval (err=%v) — TOCTOU guard missing", err)
	case <-time.After(200 * time.Millisecond):
	}

	// The real decision still works.
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

func TestHoldTripsOnGrantExpiryWhileParked(t *testing.T) {
	t.Parallel()

	gate, st, _ := testGate(t, []string{`(?i)DELETE`})

	expired := &store.Grant{
		ExpiresAt:  time.Now().Add(80 * time.Millisecond),
		Definition: &store.GrantDefinition{},
	}
	guard := NewLimitGuard(expired, nil, nil)

	errc := make(chan error, 1)

	go func() {
		_, err := gate.Hold(context.Background(), HoldRequest{
			SQL: "DELETE FROM users", Pattern: `(?i)DELETE`, Guard: guard,
		})
		errc <- err
	}()

	waitForPending(t, st)

	select {
	case err := <-errc:
		if !errors.Is(err, ErrGrantExpired) {
			t.Fatalf("got %v, want ErrGrantExpired", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("grant expiry did not trip while parked — quotas would be bypassed by the hold")
	}
}

func TestHoldEndsOnContextCancel(t *testing.T) {
	t.Parallel()

	gate, st, _ := testGate(t, []string{`(?i)DELETE`})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)

	go func() {
		_, err := gate.Hold(ctx, HoldRequest{
			SQL: "DELETE FROM users", Pattern: `(?i)DELETE`, Guard: NewLimitGuard(nil, nil, nil),
		})
		errc <- err
	}()

	waitForPending(t, st)
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrApprovalAbandoned) {
			t.Fatalf("got %v, want ErrApprovalAbandoned", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown would hang on a parked query")
	}
}

func TestHoldFailsClosedWhenPersistFails(t *testing.T) {
	t.Parallel()

	st := &fakeApprovalStore{createErr: errTestStoreDown}

	gate := NewApprovalGate(ApprovalDeps{
		Enabled: true, Store: st, Registry: approval.NewRegistry(), Broker: events.New(),
	}, ApprovalDialectStandard, &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: []string{"x"}}}, uuid.New(), nil, "")

	_, err := gate.Hold(context.Background(), HoldRequest{SQL: "x", Pattern: "x", Guard: NewLimitGuard(nil, nil, nil)})
	if !errors.Is(err, ErrApprovalUnavailable) {
		t.Fatalf("got %v — a statement must never be forwarded because the bookkeeping broke", err)
	}
}

func TestHoldPublishesOnBothTopics(t *testing.T) {
	t.Parallel()

	st := &fakeApprovalStore{}
	reg := approval.NewRegistry()
	broker := events.New()
	connUID := uuid.New()

	gate := NewApprovalGate(ApprovalDeps{
		Enabled: true, Store: st, Registry: reg, Broker: broker, PollInterval: 10 * time.Millisecond,
	}, ApprovalDialectStandard, &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: []string{"(?i)DELETE"}}}, connUID, &store.User{UID: uuid.New(), Username: "alice"}, "prod")

	sub := broker.Subscribe(func(string, *events.Event) bool { return true }, 64)
	defer sub.Close()
	sub.Subscribe(events.TopicApprovalsPending)
	sub.Subscribe(events.ConnectionQueriesTopic(connUID.String()))

	go func() {
		_, _ = gate.Hold(context.Background(), HoldRequest{
			SQL: "DELETE FROM users", Pattern: "(?i)DELETE", Guard: NewLimitGuard(nil, nil, nil),
		})
	}()

	pending := waitForPending(t, st)

	seenTopics := map[string]bool{}
	deadline := time.After(3 * time.Second)

	for len(seenTopics) < 2 {
		select {
		case ev := <-sub.Events():
			if ev.Type == events.EventApprovalPending {
				seenTopics[ev.Topic] = true

				if ev.Data["query_uid"] != pending.UID.String() {
					t.Fatalf("event carries the wrong query uid: %v", ev.Data["query_uid"])
				}
			}
		case <-deadline:
			t.Fatalf("only saw topics %v", seenTopics)
		}
	}

	reg.Resolve(approval.Decision{QueryUID: pending.UID, Status: store.ApprovalApproved, ByName: "bob"})
}

func waitForPending(t *testing.T, st *fakeApprovalStore) *store.Query {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if q := st.lastCreated(); q != nil {
			return q
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("gate never persisted a pending query")

	return nil
}

// deleteOrdersPattern is the pattern shape docs/approvals.md teaches.
const deleteOrdersPattern = `(?i)DELETE\s+FROM\s+orders`

// TestApprovalPatternsMatchCommentedStatements is the 2026-11-16 scan's M1: a
// comment between two keywords is ignored by the database, so it must not
// carry the statement past its hold.
func TestApprovalPatternsMatchCommentedStatements(t *testing.T) {
	t.Parallel()

	compiled := CompileApprovalPatterns([]string{deleteOrdersPattern})

	cases := []struct {
		name    string
		sql     string
		dialect ApprovalDialect
		want    bool
	}{
		{"plain", "DELETE FROM orders WHERE id = 1", ApprovalDialectStandard, true},
		{"empty block comment", "DELETE/**/FROM orders", ApprovalDialectStandard, true},
		{"block comment", "DELETE /* x */ FROM orders", ApprovalDialectStandard, true},
		{"line comment", "DELETE -- x\nFROM orders", ApprovalDialectStandard, true},
		{"comment before table", "DELETE FROM /* x */ orders", ApprovalDialectStandard, true},
		{"mysql hash comment", "DELETE # x\nFROM orders", ApprovalDialectMySQL, true},
		{"mysql block comment", "DELETE/**/FROM orders", ApprovalDialectMySQL, true},
		// MySQL runs the body of /*! … */: it stays in the matched text.
		{"mysql executable comment", "DELETE /*!50000 FROM */ orders", ApprovalDialectMySQL, true},
		// Positive controls: the raw text still matches as before.
		{"pattern in a literal", "SELECT 'DELETE FROM orders'", ApprovalDialectStandard, true},
		// Open question of the spec, answered "keep": a comment holding the
		// pattern text matches the raw text, so it is held as before the fix.
		{"pattern only in a comment", "SELECT 1 /* DELETE FROM orders */", ApprovalDialectStandard, true},
		{"unrelated", "SELECT 1", ApprovalDialectStandard, false},
		{"unrelated mysql", "SELECT 1 # nothing", ApprovalDialectMySQL, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pattern, got := compiled.MatchDialect(tc.sql, tc.dialect)
			if got != tc.want {
				t.Fatalf("MatchDialect(%q) = %v, want %v", tc.sql, got, tc.want)
			}

			if got && pattern != deleteOrdersPattern {
				t.Fatalf("wrong pattern reported: %q", pattern)
			}
		})
	}
}

// TestApprovalDialectMySQLHashIsNotStandardComment pins that the dialect is
// honored: `#` is not a comment outside MySQL, so the standard dialect does
// not strip it.
func TestApprovalDialectMySQLHashIsNotStandardComment(t *testing.T) {
	t.Parallel()

	compiled := CompileApprovalPatterns([]string{deleteOrdersPattern})

	if _, ok := compiled.MatchDialect("DELETE # x\nFROM orders", ApprovalDialectStandard); ok {
		t.Fatal("standard dialect treated # as a comment")
	}
}

// TestApprovalDialectMongoDBMatchesRawTextOnly keeps MongoDB's behavior:
// extended JSON has no comments, so nothing is stripped.
func TestApprovalDialectMongoDBMatchesRawTextOnly(t *testing.T) {
	t.Parallel()

	compiled := CompileApprovalPatterns([]string{`"delete":\s*"orders"`})

	if _, ok := compiled.MatchDialect(`  {"delete": "orders"}  `, ApprovalDialectMongoDB); !ok {
		t.Fatal("raw mongo command no longer matches")
	}

	if _, ok := compiled.MatchDialect(`{"delete": /**/ "orders"}`, ApprovalDialectMongoDB); ok {
		t.Fatal("mongo dialect stripped a SQL comment")
	}
}

// TestGateMatchUsesItsDialect checks the live gate threads its dialect.
func TestGateMatchUsesItsDialect(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: []string{deleteOrdersPattern}}}

	pg := NewApprovalGate(ApprovalDeps{Enabled: true}, ApprovalDialectStandard, grant, uuid.New(), nil, "")
	if _, ok := pg.Match("DELETE /* x */ FROM orders"); !ok {
		t.Fatal("standard gate let a block comment evade the pattern")
	}

	my := NewApprovalGate(ApprovalDeps{Enabled: true}, ApprovalDialectMySQL, grant, uuid.New(), nil, "")
	if _, ok := my.Match("DELETE # x\nFROM orders"); !ok {
		t.Fatal("mysql gate let a # comment evade the pattern")
	}
}

// TestValidatePatternsMatchesCommentedStatements: the validate-patterns API
// reports what the gate does.
func TestValidatePatternsMatchesCommentedStatements(t *testing.T) {
	t.Parallel()

	_, results := ValidatePatternsAgainstQueries([]string{deleteOrdersPattern}, []string{"DELETE/**/FROM orders"})
	if len(results) != 1 || !results[0].Matched {
		t.Fatalf("validate-patterns missed a commented statement: %+v", results)
	}
}
