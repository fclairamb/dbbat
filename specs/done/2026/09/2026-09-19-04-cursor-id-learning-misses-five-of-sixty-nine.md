---
model: opus
effort: medium
---

# `TestIntegration_CursorIDLearningMissRate` fails on `main`: 5 of 69 re-executions name an unknown cursor

## Goal

Get the cursor-id learning miss rate back to zero — or, if the misses are
structural, change the assertion to say what is actually guaranteed and record
why the remainder cannot be learned.

## Why

Found on 2026-09-19 while QA-ing
`2026-09-19-03-oracle-ojdbc6-auth-fails-through-the-proxy.md`. A full
`make test-e2e-oracle` run (with `ORACLE_TEST_OJDBC_JAR` and `OJDBC6_JAR` set,
under `-race`, 893s) came back with exactly two failures, and both are the same
one, on both thin clients:

```
--- FAIL: TestIntegration_CursorIDLearningMissRate (18.70s)
      re-executions naming an unknown id:   5
      learning miss rate:                   5/69
      parses with no cursor id learned:     DROP TABLE dbbat_learn_probe |
        SELECT * FROM dbbat_no_such_table_at_all | (x3)
--- FAIL: TestIntegration_CursorIDLearningMissRate_PythonThin (20.90s)
      learning miss rate:                   5/69
```

**It is not a regression from that spec.** The identical 5/69 reproduces at
`d8ae7785` — the pre-dispatch HEAD, before any of the pre-v315 AUTH work — in a
detached worktree, on both tests. The ojdbc6 changes are gated on
`session.tnsLegacyLength`, which is false for every v315 client, so both tests
run byte-identical code either way; the worktree run confirms that rather than
assuming it.

So `main` has two red integration tests, and they have been red for at least
that long. The assertion is load-bearing: its own message says a miss would
become an `ORA-01031` the day the piggyback path fails closed, so "5 of 69
re-executions are unidentifiable" is a real statement about enforcement, not a
test detail.

The parses that never yield a cursor id are the interesting part: four of the
five are statements that *failed* (`SELECT * FROM dbbat_no_such_table_at_all`,
three times) or a `DROP TABLE`. A statement the server refused may never
allocate a cursor at all, in which case the re-executions naming an unknown id
are a different frame than the counter assumes, and the miss is in the
*accounting* rather than in the learning.

## Implementation

1. Reproduce: `go test -race -tags integration -timeout 40m -count=1 -run
   'TestIntegration_CursorIDLearningMissRate$' ./internal/proxy/oracle/`. It is
   ~20s plus an Oracle container.
2. Find out what the 5 unknown-id re-executions actually are. `logcapture_test.go`'s
   handler already records `logMsgUntrackedCursorForwarded` with `cursor_id`;
   correlate those ids against the parses in the same session and against the
   close-cursors frames (`cursorsTracked` peaked at 3 and ended at 1, which the
   test already prints).
3. Decide which of the two it is:
   - **learning gap** — `learnCursorID` misses a shape the upstream sends (see
     docs/oracle.md, "Learning the cursor id"), in which case fix it; or
   - **accounting** — a failed parse allocates no cursor, so a client that
     re-executes anyway is naming an id that never existed. Then the denominator
     is wrong, and the assertion should exclude those rather than be relaxed:
     `refuseUnknownCursor` already fails closed for them under a restrictive
     grant, which is the behaviour that matters.
4. Whichever it is, the test must end green and its message must be true. If the
   remainder is structural, say so in `docs/oracle.md` next to the learning
   notes, with the count and the reason.

## Files

- `internal/proxy/oracle/cursor_learning_integration_test.go` (both tests, and
  the workload in `runCursorWorkloads`)
- `internal/proxy/oracle/intercept.go` — `refuseUnknownCursor`, `regateCursor`
- `docs/oracle.md` — "Cursor re-execution", "Learning the cursor id"
