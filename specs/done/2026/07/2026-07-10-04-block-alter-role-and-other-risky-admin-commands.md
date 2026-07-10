# No way to block ALTER ROLE — and other risky administrative commands slip through the proxy

## Problem

A grant with full write access (no controls, or only `block_copy`) lets the
client run `ALTER ROLE x SUPERUSER`, `ALTER ROLE x CREATEROLE`,
`ALTER ROLE x BYPASSRLS`, `GRANT ... TO ...`, etc. through the proxy. None of
the three existing controls
([`internal/store/models.go:21-23`](internal/store/models.go:21), surfaced as
the "Access Controls" checkboxes in
[`front/src/routes/_authenticated/grants/index.tsx:62`](front/src/routes/_authenticated/grants/index.tsx:62))
squarely covers privilege/role administration:

- `read_only` and `block_ddl` do catch `ALTER ROLE` — but only incidentally,
  via the `ALTER`/`GRANT` prefix lists in
  [`internal/proxy/shared/validation.go:23-29`](internal/proxy/shared/validation.go:23).
  There is no way to allow normal writes (or even schema changes) while
  blocking role/privilege administration.
- `block_ddl`'s keyword list (`CREATE, ALTER, DROP, TRUNCATE`) does **not**
  include `GRANT`/`REVOKE`, so a write grant with `block_ddl` can still
  re-grant privileges.
- The only always-on protection is the password-change block
  ([`validation.go:84-93`](internal/proxy/shared/validation.go:84)), which
  matches `ALTER ROLE/USER ... PASSWORD` only — every other `ALTER ROLE`
  form passes.

This matters more than an ordinary write: the proxy connects to the target
with **shared, dbbat-held credentials**. Role administration through it can
escalate the proxied account's own privileges, break dbbat's connectivity,
or grant access that bypasses dbbat entirely — which is why password changes
are already unconditionally blocked. Oracle and MySQL already have
always-blocked pattern lists for exactly this category
([`validation.go:32-57`](internal/proxy/shared/validation.go:32));
PostgreSQL has none.

Other risky commands that currently pass on a full-write grant:

**PostgreSQL** (no `pgBlockedPatterns` list exists today):
- `ALTER SYSTEM` — rewrites server config; blocked on Oracle but not PG.
- `COPY ... TO/FROM PROGRAM` — arbitrary command execution on the DB host;
  today only blocked when the optional `block_copy` control is set
  ([`internal/proxy/postgresql/intercept.go:66`](internal/proxy/postgresql/intercept.go:66)).
- `CREATE ROLE` / `DROP ROLE` / `ALTER DEFAULT PRIVILEGES`.
- `SET ROLE` / `SET SESSION AUTHORIZATION` — identity switching that breaks
  dbbat's per-user attribution of queries.
- `CREATE EXTENSION`, `CREATE SERVER` / `CREATE FOREIGN DATA WRAPPER` —
  server-side code loading and network egress (the PG analogue of Oracle's
  blocked `CREATE DATABASE LINK` / `UTL_HTTP`).

**MySQL** (additions to `mysqlBlockedPatterns`):
- `CREATE USER` / `DROP USER` / `RENAME USER`, `GRANT` / `REVOKE`,
  non-password `ALTER USER` forms.
- `SET PERSIST` — same effect as the already-blocked `SET GLOBAL`, but
  persists across restarts and does not match the current pattern.
- `INSTALL PLUGIN` / `UNINSTALL PLUGIN`, `CREATE FUNCTION ... SONAME` (UDF
  loading), `SHUTDOWN`.

**Oracle** (additions to `oracleBlockedPatterns`):
- Non-password `ALTER USER` forms, `CREATE USER` / `DROP USER`,
  `GRANT` / `REVOKE`, `AUDIT` / `NOAUDIT`.

## Proposal

Two layers, mirroring the existing design (always-blocked patterns + opt-in
grant controls):

### 1. Always-block privilege/identity administration (all protocols)

Extend the unconditional checks in `ValidateQuery` — the same tier as the
password-change block — to cover statements that can escalate the shared
proxy account or break attribution, regardless of grant controls:

- `ALTER ROLE` / `ALTER USER` (all forms, not just `... PASSWORD`),
  `CREATE ROLE|USER`, `DROP ROLE|USER`, `GRANT`, `REVOKE`.
- PG: introduce a `pgBlockedPatterns` list (parallel to the Oracle/MySQL
  ones) with `ALTER SYSTEM`, `COPY ... PROGRAM`, `SET SESSION AUTHORIZATION`
  / `SET ROLE`, plus the egress/code-loading candidates above.
- MySQL/Oracle: extend the existing pattern lists per the candidates above.

A new distinct error (e.g. `ErrAdminCommandBlocked`, "role and privilege
administration is not permitted through the proxy") so clients get a clear
message, consistent with `ErrPasswordChangeBlocked`.

### 2. (Optional) keep the greyer commands behind a new control

If some of the above are considered legitimate for trusted grants (most
likely `GRANT`/`REVOKE` and `CREATE EXTENSION`), add a fourth control
instead — e.g. `block_admin` ("Block admin commands") in `AllControls`
([`internal/store/models.go:28`](internal/store/models.go:28)), the OpenAPI
enum, and the two frontend checkbox lists
([`grants/index.tsx:62`](front/src/routes/_authenticated/grants/index.tsx:62),
[`grant-definitions/index.tsx:56`](front/src/routes/_authenticated/grant-definitions/index.tsx:56)).
Recommendation: start with everything always-blocked (layer 1 only, no
schema/UI change); demote specific commands to a control later if a real
use case shows up.

### Hardening notes (same code path, worth fixing while in there)

- All checks are prefix matches on `strings.TrimSpace(sql)` — a leading
  comment (`/* x */ ALTER ROLE ...`) bypasses every one of them today,
  including the password block. Strip leading comments before
  classification.
- Verify multi-statement handling: in the PG simple query protocol,
  `SELECT 1; ALTER ROLE ...` must not be classified by the first statement
  only. Check how `intercept.go` splits statements and add a test either way.

### Open questions

- Should `GRANT`/`REVOKE` be always-blocked or control-gated? (dbbat's own
  admin may legitimately manage the DB through the proxy.)
- Which of the PG egress/code-loading patterns (`CREATE EXTENSION`,
  `CREATE SERVER`, file-access functions like `pg_read_file`/`lo_import`)
  are in scope for the first pass vs. follow-up?
- `SET ROLE` is used by some connection poolers/ORMs; confirm blocking it
  doesn't break legitimate clients before including it.

### Acceptance / verification

- On a full-write grant (no controls), `ALTER ROLE x SUPERUSER`,
  `ALTER USER x CREATEROLE`, `CREATE ROLE`, `DROP ROLE`, `GRANT`, `REVOKE`
  are rejected with the new error on all three protocols.
- PG: `ALTER SYSTEM ...` and `COPY ... FROM PROGRAM '...'` are rejected even
  without `block_copy`.
- MySQL: `SET PERSIST ...`, `CREATE USER ...`, `INSTALL PLUGIN ...` are
  rejected.
- Comment-prefixed variants (`/* hi */ ALTER ROLE ...`) are rejected too.
- Ordinary DML (`INSERT`/`UPDATE`/`DELETE`) and reads still pass on a
  full-write grant; existing `read_only` / `block_copy` / `block_ddl`
  behavior is unchanged.
- Unit tests in `internal/proxy/shared/validation_test.go` per protocol and
  per new pattern; blocked queries appear in the query log with the
  rejection, as today.

## Implementation Plan

**Chosen approach: Layer 1 only (always-block), no new grant control / migration /
OpenAPI / frontend change.** This is a backend-only change confined to
`internal/proxy/shared/validation.go` (+ its test) and the PostgreSQL intercept
(`internal/proxy/postgresql/intercept.go` + `errors.go`). MySQL and Oracle already
route through `shared.ValidateQuery` (via `ValidateMySQLQuery` / `ValidateOracleQuery`),
so wiring the cross-protocol admin block into `ValidateQuery` covers them for free.
PostgreSQL does not call `ValidateQuery` (it inlines granular checks with PG-local
errors), so it gets an inline call to the new shared detector functions.

### Decisions on the open questions

- **GRANT / REVOKE → always-blocked** (not control-gated). Acceptance criteria
  require them rejected on a full-write grant on all three protocols, and the proxy
  connects with shared dbbat-held credentials, so re-granting privileges through it
  is exactly the escalation the block exists to prevent. A future `block_admin`
  control can demote them if a real admin-through-proxy use case appears (layer 2).
- **SET ROLE → deferred (NOT always-blocked).** The spec's own open question flags
  it as used by connection poolers / ORMs, and the existing PG test
  `TestHandleQuery_BlocksReadOnlyBypass` explicitly asserts `SET ROLE` is allowed on a
  write grant. It remains blocked under `read_only` (existing `readOnlyBypassPatterns`),
  which covers the highest-risk case. Revisit once pooler/ORM compatibility is confirmed.
- **SET SESSION AUTHORIZATION → deferred (NOT always-blocked).** Grouped with SET ROLE
  as identity-switching in the proposal and flagged by the same open question. Keeping
  it out avoids changing the error returned for it under a `read_only` grant (it stays
  `ErrReadOnlyBypassAttempt`), preserving existing behavior. Still blocked under
  `read_only` today. Candidate for a follow-up identity-switch block.
- **PG egress / code-loading in scope this pass:** `ALTER SYSTEM` (acceptance),
  `COPY ... TO/FROM PROGRAM` (acceptance, arbitrary host command execution),
  `ALTER DEFAULT PRIVILEGES` (privilege admin, consistent with always-blocking
  GRANT/REVOKE), `CREATE SERVER` and `CREATE FOREIGN DATA WRAPPER` (network egress /
  FDW, the PG analogue of Oracle's blocked `CREATE DATABASE LINK`).
- **PG egress / code-loading deferred to follow-up:** `CREATE EXTENSION` (the proposal
  itself floats it as a legitimate-for-trusted-grants control candidate; blocking it
  would break common `CREATE EXTENSION pg_trgm`-style usage) and file-access functions
  (`pg_read_file`, `lo_import`, `lo_export`, …) which are function-level (not
  statement-leading), higher false-positive risk, and not named by acceptance.

### Steps

1. **Shared error + hardening helpers** (`validation.go`): add `ErrAdminCommandBlocked`;
   add `stripLeadingComments` (strips stacked leading `/* */` and `--` comments) and a
   `classifyStatements` helper (strip-leading-comment then split on `;`, strip each
   fragment) so classification is comment- and multi-statement-aware. Refactor
   `IsWriteQuery` / `IsDDLQuery` / `IsPasswordChangeQuery` to run per-statement over
   `classifyStatements`, fixing the leading-comment bypass and the `SELECT 1; ALTER ROLE …`
   first-statement-only gap for every classifier including the password block.
2. **Cross-protocol admin block:** add `adminCommandPatterns` (`ALTER|CREATE|DROP
   ROLE|USER|GROUP`, `RENAME USER`, `GRANT`, `REVOKE`, anchored at statement start so
   `ALTER TABLE` / DML are unaffected) + `IsAdminCommand`; wire it into `ValidateQuery`
   right after the password check (same always-blocked tier) → covers MySQL + Oracle.
3. **PG blocked patterns:** add `pgBlockedPatterns` (`ALTER SYSTEM`, `COPY … PROGRAM`,
   `ALTER DEFAULT PRIVILEGES`, `CREATE SERVER`, `CREATE FOREIGN DATA WRAPPER`) +
   `IsPostgreSQLBlockedPattern` (applied per-statement).
4. **Oracle additions:** add `AUDIT` / `NOAUDIT` (statement-start-anchored to avoid
   false positives on a column named `audit`) to `oracleBlockedPatterns`. The remaining
   Oracle items in the spec (non-password `ALTER USER`, `CREATE/DROP USER`, `GRANT`,
   `REVOKE`) are covered by the cross-protocol `IsAdminCommand`.
5. **MySQL additions:** add `SET PERSIST` / `SET PERSIST_ONLY`, `INSTALL` /
   `UNINSTALL PLUGIN`, `CREATE FUNCTION … SONAME` (UDF loading), and `SHUTDOWN`
   (statement-start-anchored) to `mysqlBlockedPatterns`. The user/grant admin items are
   covered by `IsAdminCommand`.
6. **PG wiring** (`intercept.go` + `errors.go`): add PG-local `ErrAdminCommandNotAllowed`
   and `ErrRestrictedCommandNotAllowed`; in `handleQuery` and `handleParse`, after the
   password check, return them when `shared.IsAdminCommand` / `shared.IsPostgreSQLBlockedPattern`
   match (unconditional, before the grant-control checks). `readOnlyBypassPatterns` is
   left untouched so SET ROLE / SET SESSION AUTHORIZATION keep their read-only behavior.
7. **Tests:** extend `validation_test.go` with per-protocol / per-pattern admin cases,
   comment-prefixed and multi-statement variants, and "ordinary DML/reads still pass"
   guards; update the two existing tests whose asserted behavior the spec deliberately
   changes (`TestValidateQuery_ReadOnly_BlocksWrites` GRANT/REVOKE now admin-blocked;
   `TestValidateQuery_CommentBypass` now blocks the stripped INSERT). Update the PG
   `TestHandleQuery_BlocksPasswordChange` `ALTER USER … WITH LOGIN` case (now admin-blocked)
   and add PG handleQuery/handleParse admin + pattern coverage.

### Known limitations (consistent with the existing heuristic approach)

Classification is prefix/pattern-based, not a full SQL parse. A semicolon embedded
inside a non-leading comment or a string literal can still split a statement oddly, and
SQL smuggled inside a string (`EXECUTE IMMEDIATE 'GRANT …'`) is not inspected. These
match the pre-existing behavior of the read-only / DDL / password heuristics and are
out of scope for this pass; the failure mode is conservative (block) for the literal
cases the patterns do see.
