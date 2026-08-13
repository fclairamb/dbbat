package shared

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// ErrRelayPanic is what a panicking relay goroutine hands its session instead of
// taking the process down. It is wrapped, never returned bare, so a log line
// carries the direction and the panic value.
var ErrRelayPanic = errors.New("proxy relay panicked")

const (
	// LogMsgRelayPanic is the record of a panic that ended one session rather
	// than every session. It should never appear: every protocol decoder that
	// touches attacker-shaped bytes already guards itself, so reaching here
	// means a path that was believed panic-free was not.
	LogMsgRelayPanic = "recovered from panic on a proxy relay goroutine: ending the session"

	// LogMsgGoroutinePanic is the same record for a per-session goroutine that
	// has no error channel to end the session through — the limit watchdog, the
	// TDS client reader. Those tear the session down by closing what they own,
	// so recovering is the whole of the fix.
	LogMsgGoroutinePanic = "recovered from panic on a proxy session goroutine"
)

// RunRelay runs one direction of a session's byte relay and converts a panic
// into an error.
//
// Go kills the whole process on an unrecovered panic in *any* goroutine, and a
// relay is a goroutine of its own: without this, one malformed session takes
// down every other live session, including sessions belonging to other users and
// other databases. Recovering here downgrades that to what a panic anywhere else
// in a session already costs — the session ends, normally, through the same
// teardown an I/O error takes.
//
// The returned error is what makes that true: the caller's pattern is
// `errCh <- shared.RunRelay(...)`, so the send happens on the panic path too.
// Dropping it would park the session's wait on a relay that is already gone.
func RunRelay(ctx context.Context, logger *slog.Logger, direction string, fn func() error) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		logPanic(ctx, logger, LogMsgRelayPanic, direction, r)

		err = fmt.Errorf("%w (%s): %v", ErrRelayPanic, direction, r)
	}()

	return fn()
}

// RunGuarded is RunRelay for a per-session goroutine that reports nothing: it
// recovers and logs, and the goroutine's own defers (closing the channel or the
// conn it owns) are what end the session.
func RunGuarded(ctx context.Context, logger *slog.Logger, name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logPanic(ctx, logger, LogMsgGoroutinePanic, name, r)
		}
	}()

	fn()
}

// logPanic writes the panic with its stack, tolerating a nil logger so a
// half-built session (tests, a failure before the logger is wired) still gets
// the recover rather than the process death.
func logPanic(ctx context.Context, logger *slog.Logger, msg, name string, r any) {
	if logger == nil {
		logger = slog.Default()
	}

	logger.ErrorContext(ctx, msg,
		slog.String("goroutine", name),
		slog.Any("panic", r),
		slog.String("stack", string(debug.Stack())))
}
