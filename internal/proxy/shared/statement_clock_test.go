package shared

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// armedGuard builds a guard with nothing but a statement limit armed, on a
// frozen clock the test moves by hand.
func armedGuard(t *testing.T, limit, grace time.Duration, clock *StatementClock, now *time.Time) *LimitGuard {
	t.Helper()

	g := NewLimitGuard(nil, &atomic.Int64{}, &atomic.Int64{}).
		WithStatementTimeout(limit, grace, clock)
	g.setNow(func() time.Time { return *now })

	return g
}

func TestStatementClock_IdleNeverTrips(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	clock := &StatementClock{}
	g := armedGuard(t, time.Second, StatementTimeoutGrace, clock, &now)

	// Idle for an hour: no statement is executing, so nothing is over time.
	now = now.Add(time.Hour)

	if err := g.Check(); err != nil {
		t.Fatalf("idle Check() = %v, want nil", err)
	}

	if clock.Running() {
		t.Fatal("idle clock reports Running")
	}
}

func TestStatementClock_ClearedBeforeLimitNeverTrips(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	clock := &StatementClock{}
	g := armedGuard(t, 30*time.Second, StatementTimeoutGrace, clock, &now)

	clock.StartAt(now)
	now = now.Add(5 * time.Second)

	if err := g.Check(); err != nil {
		t.Fatalf("Check() mid-statement = %v, want nil", err)
	}

	clock.Stop()
	now = now.Add(time.Hour)

	if err := g.Check(); err != nil {
		t.Fatalf("Check() after completion = %v, want nil", err)
	}
}

func TestStatementClock_TripsPastLimitPlusGrace(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	clock := &StatementClock{}
	g := armedGuard(t, 30*time.Second, StatementTimeoutGrace, clock, &now)

	clock.StartAt(now)

	// Past the limit but inside the grace: the server-side setting still has
	// its chance to end the statement cleanly.
	now = now.Add(31 * time.Second)

	if err := g.Check(); err != nil {
		t.Fatalf("Check() inside grace = %v, want nil", err)
	}

	// Past limit+grace.
	now = now.Add(2 * time.Second)

	if err := g.Check(); !errors.Is(err, ErrStatementTimeout) {
		t.Fatalf("Check() past grace = %v, want ErrStatementTimeout", err)
	}

	if got := g.StatementOverrun(); got != 33*time.Second {
		t.Fatalf("StatementOverrun() = %v, want 33s", got)
	}
}

func TestStatementClock_OldestOfSeveralWins(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	clock := &StatementClock{}
	g := armedGuard(t, 10*time.Second, 0, clock, &now)

	first := now
	clock.StartAt(first)

	// A second statement is forwarded 8s later. It must not reset the clock:
	// the oldest in-flight statement is what the limit is about.
	now = now.Add(8 * time.Second)
	second := now
	clock.StartAt(second)

	if got := clock.Since(); !got.Equal(first) {
		t.Fatalf("Since() = %v, want the first statement's start %v", got, first)
	}

	now = now.Add(3 * time.Second) // first has run 11s, second 3s
	if err := g.Check(); !errors.Is(err, ErrStatementTimeout) {
		t.Fatalf("Check() = %v, want ErrStatementTimeout on the oldest", err)
	}

	// The oldest completes; the clock re-arms from the one still in flight,
	// which is well inside the limit.
	clock.Rearm(second)

	if err := g.Check(); err != nil {
		t.Fatalf("Check() after rearm = %v, want nil", err)
	}

	// Rearming with a zero instant is the same as Stop.
	clock.Rearm(time.Time{})

	if clock.Running() {
		t.Fatal("clock still Running after zero Rearm")
	}
}

func TestStatementClock_NilSafe(t *testing.T) {
	t.Parallel()

	var clock *StatementClock

	clock.Start()
	clock.StartAt(time.Now())
	clock.Stop()
	clock.Rearm(time.Now())

	if clock.Running() {
		t.Fatal("nil clock reports Running")
	}

	if got := clock.Since(); !got.IsZero() {
		t.Fatalf("nil clock Since() = %v, want zero", got)
	}
}

func TestLimitGuard_WithStatementTimeout_DisarmedCases(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	clock := &StatementClock{}
	clock.StartAt(now)

	// A non-positive limit disarms — that is the "0 = no limit" definition
	// value, and it must not be read as "trip immediately".
	zero := NewLimitGuard(nil, nil, nil).WithStatementTimeout(0, StatementTimeoutGrace, clock)
	zero.setNow(func() time.Time { return now.Add(time.Hour) })

	if err := zero.Check(); err != nil {
		t.Fatalf("zero-limit Check() = %v, want nil", err)
	}

	if got := zero.StatementLimit(); got != 0 {
		t.Fatalf("zero-limit StatementLimit() = %v, want 0", got)
	}

	// A nil clock disarms too: nothing is driving it, so there is nothing to
	// judge and a session must not be killed on an always-zero reading.
	noClock := NewLimitGuard(nil, nil, nil).WithStatementTimeout(time.Second, 0, nil)
	if err := noClock.Check(); err != nil {
		t.Fatalf("nil-clock Check() = %v, want nil", err)
	}

	// Nil guard stays nil-safe.
	var nilGuard *LimitGuard
	if got := nilGuard.WithStatementTimeout(time.Second, 0, clock); got != nil {
		t.Fatal("nil guard WithStatementTimeout returned non-nil")
	}
}

func TestLimitGuard_Watch_StatementTimeoutOnly(t *testing.T) {
	t.Parallel()

	// A grant with no byte cap, no expiry and no revocation used to make
	// Watch return immediately ("nothing to enforce"). With a statement clock
	// attached there is something to enforce.
	clock := &StatementClock{}
	clock.StartAt(time.Now().Add(-time.Hour))

	g := NewLimitGuard(nil, nil, nil).
		WithStatementTimeout(time.Second, StatementTimeoutGrace, clock)

	got := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	g.Watch(ctx, 10*time.Millisecond, func(err error) { got <- err })

	select {
	case err := <-got:
		if !errors.Is(err, ErrStatementTimeout) {
			t.Fatalf("Watch reported %v, want ErrStatementTimeout", err)
		}
	default:
		t.Fatal("Watch returned without reporting a violation")
	}
}

func TestAccessGrant_StatementTimeout_ThreeStates(t *testing.T) {
	t.Parallel()

	global := 30 * time.Second

	cases := []struct {
		name string
		defn *store.GrantDefinition
		want time.Duration
	}{
		{"nil definition inherits the global", nil, global},
		{"unset inherits the global", &store.GrantDefinition{}, global},
		{
			"explicit zero overrides the global with no limit",
			&store.GrantDefinition{StatementTimeoutSeconds: int64Ptr(0)},
			0,
		},
		{
			"positive wins over the global",
			&store.GrantDefinition{StatementTimeoutSeconds: int64Ptr(5)},
			5 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			grant := &store.Grant{Definition: tc.defn}
			if got := grant.StatementTimeout(global); got != tc.want {
				t.Fatalf("StatementTimeout(%v) = %v, want %v", global, got, tc.want)
			}
		})
	}

	// A nil grant inherits the global rather than answering "no limit": the
	// widening answer is never the safe default here.
	var nilGrant *store.Grant
	if got := nilGrant.StatementTimeout(global); got != global {
		t.Fatalf("nil grant StatementTimeout = %v, want the global %v", got, global)
	}
}

func TestResolveStatementTimeout_ParameterWinsOverEnv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		param string
		env   string
		want  time.Duration
	}{
		{"nothing set", "", "", 0},
		{"env only", "", "45s", 45 * time.Second},
		{"parameter wins", "10s", "45s", 10 * time.Second},
		{"parameter zero disables the env default", "0", "45s", 0},
		{"malformed parameter disables rather than shortens", "thirty", "45s", 0},
		{"malformed env disables", "", "thirty", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := store.ResolveStatementTimeout(
				store.Limits{StatementTimeout: tc.param},
				&config.Config{StatementTimeout: tc.env},
			)
			if got != tc.want {
				t.Fatalf("ResolveStatementTimeout = %v, want %v", got, tc.want)
			}
		})
	}

	if got := store.ResolveStatementTimeout(store.Limits{}, nil); got != 0 {
		t.Fatalf("ResolveStatementTimeout(nil cfg) = %v, want 0", got)
	}
}
