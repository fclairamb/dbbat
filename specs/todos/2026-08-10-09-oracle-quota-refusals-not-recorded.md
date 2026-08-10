# Record Oracle quota refusals in query history, like the other protocols

## Goal

A statement Oracle refuses because the grant's quota is exhausted — or because
the grant expired or was revoked mid-session — should leave a `queries` row
with the refusal as its `error`, exactly like a statement refused by
`read_only`, `block_copy` or `block_ddl` already does.

## Why

`2026-08-09-log-blocked-statements-pg-oracle` closed the control-refusal gap on
PostgreSQL and Oracle, so all five protocols now record a statement blocked by
a control. It left one asymmetry behind, deliberately, because quota was
outside its literal goal:

- **PostgreSQL** records quota refusals — `checkQuotas` failures go through
  `refuse` / `recordBlockedQuery` in
  `internal/proxy/postgresql/intercept.go` (`handleQuery`, `handleExecute`).
- **MySQL/MariaDB** records them too — `checkQuotas` → `recordQuery(..., &errStr)`
  in `internal/proxy/mysql/intercept.go`.
- **Oracle does not.** Two call sites refuse without recording:
  - `gateStatement` in `internal/proxy/oracle/session.go` (~line 1331): the
    shared pre-flight for OALL8, the v315+ piggyback exec and the JDBC thin
    driver's exec. `checkQuotas()` fails → `sendOracleError(err)` → the packet
    is dropped, and nothing is written.
  - `handleOFETCH`'s own quota check in `internal/proxy/oracle/intercept.go`
    (~line 540), on the cursor-re-execution branch.

The same class of event — an attempt the grant did not permit — is therefore
evidence on four protocols and a log line on the fifth. That is the exact
inconsistency the parent spec set out to remove.

No GitHub issue yet — file one when picking this up.

## Implementation

The recorder already exists: `s.recordBlockedQuery(sql, binds, refusal)` /
`s.refuseStatement(...)` in `internal/proxy/oracle/intercept.go`. The work is
getting the SQL to it, and that is why this was not folded into the parent
spec:

- `gateStatement` runs **before** the op's handler, so at quota-check time the
  TTC payload is still undecoded — each op decodes differently
  (`decodeOALL8`, `decodePiggybackExecSQL`, `decodeExecSQL`,
  `decodeCursorReexec`). Options: give `gateStatement` a per-op SQL extractor,
  or move the quota check inside each handler, after the decode and before the
  static controls. The second is cleaner but has to preserve today's
  fail-behaviour on a decode failure (currently: forward, don't block — see the
  Oracle caveat in `docs/approvals.md`), and has to keep the quota check ahead
  of the approval hold so an over-quota statement is never parked on a human.
- `handleOFETCH`'s check sits before the cursor lookup; the SQL is only known
  after it. Moving it after would change what an *unknown* cursor gets
  (`refuseUnknownCursor` vs the quota error), so decide that ordering
  deliberately rather than by accident.
- Whatever shape it takes, keep the recording out of the continuation-fetch
  path: refusing (and recording) mid-result-set is what the comment above that
  check exists to prevent.

## Tests

`internal/proxy/oracle/blocked_persist_test.go` is the pattern — a recorder
store for the shape, and the real-store test for the query-chain append. Add a
quota-exhausted grant case to it.
