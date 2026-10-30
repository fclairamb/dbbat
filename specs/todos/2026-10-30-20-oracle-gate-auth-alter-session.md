---
model: opus
effort: medium
---

# Oracle: the client's AUTH_ALTER_SESSION is run at logon without any check

## Problem

Found by the security scan of 2026-10-30 (finding L9).

Severity: Low. Triggered by an authenticated user on an Oracle server. The worst case needs an upstream account with the `SET CONTAINER` privilege.

When dbbat rewrites the client's AUTH phase 2 for the upstream login, it replaces the username, session key, password and speedy key, and passes every other key/value through verbatim, `AUTH_ALTER_SESSION` included (`internal/proxy/oracle/phase2_forward.go:178-189`). Oracle runs that value as an `ALTER SESSION` statement while the session is established. It never goes through `shared.ValidateOracleQuery`: not through the `alterSessionAllowedParams` allowlist that `read_only`/`block_ddl` apply (`internal/proxy/shared/validation.go:252` onwards), not through the outright `ALTER SESSION SET CONTAINER` refusal (`validation.go:141`), and it gets no `queries` row.

So a client can choose, at logon, session parameters dbbat refuses mid-session. With the privilege, that includes switching to another pluggable database while every `queries` row still names the granted one. That is the escape the `CONTAINER` block exists to prevent.

## Proposal

1. Extract `AUTH_ALTER_SESSION` from the client's phase 2 before the rewrite. Run its statement(s) through the same checks a mid-session `ALTER SESSION` gets: the outright blocks always, the parameter allowlist under `read_only`/`block_ddl`.
2. Refuse the login (a readable ORA error, like the other pre-auth refusals) when it fails, or when the value cannot be parsed.
3. Record what was applied, as a `queries` row or on the connection's open audit entry, so the session's starting state is on file.

## Tests

- `internal/proxy/oracle/phase2_forward_test.go`: a phase 2 whose `AUTH_ALTER_SESSION` sets `CONTAINER` is refused under every grant. One setting an unlisted parameter is refused under `read_only`. This fails before the fix.
- Positive control: the `AUTH_ALTER_SESSION` the recorded clients send (`testdata/*.pcapng`: time zone / NLS settings) is still forwarded unchanged and the recorded logins still replay.

## To verify

- The exact shape of `AUTH_ALTER_SESSION` in the recorded clients (one statement or several, terminator), so the parser accepts all of them.
