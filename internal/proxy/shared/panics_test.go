package shared_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestRunRelayPassesThroughResult keeps the happy path honest: no panic, no
// wrapping, the relay's own error (including nil) is what the session sees.
func TestRunRelayPassesThroughResult(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("upstream closed") //nolint:err113 // test-local sentinel

	if err := shared.RunRelay(context.Background(), discardLogger(), "c2u", func() error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("expected the relay's own error, got %v", err)
	}

	if err := shared.RunRelay(context.Background(), discardLogger(), "c2u", func() error {
		return nil
	}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestRunRelayRecoversPanic is the whole point: the panic must not reach the
// runtime (an unrecovered panic in any goroutine kills the test binary, which is
// itself the assertion) and it must come back as an error, because the caller's
// `errCh <- RunRelay(...)` is what unblocks the session's teardown.
func TestRunRelayRecoversPanic(t *testing.T) {
	t.Parallel()

	errCh := make(chan error, 1)

	go func() {
		errCh <- shared.RunRelay(context.Background(), discardLogger(), "upstream→client", func() error {
			var pkt []byte

			_ = pkt[7] // index out of range, the shape a malformed frame takes

			return nil
		})
	}()

	err := <-errCh
	if !errors.Is(err, shared.ErrRelayPanic) {
		t.Fatalf("expected ErrRelayPanic, got %v", err)
	}

	if got := err.Error(); !strings.Contains(got, "upstream→client") {
		t.Fatalf("expected the direction in the error, got %q", got)
	}
}

// TestRunRelayNilLoggerStillRecovers covers the half-built session: recovering
// must not itself depend on a logger having been wired.
func TestRunRelayNilLoggerStillRecovers(t *testing.T) {
	t.Parallel()

	err := shared.RunRelay(context.Background(), nil, "c2u", func() error {
		panic("boom")
	})
	if !errors.Is(err, shared.ErrRelayPanic) {
		t.Fatalf("expected ErrRelayPanic, got %v", err)
	}
}

// TestRunRelayRunsTheRelaysOwnDefers is the other half of "the session tears
// down normally": a relay's deferred cleanup (a dump flush, a query finalized
// with its real reason) has to run on the panic path too, or recovering merely
// swaps a dead process for a session that ended without recording why.
func TestRunRelayRunsTheRelaysOwnDefers(t *testing.T) {
	t.Parallel()

	cleaned := false

	err := shared.RunRelay(context.Background(), discardLogger(), "c2u", func() error {
		defer func() { cleaned = true }()

		panic("boom")
	})

	if !errors.Is(err, shared.ErrRelayPanic) {
		t.Fatalf("expected ErrRelayPanic, got %v", err)
	}

	if !cleaned {
		t.Fatal("the relay's own deferred cleanup did not run")
	}
}

// TestRunGuardedRecoversAndRunsDefers pins the no-error-channel variant: the
// goroutine's own defers still run, which is how it ends the session. That
// precondition is RunGuarded's entire contract — a goroutine that owns nothing
// it closes belongs under RunWatchdog instead.
func TestRunGuardedRecoversAndRunsDefers(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		shared.RunGuarded(context.Background(), discardLogger(), "tds client reader", func() {
			defer close(done)

			panic("boom")
		})
	}()

	<-done
	<-finished
}

// TestRunWatchdogEndsTheSessionOnPanic is the enforcement half. The watchdog
// owns nothing it closes, so recovering alone would leave the session running
// with no expiry, no byte quota and no revocation check — on MongoDB, MSSQL and
// MySQL there is no other enforcement path at all. The teardown is what keeps
// "recovered" from meaning "unmetered".
func TestRunWatchdogEndsTheSessionOnPanic(t *testing.T) {
	t.Parallel()

	tornDown := make(chan struct{})

	go shared.RunWatchdog(context.Background(), discardLogger(), "limit watchdog", func() {
		panic("onViolation blew up mid-teardown")
	}, func() { close(tornDown) })

	select {
	case <-tornDown:
	case <-time.After(5 * time.Second):
		t.Fatal("a recovered watchdog panic left the session running unenforced")
	}
}

// TestRunWatchdogSurvivesATeardownPanic covers the one path with nothing left to
// try: the teardown runs from inside a recover, so a panic there would be
// exactly the process death this file exists to prevent. Reaching the end of
// this test is the assertion.
func TestRunWatchdogSurvivesATeardownPanic(t *testing.T) {
	t.Parallel()

	finished := make(chan struct{})

	go func() {
		defer close(finished)

		shared.RunWatchdog(context.Background(), discardLogger(), "limit watchdog", func() {
			panic("watch blew up")
		}, func() {
			panic("and so did the teardown")
		})
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatchdog never returned")
	}
}

// TestRunWatchdogLeavesTheTeardownAloneOnACleanExit: a watchdog that fired
// normally has already torn the session down through onViolation, and one that
// exited on ctx cancel must not tear anything down at all.
func TestRunWatchdogLeavesTheTeardownAloneOnACleanExit(t *testing.T) {
	t.Parallel()

	torn := false

	shared.RunWatchdog(context.Background(), discardLogger(), "limit watchdog",
		func() {}, func() { torn = true })

	if torn {
		t.Fatal("the teardown ran on a watchdog that did not panic")
	}
}
