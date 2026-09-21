---
model: opus
effort: medium
---

# Cursor-id learning latches a value out of row bytes, and a live sqlplus session was measured holding 17744

## Goal

Stop `learnCursorID` from latching an id its anchored scan found inside
row-stream bytes, so the id a re-execution is gated against is the one the
server actually assigned.

## Why

`midFetchOERNamesTheStreamingCursor`'s own doc already spells out the caveat:
`learnCursorID` runs on **every** upstream packet and latches only once it has
succeeded, so for a statement whose id is never learned from its own end-of-call
OER, `findPlausibleOERInResponse`'s anchored scan keeps running over row-stream
bytes for the whole fetch. That was written as a known risk. It is now a
**measurement**: driving `TestIntegration_OCIRowCaptureCarriesRealColumnNames`
against a real Oracle Free 23ai server, a sqlplus fetch whose end-of-data
terminator correctly reported cursor **2** ran on a session that held **17744**
for that same fetch. 17744 is inside `cursorReexecMaxID` (0xFFFF) and so passes
every bound the scan applies; nothing about it is distinguishable from a real id
after the fact.

That id is not inert. It is what `rememberCursor` files the statement under, so
it is what a later re-execution naming a cursor resolves against — the tracker
entry for the real id 2 is simply never written, and an id the server *does*
recycle onto 17744 later would resolve to the wrong statement. It is also the
reference `midFetchOERNamesTheStreamingCursor` compares a mid-fetch diagnostic
to, which is how a genuine ORA text can be dropped. The status path stopped
depending on it in `2026-09-20-02` (see `statusOERMayEndTheCall`) precisely
because the reference could not be trusted, but the two paths that still depend
on it were left as they were.

## Implementation

1. Measure first, the way the row-stream work did. `TestIntegration_CursorIDLearningMissRate`
   already replays learning live; extend it (or add a sibling) to record, per
   statement, the id learned and **where in the packet** the accepted OER sat —
   byte 0 versus a scan hit — and whether a row stream was open at the time.
   The corpus replay (`walkMidStreamOERs` in `midfetch_fail_replay_test.go`) can
   ask the same question offline across all 33 recordings.
2. The likely bound is the one the status predicate already uses: do not learn a
   cursor id from a packet that arrives **while `rowStreamActive()` is true**,
   since by then the server has already reported the id once. Check against the
   measurement whether any statement's id is *only* ever available mid-stream —
   if so, that shape needs its own rule rather than the blanket refusal.
3. A second, cheaper bound worth measuring alongside: the OCI terminator reports
   the cursor at a fixed offset in a block the RetCode anchor validates, so a
   byte-0 fixed-width status is a *better* source than a scan hit and could be
   preferred over one.
4. Whatever lands, re-run the OCI integration suite — the 17744 case is
   reproducible there, and `logMsgLearnedCursorID` carries the id.

## Files

- `internal/proxy/oracle/intercept.go` — `learnCursorID`, `rememberCursor`
- `internal/proxy/oracle/ttc_oer.go` — `findPlausibleOERInResponse`,
  `plausibleStatusOER`, `cursorReexecMaxID`
- `internal/proxy/oracle/session.go` — `midFetchOERNamesTheStreamingCursor`,
  `statusOERMayEndTheCall` (the path that stopped trusting the id)
- `internal/proxy/oracle/cursor_learning_integration_test.go` — the live
  measurement
- `internal/proxy/oracle/oci_column_names_integration_test.go` — where 17744 was
  observed
