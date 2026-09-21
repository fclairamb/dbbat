---
model: opus
effort: medium
---

# The cursor-id scan still reads describe records, and a fixed-width session is still offered the compressed encoding

## Goal

Tighten the two remaining places `findCursorIDInResponse` accepts bytes that
cannot be a server OER, both surfaced by the measurement in
`2026-09-21-05` (`specs/done/`… once archived) rather than argued for.

## Why

`2026-09-21-05` ranked the evidence behind a learned cursor id
(`cursorIDSource`) so the server's own end-of-call object outranks an anchored
scan, and that is what corrects the live **17744** latch to the real cursor 2.
The ranking closed the gating hazard; it did not make the weak reading correct,
and the measurement named two reasons it is weaker than its rank suggests.

**1. `cursorIDFromScan` is not "outside a row stream" in the sense the codebase
assumes.** The argument everywhere else (`decodeErrorOER`, `handleResponse`) is
that outside a row stream a payload cannot be row bytes. True, but the packet
`learnCursorID` reads first on a fetch is the **QueryResult**, and
`learnCursorID` runs *before* `handleQueryResultV2` — so its payload is the
server's **describe records**, which decode as seven bounded ints just as
happily as row data does. That is literally where 17744 came from, live, and it
was rated `scan`. Between that packet and the terminator the wrong id is in the
tracker; no re-execution can arrive in that window today (the client is
fetching), which is why the ranking was enough, but the window is a property of
client behaviour rather than of the code.

**2. A session known to speak fixed-width is still offered the compressed
reading.** `decodeOERFixedFieldsAt` refuses a fixed-width layout a learned
*compressed* shape did not ask for, deliberately ("a client known to speak
compressed integers is never scanned for a fixed-width block"). The converse is
not enforced: `locatePlausibleOER` tries `decodeOERFieldsAt` first,
unconditionally, at every offset, on every session. A server speaks one
encoding, so on a learned fixed-width session a compressed acceptance is by
construction not a server OER. Measured across all 33 recordings: 22 ids are
learned on fixed-width sessions and **none** of them is read as compressed, so
the tightening costs nothing there — but it was measured only as a by-product,
not aimed at.

## Implementation

1. Measure first, as `2026-09-21-05` did. Extend
   `TestDumpReplay_CursorIDLearningSource` to record the **TTC function code of
   the packet** each id was learned from, so "learned off a QueryResult" is a
   figure rather than an inference. Check the same live for the JDBC/dbeaver
   shape, whose one `mid_stream_scan` id is the corpus's only mid-stream learn.
2. Add a rank below `cursorIDFromScan` for a scan hit on a packet that is not a
   call boundary at all — a QueryResult's describe records. Do **not** refuse
   it: the 17744 session's real id would not have been learned any earlier
   without it, and a client that re-executes before its terminator arrives would
   meet `refuseUnknownCursor`. The point is only that a later reading of any
   other kind must outrank it, which today it does not (a second scan hit
   cannot).
3. Make the encoding check symmetric in `locatePlausibleOER`: when
   `shape.tailLearned && shape.fixedWidth`, skip `decodeOERFieldsAt` entirely.
   Note this locator is shared with `handleResponse`'s out-of-stream
   fall-through, so the corpus measurement has to cover completion as well as
   learning — `TestDumpReplay_MidStreamOERFalsePositiveRate` and the
   per-client completion tests are the guard.
4. Re-run the live OCI suites. `TestIntegration_OCIRowCaptureCarriesRealColumnNames`
   already asserts the final ids and that the call boundary was reached; if step
   2 lands, 17744 should be rated below `scan` there, which the log will say.

## Files

- `internal/proxy/oracle/ttc_oer.go` — `cursorIDSource`, `findCursorIDInResponse`,
  `locatePlausibleOER`, `decodeOERFixedFieldsAt`
- `internal/proxy/oracle/intercept.go` — `learnCursorID`
- `internal/proxy/oracle/cursor_learning_source_replay_test.go` — the corpus measurement
- `internal/proxy/oracle/oci_column_names_integration_test.go` — the live 17744 scenario
- `docs/oracle.md` — "Learning the cursor id"
