---
model: opus
effort: low
pr: batch
automerge: false
---

# An unknown `ssl_mode` on a server row silently fails open to `prefer`

## Problem

Found by the security scan of 2026-11-09 (finding L2). Severity: Low.
Triggerable by an admin who creates or edits a server row **through the API**
(scripts, Terraform, `curl` — the UI dropdown cannot produce a typo) with a
misspelled `ssl_mode`.

`PlanFor` maps any unrecognised `ssl_mode` to `prefer` in its `default:` branch
(`internal/proxy/upstream/ssl.go:102-104`): TLS is offered but **plaintext
fallback is allowed and the server is not authenticated**. The API boundary does
not validate the value — `validateCreateProtocolFields`
(`internal/api/servers.go:792-799`) only defaults an *empty* value, and
`validateUpdateProtocolFields` (`servers.go:815-844`) checks the protocol but
not `ssl_mode`. So a typo such as `verify-ful` or `verifyfull` persists, the UI
and audit trail present it as a verifying mode, and the upstream leg silently
ships the stored (encrypted-at-rest) database credentials with neither
encryption guarantees nor server authentication.

Every other security-relevant config value in dbbat fails closed
(`DBB_AUTH_DEFAULT_ROLE`, `DBB_QUERY_TAGGING_ORACLE`, `DBB_MSSQL_TLS_MAX_VERSION`,
malformed `DBB_TRUSTED_PROXIES` all fail at startup); this one branch is the
exception.

## Proposal

1. Validate `ssl_mode` against the six known values in both
   `validateCreateProtocolFields` and `validateUpdateProtocolFields`
   (`internal/api/servers.go`), returning `400` with the allowed set on an
   unknown value — matching how `DBB_MSSQL_TLS_MAX_VERSION` and a bad protocol
   are already rejected. Enumerate the known modes from one source so the API
   and `PlanFor` cannot drift.
2. Defence in depth: make `PlanFor`
   (`internal/proxy/upstream/ssl.go:102-104`) return an error (or fail closed to
   `require`/`verify-full`, not `prefer`) for a mode it does not recognise, so a
   row that reaches the proxy layer with an unknown mode is not silently
   downgraded. Decide which in the implementation (see Open questions).

## Tests

- `internal/api/servers_test.go` (or the protocol-fields validation test):
  `POST`/`PUT /servers` with `ssl_mode: "verify-ful"` returns `400` naming the
  allowed values (fails before the fix — currently accepted and persisted).
- Positive control: each of the six valid modes is accepted.
- `internal/proxy/upstream/ssl_test.go`: `PlanFor("verify-ful")` no longer
  returns the opportunistic `prefer` plan (returns an error or a fail-closed
  plan).

## Open questions

- For an already-stored unknown mode (pre-fix rows), should `PlanFor` error
  (refuse the connection) or fail closed to `require`? Recommended: fail closed
  to `require` for an existing row and reject at the API boundary for new/edited
  rows, so an upgrade does not break live sessions while new typos are caught.
