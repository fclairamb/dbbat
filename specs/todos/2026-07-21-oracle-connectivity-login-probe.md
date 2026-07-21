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
