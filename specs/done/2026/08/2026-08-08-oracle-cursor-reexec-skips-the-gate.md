# Oracle: re-executing a cached cursor skips the approval gate and the static controls

**No GitHub issue filed yet — one should be.**

## Goal

Make an Oracle re-execution that carries no SQL text — the client re-runs a
cursor it already parsed — go through `shared.ValidateOracleQuery` and
`holdIfNeeded` against the SQL that cursor is *known* to hold, instead of being
forwarded ungated.

## Why

Found while fixing
`specs/todos/2026-08-08-oracle-jdbc-exec-bypasses-approval-holds.md` (the JDBC
exec op), auditing every Oracle op that can put a statement on the wire. That
one was the same mechanical fix as its siblings; this one is not, so it is
split out.

Two related paths in `internal/proxy/oracle`:

1. **`OALL8` with an empty SQL length.** `decodeOALL8`
   (`internal/proxy/oracle/ttc_decode.go:411`) returns `ErrEmptySQL` when
   `sqlLen == 0`. `handleOALL8` (`intercept.go:59-64`) treats *any* decode
   error as "don't block on decode failure — let it pass through" and returns
   `nil`, so `interceptClientMessage` forwards the packet. The cursor id was
   decoded successfully two fields earlier and is thrown away with the error.
   The session already knows that cursor's SQL: `s.tracker.cursors[cursorID]`
   was populated when the statement was first parsed.

2. **`OFETCH` against a tracked cursor.** `handleOFETCH`
   (`intercept.go:315-321`) starts a fresh `pendingQuery` when none is in
   flight, with the comment "re-execution of cursor". That new pending query is
   persisted (via `captureRow` → `persistQueryRecord`, or `completeQuery`) and
   never re-validated.

The practical effect is that an approval decision, and every static control,
apply to the *parse* rather than to each execution. A client that parses once
and re-executes many times inside one session gets one hold and then a free
run. Under `read_only`/`block_ddl` the exposure is smaller (the statement was
refused at parse time, so there is no cursor to re-execute), but it is not zero:
a control that becomes relevant mid-session — a grant revoked, a quota crossed —
is enforced by `checkQuotas`, while the *statement-shaped* controls are not
re-evaluated at all.

**This needs protocol verification before it is implemented.** How often real
Oracle clients (JDBC thin, `go-ora`, `python-oracledb`, OCI/sqlplus, SQLcl,
DBeaver) actually re-execute by cursor id without resending the SQL is not
established here — it is inferred from the shape of the code, not observed on a
wire capture. If the answer is "essentially never", this is a hardening task
rather than a live hole, and the fix should say so.

## Implementation

- `internal/proxy/oracle/ttc_decode.go`: give `decodeOALL8` a way to report
  "well-formed, cursor id `N`, no SQL text" distinctly from a genuine decode
  failure — either a sentinel error carrying the cursor id, or a result whose
  `SQL` is empty and `CursorID` valid. Do not widen the "pass through on decode
  failure" rule; the point is to stop this case from *being* a decode failure.
- `internal/proxy/oracle/intercept.go`: in `handleOALL8`, when that case is
  reported and `s.tracker.cursors[cursorID]` is known, run
  `shared.ValidateOracleQuery` + `holdIfNeeded` on the tracked SQL before
  forwarding, exactly as the SQL-carrying path does. When the cursor is
  *unknown*, decide explicitly and document it — forwarding an unidentifiable
  execution under a restrictive grant is the same fail-open shape the SQL Server
  proxy chose to refuse (`ErrUnknownPreparedStatement`,
  `internal/proxy/mssql/`), and that precedent is probably the right one here.
- `handleOFETCH`: decide whether a fetch that starts a *new* pending query is a
  re-execution that should be re-gated, or merely more rows of the statement
  that was already approved. If it is the former, gate it on
  `cursor.sql`; if the latter, say so in a comment so the next audit stops here.
- Tests, under the default build (no Oracle container needed), next to
  `internal/proxy/oracle/jdbc_exec_gate_test.go`: parse a statement that an
  approval pattern matches, release the hold, then re-execute the same cursor
  with a SQL-less `OALL8` and assert it is held again. Same shape for
  `block_ddl`.
- Capture a real cursor-reuse exchange into `internal/proxy/oracle/testdata`
  first if one can be produced — `docs/oracle.md` documents the harness
  (`make test-e2e-oracle`) and the dump-per-use-case fixture convention.
- Re-check the Oracle caveat in `docs/approvals.md` ("The hold, in order")
  afterwards: it currently documents this gap as known.

## Resolved open questions

**Q: "When the cursor is *unknown*, decide explicitly and document it —
forwarding an unidentifiable execution under a restrictive grant is the same
fail-open shape the SQL Server proxy chose to refuse."**

**Decision: refuse only under a restrictive grant.** When a SQL-less `OALL8`
names a cursor id that is not in `s.tracker.cursors`:

- If the session's grant carries **statement-shaped controls** — a non-empty
  approval-pattern set, `read_only`, or `block_ddl` — **refuse** the execution
  (fail closed) with a TTC error, in the same shape `handleJDBCExec` uses for a
  refused statement. A restrictive grant must not be bypassable by an execution
  the proxy cannot identify.
- If the grant carries **none** of those controls (or there is no grant), **forward**
  the frame as today, and log at WARN with the cursor id.

Rationale: an untracked cursor is not necessarily an attack — the proxy may have
attached mid-session, the tracker may have evicted the entry, or the client may
have reconnected — so refusing unconditionally would break permissive sessions
for no security gain. Refusing exactly where a statement control exists keeps the
guarantee that matters without that blast radius. Document this asymmetry in
`docs/approvals.md` next to the existing Oracle caveat; it is a deliberate
trade-off, not an oversight.

**Q: "`handleOFETCH`: decide whether a fetch that starts a *new* pending query is
a re-execution that should be re-gated, or merely more rows of the statement that
was already approved."**

**Decision: re-gate it.** Treat the no-query-in-flight branch as a genuine
re-execution and run `shared.ValidateOracleQuery` + `holdIfNeeded` against the
tracked `cursor.sql` before forwarding. The justification is the code's own
behaviour: that branch already persists a **distinct query row**, so anything
recorded as its own query must be gated as its own query, or the audit trail and
the enforcement disagree.

A fetch that continues a query already in flight is **not** affected — only the
branch that starts a fresh `pendingQuery` gates. Do not hold mid-result-set.

**Q: "This needs protocol verification before it is implemented" — how often real
Oracle clients re-execute by cursor id without resending SQL is inferred from the
shape of the code, not observed on a wire capture.**

**Decision: implement now with synthesized frames; do not block on a capture.**
Build the fix and cover it with hand-built TTC frames under the **default build**
(no Oracle container, no `make test-e2e-oracle`), as the spec's own test bullet
already asks. The hardening is worth having whatever the real-world frequency
turns out to be.

State plainly in `docs/approvals.md` (and in the final report) that the
real-world re-execution rate is **unmeasured** — this is hardening against a
shape the code permits, not a response to an observed exploit. Capturing a real
cursor-reuse exchange into `internal/proxy/oracle/testdata` remains worth doing
and should be filed as its own follow-up in `specs/todos/`, not folded in here.
