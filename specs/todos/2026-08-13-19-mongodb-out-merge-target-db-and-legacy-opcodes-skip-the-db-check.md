# MongoDB: `$out`/`$merge` with an explicit target `db`, and legacy opcodes, skip the `$db` check

**No GitHub issue filed yet — one should be.** (Automation must not run
`gh issue create`; see `specs/todos/2026-08-11-06-*.md`.)

## Goal

Close the two paths on the MongoDB proxy that reach a database other than the
one the grant covers without ever presenting a disallowed `$db`.

## Why

Found while auditing all five protocols for the Oracle `ALTER SESSION SET
CONTAINER` escape
(`specs/todos/2026-08-13-14-alter-session-set-container-escapes-a-full-write-grant.md`;
the MySQL and SQL Server holes from the same audit are
`specs/todos/2026-08-13-18-*.md`). MongoDB is the one protocol with real
per-statement database enforcement — `handleClientOpMsg`
(`internal/proxy/mongodb/intercept.go`) reads `$db` off every OP_MSG and
`mongoDatabaseAllowed` (`internal/proxy/shared/validation.go`) refuses anything
but the session's server row — so the main path is sound. Two things go around
it:

1. **`$out` / `$merge` with an explicit target database.**
   `aggregatePipelineWrites` (`internal/proxy/shared/validation.go`) looks for the
   stage key only, to *reclassify* the aggregate as a write. It never inspects the
   stage's value. So under any grant that is not `read_only`,
   `{$merge: {into: {db: "other", coll: "x"}}}` — or the equivalent `$out` form —
   writes into a database the grant does not cover, while `$db` on the message is
   the granted one and passes the check honestly. The write is attributed to the
   granted database in `queries`, which carries no database column of its own.
2. **Non-OP_MSG opcodes are forwarded verbatim after auth**
   (`internal/proxy/mongodb/intercept.go`), so a hand-crafted legacy `OP_QUERY`
   against `otherdb.$cmd` never reaches `ValidateMongoCommand` at all — no `$db`
   check, no read_only check, no logging shaped like a statement. This one only
   bites against MongoDB < 5.1, which still serves legacy opcodes; ≥ 5.1 refuses
   them upstream. That makes it lower severity, not a non-issue: dbbat does not
   choose the upstream version.

## Implementation

- Extend the `$out`/`$merge` inspection so it returns the target *database* as
  well as "this writes". `$out` takes either a collection name (same db) or
  `{db, coll}`; `$merge`'s `into` takes either a string or `{db, coll}`. When a
  `db` is present and is not the session's server row, refuse — reuse
  `ErrMongoDatabaseBlocked` so the client sees the same Unauthorized (13) errmsg
  as any other cross-database attempt. Keep the write reclassification as it is;
  this is an additional refusal, not a replacement.
- Do it in `internal/proxy/shared/validation.go` next to
  `aggregatePipelineWrites`, so the decision lives beside `mongoDatabaseAllowed`
  rather than in the proxy — `ValidateMongoCommand` already receives the
  `*store.Server`, so no signature change is needed.
- Legacy opcodes: decide explicitly rather than by omission. The cheap and
  defensible answer is to **refuse** any non-OP_MSG opcode post-auth (OP_QUERY,
  OP_GET_MORE, OP_INSERT/UPDATE/DELETE, OP_KILL_CURSORS) with a clear error,
  since a modern driver against a supported server never sends them; the
  alternative — parsing them so they can be validated — is a lot of wire code for
  a path MongoDB itself removed. Whichever is chosen, record it in
  `docs/mongodb.md` under the existing contract sections.
- Tests: `internal/proxy/shared/validation_test.go` for the pipeline check (pin
  both `$out`/`$merge` string and `{db, coll}` forms, the same-database form
  staying *allowed*, and that the refusal is independent of grant controls);
  `internal/proxy/mongodb/` for the opcode decision.
- `docs/mongodb.md`: the contract text says every command's `$db` is checked;
  amend it to state what a pipeline stage's own `db` does, which is the thing a
  reader currently has no way to guess.
