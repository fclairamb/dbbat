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
