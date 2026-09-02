---
model: opus
effort: high
---

# `events.Subscriber` closes its channel while a publisher is mid-send — a real send-on-closed-channel panic, currently failing CI

## Problem

CI on `main` is red, and has been twice in a row for the same reason.
[Run 33494284792, job `test`](https://github.com/fclairamb/dbbat/actions/runs/33494284792/job/99812739503)
carries **two independent failures**.

### 1. `WARNING: DATA RACE` in `internal/events` — the real bug

`go test -race ./...` reports a write/read race on a channel between
`Subscriber.Close()` and `Subscriber.offer()`:

```
Write at 0x... by goroutine 58:
  runtime.closechan()
  events.(*Subscriber).Close()      internal/events/broker.go:427
  mcp.watchHold.deferwrap1()        internal/mcp/pending.go:224
Previous read at 0x... by goroutine 60:
  runtime.chansend()
  events.(*Subscriber).offer()      internal/events/broker.go:374
  events.(*Broker).deliver()        internal/events/broker.go:211
  events.(*Broker).Publish()        internal/events/broker.go:169
```

The window is plain to see in the code:

- [broker.go:363](internal/events/broker.go:363) — `offer` reads `s.closed`
  under `s.mu.RLock()`, **releases the lock**, and only then does
  `select { case s.ch <- ev: ... }` at [broker.go:374](internal/events/broker.go:374).
- [broker.go:411](internal/events/broker.go:411) — `Close` takes the write
  lock, sets `s.closed = true`, **releases it**, detaches from the broker, and
  then does `close(s.ch)` at [broker.go:427](internal/events/broker.go:427)
  with no lock held.

So a publisher that passes the `closed` check and is descheduled before the
send can wake up after `close(s.ch)` has run. That is not merely a race-detector
complaint: it is a **send on a closed channel**, i.e. a panic, on whichever
goroutine called `Publish`.

This is a **product** defect, not a test artifact. In the failing MCP run the
publisher happens to be a test helper, but the previous red run on `main`
([33427427244](https://github.com/fclairamb/dbbat/actions/runs/33427427244),
`chore(deps): update go toolchain directive to v1.26.7`) shows the exact same
race with a *product* goroutine on the send side:

```
Previous read ...
  events.(*Subscriber).offer()                 internal/events/broker.go:374
  events.(*Broker).Publish()                   internal/events/broker.go:169
  shared.(*ApprovalGate).announceResolved()    internal/proxy/shared/approval.go:621
```

`Publish` is documented as "never blocks, never errors" and is called from
proxy sessions, the approval gate and the MCP server — including from a proxy
session parked on an approval hold. A panic there is caught by
`safe.RunGuarded` in some call paths, but not all, and even where it is caught
the event is lost and the goroutine dies. The subscriber's channel is closed
exactly when an SSE client disconnects or an MCP `watchHold` returns, which is
routine, so the window opens constantly under normal load.

Fallout in the failing run: five `internal/mcp` tests fail
(`TestApprovalHoldThenAwaitApproval`, `TestAwaitApprovalRejectsForeignExecution`,
`TestSlowQueryIsStillRunningNotPending`, `TestLoopbackExecutorDialsTheRightListener`,
`TestMSSQLIsExecStatement`) — the last two only because Go fails every test in a
package once a race fires.

### 2. `TestCheck_OracleTarget_ThroughTunnel` — a flaky test harness

```
oracle_probe_test.go:241: stage/code = target_auth/db_handshake_failed,
    want target_auth/db_auth_failed (msg=the target was reachable but the
    database handshake failed: EOF)
oracle_probe_test.go:249: message = "...handshake failed: EOF", want it to
    contain the ORA-01017 text go-ora saw
```

The TNS refusal packet did not survive the fake SSH tunnel, so go-ora saw a
bare EOF instead of `ORA-01017`. `tnsRefuseListener`
([oracle_probe_test.go:32](internal/proxy/conncheck/oracle_probe_test.go:32))
already guards its own half of this — it `CloseWrite()`s and then blocks
draining, with a comment naming "a tunnel that pumps data via an intermediate
`io.Copy`" as the hazard. The fake SSH relay is the *other* half and does not
have the same care:

```go
// internal/proxy/conncheck/conncheck_test.go:213
func pipeChannel(ch ssh.Channel, upstream net.Conn) {
	go func() { _, _ = io.Copy(upstream, ch); _ = upstream.Close() }()
	go func() { _, _ = io.Copy(ch, upstream); _ = ch.Close() }()
}
```

Each direction reacts to its own EOF with a **full** close of the other end,
tearing down the opposite direction along with it. When the upstream
half-closes after writing the refusal, `ch.Close()` fires while the client may
not yet have drained the bytes — degrading the refusal into an EOF, which is
precisely the failure observed.

The identical helper is duplicated at
[dial_test.go:208](internal/proxy/shared/dial_test.go:208), so the same flake is
latent in `internal/proxy/shared`.

Confirmed test-only: production never runs this pattern — the real SSH tunnel
hands out `x/crypto/ssh`'s `net.Conn` directly, with no relay loop
(`grep io.Copy internal/proxy/shared internal/proxy/upstream` finds only test
files).

## Proposal

Two separable fixes; the first is the one that matters.

### Fix 1 — make `offer`/`Close` mutually exclusive over the channel

Hold the read lock across the non-blocking send, and close the channel under
the write lock:

- In `offer` ([broker.go:363](internal/events/broker.go:363)): take
  `s.mu.RLock()`, check `s.closed` and topic subscription, and perform the
  `select { case s.ch <- ev: default: }` **while still holding it**. The send is
  non-blocking by construction, so the lock is never held across a block, and
  concurrent publishers may hold `RLock` simultaneously — concurrent sends on a
  channel are safe. Note the drop-exempt branch already upgrades to `s.mu.Lock()`
  and re-checks `s.closed`; restructure carefully to avoid taking the write lock
  while holding the read lock (Go's `RWMutex` is not upgradable) — e.g. record
  the outcome under `RLock`, release, then re-acquire `Lock` for the priority
  append, keeping its existing `!s.closed` re-check.
- In `Close` ([broker.go:411](internal/events/broker.go:411)): move `close(s.ch)`
  inside the write-locked section that sets `s.closed = true`, so no publisher
  can be between its check and its send. Keep the broker detach
  (`delete(b.subs, s.id)`) **outside** `s.mu` or ordered so no lock cycle forms
  with `b.mu` — `deliver` releases `b.mu` before calling `offer`, so
  `s.mu → b.mu` is currently the only ordering and must stay that way.
  Preserve idempotency.

Verify the invariant holds in the other direction too: `TakePriority` and
`Authorized` must not observe a closed channel state inconsistently.

Add a regression test in `internal/events` that hammers `Publish` from N
goroutines while a subscriber closes, under `-race` — the current coverage only
catches this by luck from other packages.

Consider whether the same shape exists elsewhere: grep for other
`close(` on a channel that a lock-free sender writes to.

### Fix 2 — half-close the fake SSH relay

Rewrite `pipeChannel` to propagate EOF as a **half**-close instead of a full
close, in both copies of the helper
([conncheck_test.go:213](internal/proxy/conncheck/conncheck_test.go:213) and
[dial_test.go:208](internal/proxy/shared/dial_test.go:208)):

- client→upstream EOF → `upstream.(interface{ CloseWrite() error }).CloseWrite()`
  (fall back to `Close()` if unavailable);
- upstream→channel EOF → `ch.CloseWrite()` (`ssh.Channel` provides it);
- close both ends only once **both** directions have finished (a `sync.WaitGroup`
  or a small counter).

Prefer factoring the helper into one shared test-support location rather than
fixing the same code twice — `internal/proxy/testsupport` already exists for
this. Re-run `go test -race -count=20 ./internal/proxy/conncheck/` to confirm the
flake is gone.

### Verification

`go test -race ./...` green, plus `-count=10` on `internal/events`,
`internal/mcp` and `internal/proxy/conncheck`.
