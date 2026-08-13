package shared_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

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

// TestRunGuardedRecoversAndRunsDefers pins the no-error-channel variant: the
// goroutine's own defers still run, which is how it ends the session.
func TestRunGuardedRecoversAndRunsDefers(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		shared.RunGuarded(context.Background(), discardLogger(), "watchdog", func() {
			defer close(done)

			panic("boom")
		})
	}()

	<-done
	<-finished
}
