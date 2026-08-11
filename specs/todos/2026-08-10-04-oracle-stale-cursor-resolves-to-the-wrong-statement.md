---
model: opus
effort: high
---

# Oracle: a stale tracker entry makes a recycled cursor id gate the wrong statement

**No GitHub issue filed yet — one should be.**

## Goal

Stop a cursor id that Oracle has recycled from resolving to the statement that
*used* to hold it. Today a re-execution naming a recycled id is gated, logged in
`/queries` and matched against approval patterns as whatever SQL the stale
`trackedCursor` carries — silently, with no error anywhere.

## Why

Found while measuring cursor-id learning for
`specs/done/2026/08/2026-08-09-oracle-piggyback-reexec-unknown-cursor.md` (see its
`## Measurement` section). While `findCursorIDInResponse` had an over-tight bound
on the OER sequence number, learning stopped part-way through every session — and
the failure was invisible precisely *because* of this: instead of an untracked
cursor and a WARN, the re-executions found a stale entry and resolved to the
wrong statement. Five runs of `SELECT 1 AS n FROM dual` were recorded and gated
as `SELECT 35 AS churn FROM dual`.

That specific learning bug is fixed, so the mis-resolution is currently latent
rather than firing. It is still one missed response away from returning, and it
fails **open** in the worst way: a restrictive grant is enforced against a
statement the client is not running, which can both let the real statement
through and hold an innocent one.

Two things keep the stale entries around:

- **`s.tracker.cursors` is never evicted** except by `handleOCLOSE`. A long
  session accumulates an entry per statement, which is also an unbounded map on a
  connection that may live for hours.
- **`handleOCLOSE` only removes one cursor per frame.** It reads a single id at
  `ttcPayload[2]` (`internal/proxy/oracle/session.go`, the `IsPiggybackClose`
  branch), but Oracle's piggyback close op carries a *count* followed by an array
  of ids — go-ora and python-oracledb both batch their closes. The measurement
  observed closed cursors still sitting in the tracker afterwards, which is
  exactly how the stale entries survived to be re-resolved.

## Implementation

- Decode the full close list: read the cursor count and walk that many compressed
  ints, deleting each from `s.tracker.cursors`. Verify the layout against
  `internal/proxy/oracle/testdata/*_cursor_reexec.pcapng` — those captures end
  with the client closing its cursors — and pin it with a decoder unit test the
  way `decodeCursorReexec` is pinned.
- Consider making a recycled id detectable rather than silent: `learnCursorID`
  already overwrites `s.tracker.cursors[cursorID]`; logging at WARN when the
  entry it replaces held *different* SQL would make a mis-resolution window
  visible in the logs. (Overwriting is correct — the server really does reuse
  ids — so this is a signal, not a refusal.)
- Decide whether the tracker needs a cap at all once closes are handled properly.
  If it does, evicting the oldest entry is safe now that an untracked cursor
  fails closed under a statement-shaped control: the failure mode of eviction is
  a refusal, not a wrong-SQL gate.
- `TestIntegration_CursorIDLearningMissRate`'s statement-cache-churn workload is
  the natural place to assert the tracker does not grow without bound, since it
  already opens and closes 40 cursors on one session.

Key files: `internal/proxy/oracle/session.go` (`IsPiggybackClose` dispatch),
`internal/proxy/oracle/intercept.go` (`handleOCLOSE`, `learnCursorID`,
`oracleQueryTracker`), `internal/proxy/oracle/ttc_decode.go`,
`internal/proxy/oracle/cursor_learning_integration_test.go`.

## Resolved open questions

> Consider making a recycled id detectable rather than silent: … logging at
> WARN when the entry it replaces held *different* SQL.

**Decision: yes, log it.** In `learnCursorID`, when the entry being overwritten
carries SQL that differs from the incoming statement, emit a WARN naming the
cursor id and both statements (truncated). It stays a signal, never a refusal —
the server really does recycle ids, so overwriting remains the correct
behaviour. This is what makes a future mis-resolution window visible instead of
silent, which is the whole reason the original bug went unnoticed.

> Decide whether the tracker needs a cap at all once closes are handled
> properly.

**Decision: no cap.** Handling the full piggyback close list is the actual fix
for unbounded growth — a client that opens cursors also closes them, and the
measurement's leftover entries were the un-decoded batch closes, not genuine
leaks. Adding a cap on top would introduce an eviction policy whose only
benefit is memory, and every evicted entry converts a correctly-gated
re-execution into a refusal. Do not add one.

Instead, **assert the absence of growth**: extend
`TestIntegration_CursorIDLearningMissRate`'s statement-cache-churn workload (it
already opens and closes 40 cursors on one session) with an assertion that the
tracker's entry count returns to a small bound after the client's closes are
processed. That assertion is what would catch a real leak, and it is what makes
"no cap" a defensible decision rather than an untested assumption.

The full close-list decode and its pinned decoder unit test remain the primary
deliverable — everything above is secondary to that.
