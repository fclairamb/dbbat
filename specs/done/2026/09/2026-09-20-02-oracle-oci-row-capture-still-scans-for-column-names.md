---
model: opus
effort: medium
---

# OCI row capture still guesses column names, because turning the describe reader on puts the session in a row stream it is not ready for

## Goal

Let `handleQueryResultV2` read an OCI session's describe records instead of
falling back to `scanAndPadColumnNames`, without the row-stream bookkeeping that
follows from it ending calls mid-fetch.

## Why

`parseColumnDescribes` can already read the fixed-width records — that landed
with `2026-09-19-06-oracle-refcursor-id-in-the-oci-encoding.md`, and
`TestOCIDescribeRecordsParse` holds it to real values: an eight-column describe
recorded from sqlplus comes back with every name and every type, including a
`VARCHAR2(4000)`, a `-127`-scale float and an object column whose 16-byte type
OID is the only non-null `toID` in the corpus. What did **not** land is the one
word that uses it: `handleQueryResultV2` passes `false` rather than
`s.oerShapeSnapshot().fixedWidth`, so an OCI session's column names still come
from the heuristic scanner, which pads and guesses where a record spells it out.

Passing the real flag was tried and reverted, because it is not a cosmetic
change. An OCI session whose describes parse **learns its columns**, which puts
it in a row stream (`rowStreamActive`) over packets it used to walk straight
past — and six of those packets in the existing corpus lead with a `0x04` that
`decodeOERAt` accepts. `TestDumpReplay_MidStreamOERFalsePositiveRate` catches all
six and says what they would mean in production: a summary object accepted
mid-fetch ends the call. Reading the records right is not the same thing as the
row-stream bookkeeping being ready for it.

## Implementation

1. Start from the failing measurement, not from the flag: flip
   `handleQueryResultV2` to `s.oerShapeSnapshot().fixedWidth` and run
   `TestDumpReplay_MidStreamOERFalsePositiveRate`,
   `TestDumpReplay_OCIStatusOERsCompleteTheirOwnStatement`,
   `TestDumpReplay_MidFetchFailureIsABitLessStandaloneOER` and
   `TestDecodeFixedStatusOERAt_KeepsItsAnchors`. Those are the four that turn
   red, and what they are saying is the actual work.
2. Understand what the six accepted packets are. An OCI fetch response carries a
   genuine summary object naming the streaming cursor and its running row count
   — `midStreamBitlessStatusAcceptances` already counts 149 of those at non-zero
   offsets, which is why `decodeFixedStatusOERAt` is offered at offset 0 only.
   The question is whether a byte-0 one inside an OCI row stream is the
   *end* of the fetch (in which case the row stream should be closing, and the
   bookkeeping is what needs fixing) or a continuation (in which case the
   predicate needs the same restriction it already has off-stream).
3. Whatever the answer, it has to be measured on the corpus and not reasoned
   about: the recordings are `sqlplus_cursor_reexec.pcapng` and
   `sqlplus_midfetch_fail.pcapng`, and both already have tests that count
   rather than assert vibes.
4. Then the payoff is worth stating in a test of its own: an OCI session's
   captured `query_rows` should carry the describe's real column names. Today an
   expression column comes back padded/guessed;
   `testdata/oci_describe.hex` has a 67-character one to check against.

## Files

- `internal/proxy/oracle/intercept.go` — `handleQueryResultV2` (the one word)
- `internal/proxy/oracle/ttc_decode.go` — `decodeQueryResultV2`,
  `scanAndPadColumnNames`
- `internal/proxy/oracle/describe.go` — `parseColumnDescribes`,
  `describeColumnLayoutWide`
- `internal/proxy/oracle/midfetch_fail_replay_test.go` — the measurement that
  decides it
- `internal/proxy/oracle/testdata/oci_describe.hex`
