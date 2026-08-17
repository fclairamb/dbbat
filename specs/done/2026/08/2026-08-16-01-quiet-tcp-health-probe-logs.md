# Quiet the ERROR/WARN log spam from bare TCP health probes

## Goal

A load balancer health check that opens a TCP connection to a proxy listener and
closes it without sending a single byte should not be logged as an error. Today
it is, on every probe, on every listener — which is the dominant line in
production logs and trains the reader to ignore the `ERROR` level.

## Why

On Stonal's deployment (`tooling` namespace, behind the unified `dbbat-proxy`
NLB) the PostgreSQL listener alone produces ~22 `ERROR` lines every 3 minutes,
all identical:

```
{"level":"ERROR","msg":"Session error",
 "error":"SSL negotiation failed: peek startup header: EOF",
 "remote_addr":{"IP":"10.1.0.171",...}}
```

`10.1.0.171` is the NLB. Its health check is a plain TCP open/close, so the peek
for the startup header returns `io.EOF` before any protocol has begun. Nothing is
wrong — but a genuine session failure is now indistinguishable from probe noise
at a glance, and log volume is paid for.

MySQL has the same shape one level down: `WARN "MySQL auth failed"` with
`io.ReadFull(header) failed. err EOF`, plus an `INFO "MySQL session ended"` for
the same probe. The other three listeners should be checked for the equivalent.

## Implementation

The precedent already exists a few lines away — `internal/proxy/postgresql/server.go:256-265`
deliberately swallows `ErrCancelRequestHandled` because logging a client-side
Ctrl-C as an error would be noise on every cancel. This is the same class of
"expected, not a failure" outcome.

Sketch:

- `internal/proxy/postgresql/session.go:274` — have `negotiateSSL` distinguish
  *client hung up before saying anything* (peek returns `io.EOF` with **zero**
  bytes consumed) from a real negotiation failure. A dedicated sentinel, e.g.
  `ErrClientDisconnectedBeforeStartup`, wrapped alongside the current message.
- `internal/proxy/postgresql/server.go:264` — treat that sentinel like
  `ErrCancelRequestHandled`: log at `DEBUG` (keep it observable when chasing a
  connectivity problem) instead of `ERROR`, and keep the remote address.
- Be strict about the zero-bytes condition. An EOF *mid*-startup-packet is a
  truncated client and must stay an error; only "opened and closed saying
  nothing" is demoted.
- `internal/proxy/mysql/` — same treatment for the handshake-read EOF, and
  suppress the paired `MySQL session ended` line when the session never began.
- Check `oracle`, `mongodb` and `mssql` listeners for the same pattern; they all
  sit behind the same NLB and are presumably all probed.

A unit test per protocol: dial the listener, close immediately, assert nothing is
logged above `DEBUG`.
