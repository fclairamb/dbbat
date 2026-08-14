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

## Implementation Plan

### What the captures actually say

Read off `internal/proxy/oracle/testdata/*.pcapng` (all 20 recordings, client
leg only), the close list is **not** where the code looks for it:

- **`03 09 …` is not a close.** It appears exactly once per recording, as the
  very last client frame, three or four bytes long: `03 09 <seq> [00]`. Message
  type `0x03` is `TNS_MSG_TYPE_FUNCTION` and function `0x09` is
  `TNS_FUNC_LOGOFF`. It carries no cursor id at all, so today's
  `s.handleOCLOSE(uint16(ttcPayload[2]))` deletes the entry keyed by the **TTC
  sequence number** — a wrong deletion on every session teardown.
- **`11 69 …` is the close list.** Message type `0x11` is
  `TNS_MSG_TYPE_PIGGYBACK` and function `0x69` (105) is
  `TNS_FUNC_CLOSE_CURSORS`. Body after the header:

  ```
  [0]    0x11            message type: piggyback
  [1]    0x69            function: close cursors
  [2]    TTC sequence number
  [3]    0x00            token byte — 23ai-era clients only
  [..]   0x01            pointer flag
  [..]   count           TTC compressed int
  [..]   count x id      TTC compressed ints
  [..]   (optional) the next TTC message in the same packet
  ```

  Evidence: `go_ora_cursor_reexec.pcapng` closes cursor **2**, the same id its
  re-executions name; `jdbc_thin_cursor_reexec.pcapng` likewise; and
  `dbeaver.pcapng` frame 70 carries `01 | 01 03 | 01 05 01 03 01 04` — **count
  3, ids 5, 3, 4** — which is the batched close the spec predicted and which
  pins the count field's position unambiguously. `go-ora` v3's own
  `defaultStmt.Close` writes exactly this (`PutTTCFunc(0x11, 0x69)`,
  `PutBytes(1, 1, 1)`, `PutInt(cursorID, 4, …)`).

  dbbat already routes `0x11`/`0x69` to `handleJDBCExec`, because clients
  staple the statement they are about to run behind the close list in the same
  packet (`… 03 5e <exec>`). That path stays exactly as it is; the close list
  is decoded *before* it, matching wire order.

- **sqlplus (OCI thick) uses the wide encoding** for the same op — an 8-byte
  pointer sentinel and little-endian 32-bit count/ids. Out of scope here (it
  never re-executes by cursor id, so a stale entry cannot mis-resolve one); a
  follow-up todo is filed.

### Steps

1. `ttc_decode.go`: add `decodeCloseCursors(ttcPayload) ([]uint16, error)`,
   strict — right message type and function, pointer flag present, bounded
   count, every id in `(0, 0xFFFF]`, enough bytes for the whole list. Anything
   else is an error and deletes nothing. `ttc.go`: `IsPiggybackCloseCursors`,
   and rename `PiggybackSubClose`/`IsPiggybackClose` to the logoff they
   actually match.
2. `session.go`: decode the close list on the `0x11` branch and delete every id
   before the exec handling runs; drop the wrong delete from the logoff branch.
3. `intercept.go`: `handleCloseCursors`; and a WARN in `rememberCursor` (shared
   by `learnCursorID` and `handleOALL8`) when the entry being overwritten holds
   different SQL.
4. Tests: a decoder unit test pinned on the real capture bytes plus malformed
   inputs, a replay test over the recordings, and the tracker-bound assertion in
   `TestIntegration_CursorIDLearningMissRate`.
5. No cap — per the resolved decision above.
