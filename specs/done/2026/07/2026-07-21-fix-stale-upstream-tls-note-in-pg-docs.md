# Fix the stale "upstream TLS out of scope" note in docs/postgresql.md

## Goal

Rewrite the "Upstream TLS" subsection of `docs/postgresql.md` to describe what
the proxy actually does today.

## Why

`docs/postgresql.md` states:

> **Out of scope here.** The proxy currently dials the upstream PostgreSQL
> plaintext. The `databases.ssl_mode` column exists in the store but is not yet
> wired through `internal/proxy/postgresql/upstream.go`; that's a separate
> change set.

That is no longer true. `internal/proxy/postgresql/upstream_tls.go` implements
`negotiateUpstreamSSL` with `disable` / `prefer` / `require` / `verify-ca` /
`verify-full` handling, and `upstream_tls_test.go` covers each mode. An
operator reading the doc would wrongly conclude credentials travel plaintext to
the upstream. Spotted while adding the PostgreSQL integration suite.

## Implementation

- Read `internal/proxy/postgresql/upstream_tls.go` and
  `upstream_scram.go` and document the real behaviour: which `ssl_mode` values
  are honoured, what `prefer` falls back to, how verification and `ServerName`
  are set.
- While in there, consider extending
  `internal/proxy/postgresql/integration_test.go` with an upstream-TLS case
  (the upstream container would need a cert mounted in).

No GitHub issue exists for this yet — one should be filed.

## Implementation Plan

1. **Rewrite `### Upstream TLS` in `docs/postgresql.md`** to match
   `internal/proxy/postgresql/upstream_tls.go`:
   - the SSLRequest probe is sent from `connectUpstream` before the
     StartupMessage (`upstream.go:37`), after the (possibly SSH-tunnelled) dial;
   - a per-mode table: `disable` (no probe), `allow`/`prefer`/empty (probe,
     upgrade on `'S'` without verification, plaintext on `'N'`), `require`
     (upgrade unverified, fail on `'N'`), `verify-ca`/`verify-full` (full chain +
     hostname verification, fail on `'N'`);
   - note that `verify-ca` is deliberately treated as `verify-full` (Go stdlib),
     that `ServerName` is `servers.host`, TLS 1.2 floor, and the sentinel errors
     `ErrUpstreamTLSRequired` / `ErrUpstreamSSLResponse`;
   - note SCRAM channel binding is not used upstream (`upstream_scram.go` sends
     the `n,,` gs2 header, so `SCRAM-SHA-256-PLUS` is never selected).
2. **Add an upstream-TLS integration case** to
   `internal/proxy/postgresql/integration_test.go`: start the upstream Postgres
   container with `ssl=on` and a generated self-signed cert (copied + chowned in
   the container entrypoint so Postgres accepts the key permissions), create the
   server row with `ssl_mode=require`, then assert through the proxy that
   `SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()` returns true — i.e.
   the proxy→upstream leg is really encrypted. Add a negative case for
   `verify-full` against the self-signed cert.
3. Mention the new case in the doc's Testing section.
4. QA: `make build-binary`, `make lint`, `make test`, plus running the new
   integration test with `-tags integration` against Docker.
