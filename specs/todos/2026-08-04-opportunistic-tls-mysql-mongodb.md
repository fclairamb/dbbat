# Opportunistic upstream TLS for MySQL and MongoDB

## Goal

Make `ssl_mode=prefer` (and `allow`, and the empty default) actually negotiate
TLS upstream for the MySQL/MariaDB and MongoDB proxies, the way the PostgreSQL
proxy already does — instead of silently connecting in plaintext.

## Why

`ssl_mode` is one column shared by all four protocols, so an operator reasonably
reads `prefer` as "encrypt when the server offers it". Today that is only true
for PostgreSQL:

- `internal/proxy/postgresql/upstream_tls.go` — sends an `SSLRequest`, upgrades
  on `'S'`, falls back to plaintext on `'N'`. Correct libpq semantics.
- `internal/proxy/mysql/upstream.go:95` — `"", "disable", "prefer", "allow"` all
  fall into the same plaintext branch, with a comment saying so.
- `internal/proxy/mongodb/upstream.go:121` — same mapping, "mirroring the MySQL
  upstream mapping".

So a MySQL or MongoDB target registered with `prefer` is proxied over the wire
in clear text, even when the server would happily have done TLS. On the Stonal
fleet every server row is `prefer` except one, which makes this the default path
rather than an edge case.

This is a confidentiality gap, not a connectivity one: the connection works, it
is just unencrypted. It is also invisible — nothing in the UI or the
connectivity check says which of the two happened.

Discovered while fixing the connectivity-check probe, which had the mirror-image
version of this bug for PostgreSQL (probe stayed plaintext under `prefer` while
the proxy negotiated, so every prefer-mode PostgreSQL target reported a bogus
`db_auth_failed`). Fixed in `postgresTLSPlan` — see
`internal/proxy/conncheck/probes.go`.

## Implementation

**MySQL** (`internal/proxy/mysql/upstream.go`, `applyUpstreamOptions`)

go-mysql's client decides on TLS from the server's handshake packet: the
`CLIENT_SSL` capability flag tells you whether the upstream supports it. The
option callback runs before the handshake is read, so `prefer` cannot be decided
there. Two options:

1. Read the initial handshake ourselves (we already parse enough of the
   protocol elsewhere) and set `UseSSL(true)` only when `CLIENT_SSL` is
   advertised — this needs a hook go-mysql may not expose.
2. Try `UseSSL(true)` first and reconnect in plaintext when the upstream refuses
   — same two-attempt shape as libpq's `prefer`. Simpler, costs an extra dial
   against non-TLS servers only.

Option 2 is the pragmatic one; keep the retry narrow (only on the specific
"server does not support SSL" error, never on an auth failure — an auth error
must terminate the chain, exactly as pgconn does).

**MongoDB** (`internal/proxy/mongodb/upstream.go`, `dialUpstream`)

Mongo has no in-band TLS negotiation: it is TLS-from-the-first-byte or not at
all. So `prefer` means "attempt a TLS handshake, and on failure redial
plaintext". Implement it as an explicit two-attempt dial with the second attempt
gated on a handshake error rather than a post-auth error.

**Both**

- Mirror the change in the conncheck probes (`internal/proxy/conncheck/probes.go`,
  `probeMySQL` / `probeMongo`) — the probe and the proxy must agree on the wire
  behaviour, which is precisely the bug this todo came out of. `postgresTLSPlan`
  is the shape to follow.
- Record which way it went. The session already carries protocol metadata; an
  `upstream_tls` boolean on the connection row (and in the UI) turns "are we
  encrypted?" from a guess into an answer.
- Tests: extend the existing `TestIntegration_UpstreamTLS_*` pattern in
  `internal/proxy/postgresql/integration_test.go` to the MySQL and MongoDB
  packages — one case per mode, asserting the actual encryption state of the
  upstream socket.

No GitHub issue filed yet; one should be.

## Related

Separately, the Stonal fleet's server rows should be tightened from `prefer` to
`require` now that the connectivity check works — `prefer` only ever buys a
silent downgrade. That is an ops change, not a code one.
