# Oracle: the OFETCH re-execution path skips the unknown-cursor refusal and the query-count quota

**No GitHub issue filed yet — one should be.**

Two independent decisions left open by
`specs/todos/2026-08-08-oracle-cursor-reexec-skips-the-gate.md`, which gated
cursor re-executions but deliberately scoped both of these out. They share a
file and a code path, so they are written up together; they can be shipped
separately.

## Goal

Decide, and then implement, what an `OFETCH` that re-executes a cursor should do
about (1) a cursor id dbbat never saw parsed, and (2) the grant's
`max_query_counts` quota. Today it refuses on neither.

## Why

### 1. The unknown-cursor asymmetry

`handleOALL8` and `handleOFETCH` now both re-gate a re-execution against the SQL
the named cursor was parsed with. They disagree on what to do when the cursor is
**not** tracked:

- A SQL-less `OALL8` routes through `refuseUnknownCursor`
  (`internal/proxy/oracle/intercept.go`): under a grant carrying
  statement-shaped controls — a non-empty approval-pattern set, `read_only`, or
  `block_ddl` — it fails closed with `ErrUnknownCursor` (`ORA-01031`), and
  otherwise forwards with a WARN.
- An `OFETCH` naming an untracked cursor logs at **debug** and forwards, under
  *any* grant, including a restrictive one.

The asymmetry is not an accident — the resolved decision in the parent spec
scoped the refusal to the SQL-less `OALL8` — but it was never argued on its own
merits. The arguments each way:

- **For symmetry (route `OFETCH` through `refuseUnknownCursor` too):** an
  execution dbbat cannot identify is the thing being refused, and the wire op
  that carries it should not change the answer. As written, a client that can
  reach the `OFETCH` path has a strictly cheaper bypass than the `OALL8` one.
- **Against:** an `OFETCH` cannot introduce a *new* statement — it can only pull
  rows from a cursor the upstream already holds — so an untracked one is much
  more likely to be dbbat having attached mid-session than an attack, and the
  blast radius of refusing is a broken client for a read that was already
  authorised. `handleOFETCH` also has no `ErrUnknownCursor` message that would
  read sensibly to a user staring at a fetch loop.

Whoever picks this up should also decide whether such a fetch deserves a WARN
instead of the current debug log, independently of the refusal question —
`docs/approvals.md` currently lists it as a known gap, and a gap nobody can see
in the logs is worse than one they can.

### 2. `max_query_counts` is not enforced on an OFETCH re-execution

`interceptClientMessage` (`internal/proxy/oracle/session.go`, the
`TTCFuncOFETCH` case) calls `handleOFETCH` directly instead of through
`gateStatement`, so `checkQuotas` never runs on the fetch path. That is
deliberate: `gateStatement` would run `checkQuotas` on *every* fetch, including
continuations, and refusing mid-result-set is exactly what the re-execution gate
was built to avoid.

The consequence is precise:

- `LimitGuard.Check` (`internal/proxy/shared/limits.go`) runs on the response
  leg and covers **revocation, the byte quota and expiry** — those are enforced.
- `MaxQueryCounts` is **not** in `LimitGuard`. It is incremented in
  `completeQuery` and checked only in `checkQuotas`
  (`internal/proxy/oracle/intercept.go`), neither of which the OFETCH
  re-execution path reaches.
- So a client that parses once and then loops `OFETCH` re-executions persists a
  distinct `/queries` row per execution (`regateCursor` → `persistQueryRecord`)
  while sailing past its query-count cap. The statement-shaped controls and the
  approval hold *do* apply to each of those executions; it is the count quota
  alone that leaks.

## Implementation

Sketch — the decisions above come first.

- **Unknown cursor.** If symmetry wins: have `handleOFETCH`'s
  `!ok` branch call `s.refuseUnknownCursor(result.CursorID)` instead of logging
  and returning nil, and extend
  `internal/proxy/oracle/cursor_reexec_gate_test.go`'s
  `TestCursorReexec_UnknownCursorFailsClosedOnlyUnderAStatementControl` to drive
  the OFETCH op through the same table. If it does not win, say so in
  `docs/approvals.md` where the gap is currently listed as open, and leave a
  comment at the branch so the next audit stops there.
- **Query quota.** Do *not* simply route `handleOFETCH` through
  `gateStatement` — that drags `checkQuotas` onto continuation fetches. Run the
  quota check inside `regateCursor` instead (or in the `handleOFETCH` branch
  that calls it), so it applies exactly where a new query row is about to be
  created and nowhere else. Then verify the same hole does not exist on the
  SQL-less `OALL8` path — that one *does* go through `gateStatement`, so it
  should already be covered, and a test pinning it is cheap.
- Either change wants a test asserting that a fetch **continuing** a query in
  flight is still never refused — `TestOFETCH_ContinuationIsNotReGated` is the
  place to extend.
- Update the Oracle gap list in `docs/approvals.md` and the `session.go` comment
  that currently documents the quota leak, once it is no longer true.

Key files: `internal/proxy/oracle/intercept.go` (`handleOFETCH`,
`refuseUnknownCursor`, `regateCursor`, `checkQuotas`),
`internal/proxy/oracle/session.go` (the `TTCFuncOFETCH` dispatch),
`internal/proxy/shared/limits.go`,
`internal/proxy/oracle/cursor_reexec_gate_test.go`, `docs/approvals.md`.

## Resolved open questions

> Decide, and then implement, what an `OFETCH` that re-executes a cursor should
> do about (1) a cursor id dbbat never saw parsed […]

**Decision — symmetry wins: fail closed.** `handleOFETCH`'s `!ok` branch must
call `s.refuseUnknownCursor(result.CursorID)` instead of logging and returning
nil, so an `OFETCH` naming an untracked cursor behaves exactly like a SQL-less
`OALL8`: refused with `ErrUnknownCursor` (`ORA-01031`) under a grant carrying a
statement-shaped control (a non-empty approval-pattern set, `read_only`, or
`block_ddl`), and forwarded otherwise. Extend
`TestCursorReexec_UnknownCursorFailsClosedOnlyUnderAStatementControl` in
`internal/proxy/oracle/cursor_reexec_gate_test.go` to drive the OFETCH op
through the same table, and remove the asymmetry from the Oracle gap list in
`docs/approvals.md` (it is now closed, not open).

> Whoever picks this up should also decide whether such a fetch deserves a WARN
> instead of the current debug log, independently of the refusal question

**Decision — raise it to WARN.** In the forwarding case (no statement-shaped
control on the grant), the untracked-cursor OFETCH logs at WARN, not debug,
matching the WARN `refuseUnknownCursor` already emits when it forwards. A gap
nobody can see in the logs is worse than one they can.

**The `max_query_counts` half was never open** — implement it exactly as the
`## Implementation` section above directs: run the quota check inside
`regateCursor` (or in the `handleOFETCH` branch that calls it), **not** by
routing `handleOFETCH` through `gateStatement`, so continuation fetches are
never re-gated. Verify the SQL-less `OALL8` path is already covered and pin it
with a cheap test, and extend `TestOFETCH_ContinuationIsNotReGated` to assert a
fetch continuing a query in flight is still never refused. Update the
`session.go` comment that currently documents the quota leak.
