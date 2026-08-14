// Package safe holds the goroutine panic guards every other package wraps its
// detached goroutines in.
//
// It is deliberately a *leaf*: it imports the standard library and nothing else,
// which is the whole reason it exists as its own package rather than living in
// internal/proxy/shared alongside the proxies that were its first callers.
// `shared` sits mid-graph — it imports internal/cache and
// internal/proxy/upstream — so a goroutine in either of those could not reach
// the guards without an import cycle, and had to hand-copy the recover instead.
// Two copies of a recover is exactly one copy too many: the log message, the
// attribute names and the stack handling drift, silently, and the drift only
// shows up in an incident.
//
// Keep it a leaf. A dependency on any dbbat package here re-creates the cycle
// for whoever is unlucky enough to sit downstream of it; TestPackageImportsOnly
// StandardLibrary fails the build if one is added.
package safe

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

	// LogMsgMaintenancePanic is the record of a panic in one turn of a
	// background maintenance loop — a dump-retention sweep, an auth-cache
	// eviction, one queued capture upload. The turn is lost; the loop is not.
	LogMsgMaintenancePanic = "recovered from panic in a background maintenance task: skipping this turn"
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
// `errCh <- safe.RunRelay(...)`, so the send happens on the panic path too.
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

// RunGuarded is RunRelay for a goroutine that reports nothing and whose own
// defers already release whatever waits on it: it recovers and logs, and the
// closing of the channel or conn the goroutine owns does the rest.
//
// That precondition is the whole of its contract, and it is narrow. Use
// RunWatchdog for a goroutine that owns nothing it closes — recovering there
// without a teardown does not save the session, it only makes the session
// outlive the thing that was supposed to police it.
//
// Two shapes satisfy it. The first is a per-session goroutine whose defers end
// the session (the TDS client reader). The second is a *detached store write* —
// the query record, the completion update, the usage bump every proxy fires and
// then forgets about: it is spawned precisely so the wire never waits on the
// store, so nothing downstream is owed anything, and recovering costs one
// unwritten row instead of every live session on the process. Where such a
// goroutine does own a QuerySink the session later blocks on, it must
// `defer sink.Fail()` inside fn — Fail is a no-op once Resolve has run, so it
// bites only on the panic path.
//
// Three long-lived loops are wrapped whole rather than per turn, and only one of
// them meets the contract above cleanly:
//
//   - The RowWriter drain. Its own `defer close(w.done)` is exactly the release
//     this contract asks for — every producer parked in AddAll or Flush selects
//     on it — so a recovered panic degrades capture process-wide and frees
//     everyone waiting.
//   - Slack Socket Mode's transport (client.RunContext) and the cross-replica
//     event listener (store.ListenEvents). These release nothing, so strictly
//     they fit neither shape, and a recovered panic retires them until the
//     process restarts — the outcome RunMaintenance's doc argues against. They
//     are wrapped here anyway for two reasons: both are a single blocking call
//     into code we do not own, so there is no per-turn seam to guard instead,
//     and both already treat their own termination as a supported state, logging
//     a "stopped" warning that an operator sees. The turns that *do* have a seam
//     — one Socket Mode event, one republished notification — are guarded
//     per-turn with RunMaintenance inside them, so a panic in dbbat's own
//     handling costs one event rather than the receiver.
//
// A background loop that has a per-turn seam wants RunMaintenance, not this.
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

// RunMaintenance runs one turn of a background maintenance loop and converts a
// panic into a log line.
//
// It is meant to be called *inside* the loop, around the turn's body, not
// wrapped around the `go` that starts the loop. These loops own nothing a
// session waits on, so recovering is safe either way — but wrapping the whole
// loop would silently retire it for the lifetime of the process, and a proxy
// that quietly stopped expiring captures, evicting cached credentials or
// draining the upload queue is worse than one that logs a panic and sweeps
// again on the next tick. Guarded per turn, the blast radius is one turn.
//
// The exception is a loop whose exit releases producers that are blocked on it;
// that shape belongs under RunGuarded, whose doc names the one instance.
func RunMaintenance(ctx context.Context, logger *slog.Logger, name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logPanic(ctx, logger, LogMsgMaintenancePanic, name, r)
		}
	}()

	fn()
}

// LogGoroutinePanic records a recovered goroutine panic in the same shape
// RunGuarded would, for the caller that cannot use RunGuarded because its
// goroutine owes something a plain recover does not discharge — an MCP
// execution whose result channel only its own finish() closes, say. Such a
// caller writes the recover itself, does the discharging, and reports it here so
// the log line, the stack and the "goroutine" attribute match every other one.
//
// Reach for this last. RunGuarded covers a goroutine that releases what it owns
// through its own defers, and RunWatchdog covers one whose obligation is a
// session teardown.
func LogGoroutinePanic(ctx context.Context, logger *slog.Logger, name string, r any) {
	logPanic(ctx, logger, LogMsgGoroutinePanic, name, r)
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
