# One upstream-connect path for the proxy and the connectivity probe

## Goal

Make dbbat connect to an upstream database in exactly one way, whether it is
about to proxy a session or merely to test the server row. Today there are two
independent implementations per protocol; collapse them to one, then use that
single place to give MySQL and MongoDB the opportunistic TLS that only
PostgreSQL has.

## Why

The transport is already shared: [`shared.DialUpstream`](../../internal/proxy/shared/dial.go)
does TCP, the SSH bastion chain, connection pooling and TOFU host keys, and both
the proxies and the connectivity check go through it. That is precisely why
bastion behaviour has never drifted between them.

Everything above the socket is duplicated:

| | Proxy | Probe |
|---|---|---|
| PG TLS | hand-rolled `negotiateUpstreamSSL` (`internal/proxy/postgresql/upstream_tls.go`) | pgconn `TLSConfig` + `Fallbacks` (`internal/proxy/conncheck/probes.go`) |
| PG login | hand-rolled pgproto3 loop + own SCRAM (`internal/proxy/postgresql/upstream.go`) | pgconn `ConnectConfig` |
| MySQL | go-mysql `UseSSL`/`SetTLSConfig` (`internal/proxy/mysql/upstream.go`) | go-mysql, separate call site |
| MongoDB | `tls.Client` on the raw conn + own SCRAM (`internal/proxy/mongodb/upstream.go`) | mongo-driver `options.Client()` |
| Oracle | go-ora (`internal/proxy/oracle/`) | go-ora, separate call site |

Two implementations of one policy drift, and they did. The probe mapped
`ssl_mode=prefer` to plaintext-only while the proxy sent an `SSLRequest` and
upgraded; against a target whose `pg_hba.conf` is `hostssl`-only the plaintext
attempt is refused with SQLSTATE 28000, so **every** prefer-mode PostgreSQL
server reported `db_auth_failed` — "the database refused the stored
credentials" — for a target the proxy connects to fine. 23 of 24 registered
PostgreSQL servers were red. Fixed in `postgresTLSPlan`
(`internal/proxy/conncheck/probes.go`) by making the two paths *agree*; this
spec is about making them *the same*, so the class of bug cannot recur.

Two further consequences of the split, both user-facing:

- **A green connectivity check does not prove the proxy can connect**, because
  it is different code exercising a different library. That is the one thing the
  feature exists to tell an operator.
- **MySQL and MongoDB connect in plaintext under `ssl_mode=prefer`** — see the
  explicit comment at `internal/proxy/mysql/upstream.go` ("the client doesn't
  currently negotiate opportunistic TLS for MySQL") and the mirrored mapping in
  `internal/proxy/mongodb/upstream.go`. On the Stonal fleet every server row is
  `prefer` except one, so this is the default path, not an edge case.
  Connectivity is fine; confidentiality is not, and nothing surfaces which of
  the two happened.

The previous version of this spec said "mirror the change in the conncheck
probes". That was the wrong instruction — it would have added two more copies of
the same policy. Superseded by this one.

## Design

The proxy cannot be built on pgconn: it is a MITM that needs a raw `net.Conn` to
pump `pgproto3` messages over, and it must replay the upstream handshake to its
own client. So unify in the other direction — **the probe calls the proxy's
connect path**, and the third-party client libraries leave `conncheck` entirely.

The blocker is that the PostgreSQL upstream auth loop is fused with client-facing
work: `processUpstreamAuthMessage`
([`internal/proxy/postgresql/upstream.go`](../../internal/proxy/postgresql/upstream.go))
buffers `ParameterStatus`/`BackendKeyData`, forwards them to the client, injects
`SET SESSION default_transaction_read_only`, and registers cancel keys — all
inside the loop that is also driving the upstream login. That fusion is why the
function carries `//nolint:cyclop`. Splitting it is the actual work here, and it
is worth doing on its own merits.

Target shape, in `internal/proxy/shared` (or a new `internal/proxy/upstream`):

```go
// ConnectUpstream dials srv through its bastion chain, negotiates TLS per
// ssl_mode, and completes the protocol login with srv's stored credentials.
// It stops the instant the upstream is authenticated: the proxy carries on
// MITM-ing the returned connection, the probe closes it.
```

A single `net.Conn` return does not fit all four protocols (MySQL hands back a
`*client.Conn`, Oracle a go-ora handle), so this is a small per-protocol
connector set behind one interface, in one package, with one `ssl_mode` policy
table — not one function.

For PostgreSQL the result must carry what the login produced, so the proxy can
replay it:

```go
type PostgresUpstream struct {
    Conn              net.Conn
    Frontend          *pgproto3.Frontend
    ParameterStatuses []*pgproto3.ParameterStatus
    BackendKeyData    *pgproto3.BackendKeyData
}
```

The proxy forwards those to its client and sets read-only mode; the probe
ignores them and closes. Neither owns a second copy of the login.

## Implementation

Phased, so each phase is separately revertable. Do them in order.

1. **One `ssl_mode` policy.** A single exported mapping (mode, host) → what to
   do: attempt TLS, allow plaintext fallback, and the `tls.Config` to use.
   Encode libpq semantics once: `disable` plaintext-only; `allow` plaintext then
   TLS; `prefer`/`""` TLS then plaintext; `require` TLS without certificate
   verification; `verify-ca`/`verify-full` TLS with chain + hostname
   verification (Go cannot cleanly express verify-ca alone, so it is treated as
   verify-full — stricter than libpq, deliberately). Table-driven test over all
   six modes. `postgresTLSPlan` and `upstreamTLSConfig` both collapse into this.

2. **Split the PostgreSQL upstream login from the client replay.** Extract a
   loop that authenticates upstream and returns `PostgresUpstream`, leaving
   `Session` to do the forwarding, the read-only `SET`, and the cancel-key
   registration from the returned struct. Move `negotiateUpstreamSSL` and the
   SCRAM client into the shared package with it. The existing integration tests
   (`TestIntegration_UpstreamTLS_Require` / `_Disable` in
   `internal/proxy/postgresql/integration_test.go`) must still pass untouched —
   they are the safety net for this step.

3. **Point the probe at it.** `probePostgres` becomes "call `ConnectUpstream`,
   close". Delete the pgconn config plumbing from
   `internal/proxy/conncheck/probes.go`. Keep the probe's error *classification*
   (the `stage`/`code` mapping in `internal/proxy/conncheck/`) — that is
   genuinely probe-specific and must keep distinguishing dial / TLS / auth
   failures. `probeAppName()` stays: a probe should still be identifiable in
   `pg_stat_activity`.

4. **Same for MySQL, MongoDB, Oracle.** Cheapest first — MySQL and Oracle
   already use the same library on both sides, so those are near-mechanical.
   MongoDB drops the mongo-driver from the probe in favour of the proxy's dial +
   SCRAM.

5. **Then the original goal, once, in the shared place:** implement
   opportunistic TLS for MySQL and MongoDB.
   - MySQL: go-mysql decides TLS from the handshake's `CLIENT_SSL` capability,
     and the option callback runs before the handshake is read, so `prefer`
     needs two attempts — TLS first, redial plaintext when the upstream refuses.
     Narrow the retry to the "server does not support SSL" error only; an auth
     failure must terminate the chain, exactly as pgconn does.
   - MongoDB: no in-band negotiation — TLS from the first byte or not at all. So
     `prefer` is "attempt the TLS handshake, redial plaintext on handshake
     failure", gated on a handshake error rather than a post-auth error.

6. **Record which way it went.** The session already carries protocol metadata;
   add an `upstream_tls` boolean to the connection row and surface it, so "are
   we actually encrypted?" stops being a guess. This is what makes the `prefer`
   fallback safe to keep.

## Testing

- The table-driven `ssl_mode` test from phase 1 is the anti-drift gate: it
  asserts the policy for all six modes, and after phase 4 there is only one
  implementation for it to describe.
- Extend the `TestIntegration_UpstreamTLS_*` pattern from
  `internal/proxy/postgresql/integration_test.go` to the MySQL and MongoDB
  packages: one case per mode, asserting the actual encryption state of the
  upstream socket, not just that the connection succeeded.
- Keep `internal/proxy/conncheck/postgres_probe_test.go` — it asserts at the
  wire level that a `prefer` probe really sends the `SSLRequest`. After phase 3
  it is testing the shared path, which is the point.
- A test that the probe and the proxy produce the same wire behaviour for the
  same server row would be the strongest guard: a fake upstream that records
  whether an `SSLRequest` arrived, driven from both entry points, is enough.

## Risk

Phase 2 touches the hottest path in the product. Land the phases as separate
commits so a bisect lands on one of them, and lean on the existing PostgreSQL
integration tests as the gate — do not refactor them in the same change that
refactors the code they cover.

No GitHub issue filed yet — one should be opened.

## Related

Separately, the Stonal fleet's server rows should be tightened from `prefer` to
`require` now that the connectivity check works — `prefer` only ever buys a
silent downgrade. That is an ops change, not a code one.
