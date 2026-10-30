# Quiet the store LISTEN reconnects and classify upstream EOFs

## Goal

Two remaining sources of misleading log lines seen in Stonal's production logs
(OpenObserve, `app_dbbat`, 7 days, dbbat 0.28.0) after the client-hangup fix:

1. `pgdriver: ... bun: discarding bad listener connection: FATAL: terminating
   connection due to idle-session timeout (SQLSTATE=57P05)`, every 2 hours per
   replica. Written by bun's `pgdriver` through the standard `log` package, so
   it is plain text, not JSON, and has no level or `msg` field.
2. `Session error: failed to receive from upstream: unexpected EOF`, ~100 a
   week at ERROR, on PostgreSQL.

## Why

(1) is the storage database's `idle_session_timeout` closing the LISTEN
connection that `StartEventListener` keeps open; bun reconnects on its own, so
nothing is lost, but the line shows up unstructured and is the only one an
aggregator cannot parse. (2) is the target ending the session (an admin
`pg_terminate_backend`, a failover, the target's own idle timeout): worth
recording, but it is not a dbbat failure either, and it is indistinguishable
today from a real relay bug.

## Implementation

- (1) Route `pgdriver`'s logger into slog (`pgdriver.WithLogger` or the
  package-level logger, depending on the bun version) at DEBUG, or keep the
  LISTEN connection alive with a periodic no-op so the storage timeout never
  fires. `internal/store` owns the listener.
- (2) In `internal/proxy/postgresql/session.go` (`proxyUpstreamToClient`, the
  `failed to receive from upstream` site), wrap a FIN/ErrUnexpectedEOF from
  upstream in its own sentinel and log it from `handleConnection` at WARN as
  "Upstream closed the session", keeping the remote address and the
  connection uid. Mirror the shape of `errClientHungUp`.
