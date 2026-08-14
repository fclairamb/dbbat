# `ALTER SESSION SET CONTAINER` still escapes the grant's database under a full-write grant

**No GitHub issue filed yet — one should be.** (Automation must not run
`gh issue create`; see `specs/todos/2026-08-11-06-*.md`.)

## Goal

Decide whether `ALTER SESSION SET CONTAINER = <pdb>` should be blocked
*outright* on the Oracle proxy, the way `ALTER SYSTEM` is, rather than only
under a `read_only` / `block_ddl` grant.

## Why

Noticed while implementing the `ALTER SESSION` parameter allowlist
(`specs/todos/2026-08-13-10-*.md`, `shared.IsAllowedAlterSession`). That work
deliberately excluded `CONTAINER` from the allowlist, with the reasoning that
switching pluggable database lets a session step outside the database its grant
covers — a dbbat grant is scoped to one server row, and the whole audit trail,
quota and approval story is written against that.

But excluding it from the allowlist only restores the *pre-existing* behaviour,
and that behaviour is: `ALTER SESSION SET CONTAINER=PDB2` starts with `ALTER`,
so `read_only` and `block_ddl` refuse it — and a grant with **neither** control
(the default, "full write") allows it. The reasoning that makes it too dangerous
for a read-only session ("it steps outside the database the grant covers")
applies just as much to a full-write one: a full-write grant on PDB1 is not a
grant on PDB2, and every `queries` row written after the switch names the wrong
database.

This is pre-existing, not a regression from the allowlist work — but the
allowlist comment now states the danger explicitly, so leaving the hole
unrecorded is worse than it was.

Same question, likely same answer, for `ALTER SESSION SET CURRENT_SCHEMA` —
except there it is genuinely fine: Oracle still evaluates privileges as the
connected user and a grant is scoped to a database, not a schema. `CONTAINER` is
the one that changes which database the session is talking to.

## Implementation

- `internal/proxy/shared/validation.go`: add
  `regexp.MustCompile(`(?i)ALTER\s+SESSION\s+SET\s+"?CONTAINER"?\s*=`)` to
  `oracleBlockedPatterns`, next to `ALTER\s+SYSTEM`. That makes it a refusal
  regardless of grant controls, surfaced as `ErrOraclePatternBlocked`.
- Beware of the interaction with the allowlist: `oracleBlockedPatterns` runs
  *after* `ValidateQuery`, so an always-blocked `CONTAINER` must not be
  reachable through `IsAllowedAlterSession` — it is not today (`CONTAINER` is
  not on the list and a multi-parameter statement is refused whole), and a test
  should pin that the two mechanisms agree.
- Check the multi-parameter form: `ALTER SESSION SET CURRENT_SCHEMA=X
  CONTAINER=Y` must be blocked by the pattern too, not just by the allowlist.
- Consider whether the equivalent exists on the other four protocols
  (`USE <db>` on MySQL / SQL Server is the obvious analogue) — those proxies
  scope differently, so measure before assuming.
- Tests: `internal/proxy/shared/validation_test.go`, next to
  `TestValidateOracleQuery_BlocksDangerousPatterns` and
  `TestValidateQuery_AlterSessionCarveOut`.
