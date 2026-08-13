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

	// LogMsgGoroutinePanic is the same record for a per-session goroutine whose
	// own deferred cleanup already ends the session — the TDS client reader,
	// which closes clientGone and clientMsgs on the way out exactly as it does on
	// a read error. For that shape, and only that shape, recovering is the whole
	// of the fix. A goroutine that owns nothing it closes needs RunWatchdog.
	LogMsgGoroutinePanic = "recovered from panic on a proxy session goroutine"

	// LogMsgWatchdogPanic is the loudest of the three, and the one an operator
	// should page on: the limit watchdog is the enforcement path, so between the
	// panic and the teardown below the session was running unmetered.
	LogMsgWatchdogPanic = "recovered from panic on a proxy limit watchdog: " +
		"ending the session, which ran unenforced until this point"

	// LogMsgWatchdogTeardownPanic means the teardown itself panicked, so the
	// session may still be live with no watchdog behind it. There is nothing
	// further to try — this is the record that says so.
	LogMsgWatchdogTeardownPanic = "panic while tearing down a session whose limit watchdog panicked: " +
		"the session may still be running unenforced"
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

// RunGuarded is RunRelay for a per-session goroutine that reports nothing and
// whose own defers already end the session: it recovers and logs, and the
// closing of the channel or conn the goroutine owns does the rest.
//
// That precondition is the whole of its contract, and it is narrow. Use
// RunWatchdog for a goroutine that owns nothing it closes — recovering there
// without a teardown does not save the session, it only makes the session
// outlive the thing that was supposed to police it.
func RunGuarded(ctx context.Context, logger *slog.Logger, name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logPanic(ctx, logger, LogMsgGoroutinePanic, name, r)
		}
	}()

	fn()
}

// RunWatchdog runs a session's limit watchdog and, if it panics, ends the
// session itself.
//
// The teardown is not optional and is the difference between this and
// RunGuarded. LimitGuard.Watch owns nothing it closes: it enforces by calling
// onViolation, which force-closes the conns. Recovering a panic in there — most
// plausibly inside onViolation, mid-teardown — and simply letting the goroutine
// exit would leave the session running with no expiry, no byte quota and no
// revocation check for as long as the client cares to keep it open. On three of
// the five protocols (MongoDB, MSSQL, MySQL) the watchdog is the *only*
// enforcement path; Oracle and PostgreSQL additionally call guard.Check() on the
// relay's hot path, but that backstop is silent about the watchdog being gone.
// Trading a loud process death for a quietly unmetered session is not a trade an
// access-control proxy should make, so endSession performs what onViolation
// would have: it closes both conns, which ends the session at its next read or
// write.
//
// endSession runs from inside the recover, so it gets a recover of its own: it
// is reached only on a path where something already panicked, and a second panic
// escaping here would be the process death this whole file exists to prevent.
func RunWatchdog(ctx context.Context, logger *slog.Logger, name string, watch, endSession func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		logPanic(ctx, logger, LogMsgWatchdogPanic, name, r)

		defer func() {
			if tr := recover(); tr != nil {
				logPanic(ctx, logger, LogMsgWatchdogTeardownPanic, name, tr)
			}
		}()

		endSession()
	}()

	watch()
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
