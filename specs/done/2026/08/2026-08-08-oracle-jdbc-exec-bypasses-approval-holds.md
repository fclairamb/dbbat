# The Oracle JDBC exec path records a query but never runs the approval gate

## Goal

Make `handleJDBCExec` in the Oracle proxy go through the same
`holdIfNeeded` + `shared.ValidateOracleQuery` pipeline every other
statement-carrying path uses, so a statement issued over the JDBC thin
driver's dedicated exec op cannot skip an approval hold or a static control.

## Why

Found while verifying, for
`specs/todos/2026-08-07-02-unified-connection-query-feed.md`, that a held
statement blocks its connection on every protocol.

`internal/proxy/oracle/intercept.go:182-207` (`handleJDBCExec`, dispatched from
`internal/proxy/oracle/session.go:1299`) persists a query row with
`persistQueryRecord()` but calls **neither** `holdIfNeeded` **nor**
`shared.ValidateOracleQuery`. The two paths next to it do:

- `intercept.go:89` — OALL8, holds before recording.
- `intercept.go:155` — the v315+ piggyback exec, likewise.

So the JDBC thin driver's `func=0x11 / sub-op 0x69` execute path executes
without ever consulting the grant's `approval_patterns`. `docs/approvals.md`
says the gate "fails closed. There is no path through it that forwards a
statement without an explicit approval decision naming that statement's UID" —
this is a path through it.

It does *not* break the ordering property the feed relies on (it runs on the
same goroutine as the hold, so it cannot fire while one is parked), which is
why it was out of scope there. It is still an approval-gate hole and a
`read_only` / `block_ddl` hole.

**No GitHub issue filed yet — one should be.**

## Implementation

- `internal/proxy/oracle/intercept.go`: in `handleJDBCExec`, mirror the OALL8
  path — normalize the SQL, run `shared.ValidateOracleQuery`, then
  `holdIfNeeded` before anything is forwarded upstream, and only record the
  query row afterwards (or record it from the gate, as the hold path does).
- Confirm the JDBC path has a `WatchedConn` park/unpark available the way
  `intercept.go:647-648` does for OALL8; if the exec op is reached from a
  different frame, the park has to be wired there too.
- Extend the Oracle proxy tests with a JDBC-shaped exec that matches an
  approval pattern and assert it is held, plus one that matches `block_ddl`
  and assert it is refused. `docs/oracle.md` documents the harness
  (`make test-e2e-oracle`).
- Once fixed, re-check the claim in `docs/approvals.md` about the gate having
  no bypass — it is currently inaccurate for Oracle/JDBC.
