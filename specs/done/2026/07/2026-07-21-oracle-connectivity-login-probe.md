# Add an Oracle login probe to the connectivity check

## Goal

Make `POST /api/v1/servers/{uid}/test` verify Oracle credentials, not just TCP
reachability, so an Oracle target gets the same provisioning-time signal as
PostgreSQL, MySQL/MariaDB and MongoDB.

## Why

The connectivity check added in `internal/proxy/conncheck` runs a real
protocol-level login for every protocol except Oracle. Oracle's TNS/TTC
handshake in `internal/proxy/oracle/` only exists inside a live proxy session
(it is driven from `Session` methods with a downstream client attached), so
there is no standalone dial path to reuse. The check therefore stops at TCP
reachability and returns `stage: target_dial`, `code: auth_not_verified`.

That is honest, but it leaves the exact failure the parent spec set out to
eliminate — a wrong username or password that looks identical to success —
in place for Oracle targets specifically.

## Implementation

- `github.com/sijms/go-ora/v2` is already a dependency and speaks the client
  side of TTC. Check whether it can be given a custom transport (a
  `net.Conn` produced by `shared.Dialer.DialUpstream`) — v2 exposes
  `go_ora.NewConnector` / a `Dialer` registration hook; if a connection can be
  built over an injected conn, a probe is a few lines, mirroring
  `probePostgres` / `probeMySQL` in `internal/proxy/conncheck/probes.go`.
- If go-ora cannot accept an injected transport, the alternative is to factor
  the proxy's own O5LOGON client path out of `internal/proxy/oracle/` into a
  session-free helper and call that.
- Add the probe to `probeFor()` and cover it in
  `internal/proxy/conncheck/conncheck_test.go` (a wrong-password case must land
  on `stage: target_auth`, `code: db_auth_failed`). The Oracle integration
  tests behind `-tags integration` are the natural home for a live-server case.

No GitHub issue exists for this yet — one should be filed, alongside the issue
for the parent spec (`specs/todos/2026-07-20-ssh-bastion-connectivity-test.md`).

## Implementation Plan

Investigation result: **go-ora v2.9.0 does accept an injected transport.**
`go_ora.NewConnector(dsn)` returns an `*OracleConnector` exposing
`Dialer(configurations.DialerContext)`, and `network.Session.Connect` uses that
dialer for every dial (including the redirect retry). So no refactor of
`internal/proxy/oracle/` is needed — the fallback branch of the spec's
"Implementation" section does not apply.

One transport caveat: `network.Session.initRead`/`initWrite` call
`SetReadDeadline`/`SetWriteDeadline` **unconditionally** (with the zero time when
no `TIMEOUT` option is set). An SSH-tunneled `net.Conn` from
`golang.org/x/crypto/ssh` rejects `SetDeadline` outright, so the probe must wrap
the injected conn in a shim that no-ops a zero deadline. The probe must also
avoid setting go-ora's `TIMEOUT` option for the same reason — timeouts are
already enforced by the conncheck harness, which force-closes the transport.

Steps:

1. **`internal/proxy/conncheck/probes.go` — `probeOracle`.**
   Build a go-ora DSN with `go_ora.BuildUrl(host, port, service, user, password,
   opts)` where the service is `OracleServiceName` falling back to
   `DatabaseName` (mirrors `relay_preauth.go`'s upstream-service resolution).
   Options: `PROGRAM` = `probeAppName()`, plus `SSL`/`SSL VERIFY` derived from
   `SSLMode` the same way the other probes do. Inject the dial via
   `connector.Dialer(...)`, wrapped in the deadline shim. `Connect(ctx)` then
   `Close()`.
2. **Wire it into `probeFor()`** for `store.ProtocolOracle` and rewrite the
   package/probe doc comments that assert Oracle has no standalone dial path.
3. **Classification.** `isDBAuthRejection` must recognise Oracle's rejections.
   go-ora renders a server-side auth failure as `ORA-01017: invalid
   username/password; logon denied`, but a listener-side refuse packet is
   rendered by go-ora's own table as the shorter `ORA-1017` (no zero padding),
   so match with a regexp over `ORA-0*<code>` for the auth-ish codes (1017,
   1005, 1004, 1045, 1031, 28000, 28001, 9911). Service-name/listener errors
   (12514, 12505, 12541, ...) deliberately stay `db_handshake_failed`.
4. **Tests** (`conncheck_test.go`): a fake TNS listener that answers the Connect
   packet with a Refuse packet carrying a chosen `(ERR=nnnn)` — enough to drive
   the real go-ora client end to end over the injected dialer.
   - `ERR=1017` → `target_auth` / `db_auth_failed` (the spec's wrong-password
     case).
   - `ERR=12514` → `target_auth` / `db_handshake_failed`.
   - echo target (reachable, speaks no TNS) → `target_auth`, not
     `auth_not_verified`.
   - Oracle through an SSH tunnel → proves the deadline shim works on a
     `golang.org/x/crypto/ssh` channel.
   - unit table for the Oracle needles in `isDBAuthRejection`.
   The two existing tests that assert Oracle yields `auth_not_verified` move to
   a protocol that genuinely has no probe.
5. **`internal/api/openapi.yml`**: drop the "Oracle has no standalone dial path"
   sentence and list Oracle among the probed protocols; regenerate
   `front/src/api/schema.ts` with `bun run generate-client`.
6. A live-server case belongs in `internal/proxy/oracle/integration_test.go`
   behind `-tags integration`; note it if not runnable here.
