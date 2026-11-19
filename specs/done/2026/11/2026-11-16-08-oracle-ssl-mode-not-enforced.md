---
model: sonnet
effort: medium
pr: batch
automerge: false
---

# Oracle rows accept `ssl_mode=require/verify-*`, which the proxy never applies

## Problem

Found by the security scan of 2026-11-16 (finding L3). Severity: Low.
An admin sets it, and the risk is to an on-path attacker between dbbat and the
Oracle server. It is a fail-open configuration rather than a bypass anyone
can trigger.

The API accepts every libpq `ssl_mode` on an Oracle row:
`validateSSLMode` (`internal/api/servers.go:812`, used at `:766` and `:843`)
only checks that the mode is known. The **connectivity check** honours the
mode: `upstream.ConnectOracle` turns on go-ora's TLS, and certificate
verification under `verify-*` (`internal/proxy/upstream/oracle.go:48-56`), so
*Test connection* goes green over an encrypted, verified channel. The
**proxy** never encrypts the upstream leg: it relays the client's own TNS
Connect over a plain socket "regardless of the row's ssl_mode"
(`internal/proxy/oracle/session.go:644-655`, `WithUpstreamTLS(false)`).

So an admin who sets `verify-full` on an Oracle row and sees a green check has
every reason to think the dbbat → Oracle leg is encrypted and the server
authenticated. Every real session runs in plaintext: statements, results and
the O5LOGON exchange of the stored upstream credential. The code comment
admits it ("A green check on an Oracle row at ssl_mode=require therefore proves
more than a real session will do"). The only signal in the UI is the per-session
`upstream_tls=false`. The other protocols fail closed on an `ssl_mode` they
cannot meet (`2026-11-09-06`).

## Proposal

1. API: for `protocol = oracle`, refuse `require`, `verify-ca` and
   `verify-full` with a 400 that says the Oracle upstream leg is not
   encrypted by dbbat (use network encryption on the server, or a tunnel).
   Accept `disable`, `allow`, `prefer` and empty. This goes in
   `validateCreateProtocolFields` / `validateUpdateProtocolFields`
   (`internal/api/servers.go`), on create, on update, and when an update
   changes the protocol of a pristine row.
2. Existing rows: the connectivity check adds a **warning** (the
   `Warnings` slice, `internal/proxy/conncheck/conncheck.go:171-219`, next to
   `oracleServiceNameWarnings`) on an Oracle row whose `ssl_mode` is
   `require`/`verify-*`: "the proxy does not encrypt the upstream leg; this
   check did". The row keeps working, so an upgrade breaks nothing.
3. Front: the server form hides or disables those modes for Oracle, with the
   same explanation.
4. `docs/oracle.md`: say so where `ssl_mode` is described.

## Tests

- `internal/api/servers_test.go`: creating or updating an Oracle row with
  `ssl_mode: verify-full` returns 400. With `disable`/`prefer` it succeeds. A
  PostgreSQL row with `verify-full` still succeeds (positive control).
- `internal/proxy/conncheck`: the warning is present for an Oracle row at
  `require` and absent at `disable` and for a PostgreSQL row at `require`.

## Open questions

- Refuse at the API (recommended), or only warn? Refusing is what every other
  protocol's fail-closed rule does. Existing rows are untouched by the API
  rule, and step 2 warns about them.

## Resolved open questions

- Refuse at the API (fail closed, like every other protocol); leave existing rows untouched and warn about them as in step 2. (autopilot: recommended answer)
