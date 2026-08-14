# An OCI client's failures are recorded as successes, on every shape

## Goal

Teach dbbat's OER **decoder** the fixed-width OCI/sqlplus encoding it already
knows how to *write*, so a statement that fails on an OCI client lands in
`queries.error` the way it does on the three thin clients — and so cursor-id
learning stops going blind on those sessions.

## Why

`2026-08-13-13` recorded a mid-fetch failure on four clients and found the
fourth one uncovered. `testdata/sqlplus_midfetch_fail.pcapng` carries an
ORA-01722 as a standalone func `0x04` at byte 0 of a whole TNS Data packet,
150 packets into a row stream, with every field the mid-fetch anchors want:
read at `oerFixed32Layout`'s offsets it has call status `1` (no end-of-call bit)
at 1, ECID 166 at 5, error number 1722 at 11 and again as the RetCode at 66,
cursor id 5 at 17, call number 169 at 45, the row count 14999 compressed at 70,
and the `ORA-01722` CLR right behind it.

dbbat reads none of it. `decodeOERFieldsAt` decodes TTC **compressed** integers
only, so it returns nil for the whole block; `decodeErrorOER` refuses,
`handleOERStatus` never reaches either anchor, and `findPlausibleOERInResponse`
— which is how cursor ids are learned — is blind for the same reason, which is
why the session holds no streaming cursor id to compare the OER's against.

The blast radius is much wider than mid-fetch. The same recording opens with a
`DROP TABLE` of a missing table: an ORA-00942 raised **before the first row**,
the shape `failed_stmt_replay_test.go` and `failed_stmt_integration_test.go`
both say already works — and it is recorded with no error either. So on an OCI
client (sqlplus, SQL*Developer via OCI, Instant Client generally) *every*
failing statement is currently written to `queries` as a success. That is a
correctness hole in the audit record, not a missing nicety, and it is invisible:
the client sees its ORA text, the query row says nothing happened.

The encoder half of this has been solved and measured already
(`encodeOERFixedWidth`, `oerFixed32Layout`, `oerFixed64Layout`,
`oerFixedWidthTailFieldsAt`), including the two width variants and the offsets
each of them puts the fields at, so the decoder is not starting from an
unmeasured wire format.

## Implementation

1. Add a fixed-width sibling of `decodeOERFieldsAt` in
   `internal/proxy/oracle/ttc_oer.go`, reading the fields at `oerFixedLayout`'s
   offsets for both the 32-bit and 64-bit layouts. Anchor it the way
   `oerFixedWidthTailFieldsAt` already anchors the shape it learns — the error
   number at `errNum` must be repeated as the RetCode at `retCode`, which is
   what makes a prefix length self-validating — rather than trusting a length.
   Mid-row-stream this anchor is doing the job `diagnosticNamesCode` does on the
   compressed path, so keep **both**: the tail must still spell the code its
   fields report.
2. Decide which layout to try, and prefer *not* guessing. The session already
   learns `oerShape.fixedWidth` / `fixedWidth64` from the upstream's own OERs
   (`session.learnOERShape`), so the decode path can ask the session rather than
   sniffing. Note the ordering trap: the shape is learned from a **server** OER,
   so the first OER of a session may arrive before anything is learned —
   `readUpstreamAuthMessages` is what makes that mostly moot, and the fallback
   should be to try both layouts under the RetCode anchor rather than to accept
   the wrong one.
3. Route it: `decodeErrorOER` (so `handleOERStatus` records ORA text) and
   `findPlausibleOERInResponse` (so cursor ids are learned, which is what the
   mid-fetch anchor compares against). Getting cursor learning working is what
   turns `midFetchOERNamesTheStreamingCursor` from "fails closed on OCI" into a
   real check there.
4. Tests, in this order:
   - `internal/proxy/oracle/midfetch_fail_replay_test.go` already holds the
     measurement. `TestDumpReplay_OCIMidFetchFailureIsFixedWidthAndUnreadable`
     pins today's refusal and says in its failure message what to do when it
     starts passing the other way; `TestDumpReplay_OCIFailuresAreNotRecordedAtAll`
     pins the ORA-00942 half. Both must be **rewritten, not deleted**: the field
     values they assert are the fixture the decoder has to reproduce.
   - move `sqlplusMidFetchDump` into `midFetchDumps()`, which puts it through
     `TestDumpReplay_MidFetchFailureIsABitLessStandaloneOER` (cursor anchor
     included, expecting cursor 5) and both `RecordsItsORAText` tests, and widen
     the accepted count in `TestDumpReplay_MidStreamOERFalsePositiveRate` from 3
     to 4.
   - add an OCI case to `failed_stmt_integration_test.go`, live: the existing
     `startOracleThroughProxyForOCI` fixture in `oci_client_integration_test.go`
     already stands up a sqlplus client for exactly this kind of end-to-end
     claim.
5. Update the "A failure raised mid-fetch" section of `docs/oracle.md` — the
   OCI subsection there is written as a description of a known gap, and the
   client table's `never learned` cell is the thing to correct.

No GitHub issue filed (automation does not run `gh issue create` — see
`specs/todos/2026-08-11-06-*.md`); one should be filed by hand.
