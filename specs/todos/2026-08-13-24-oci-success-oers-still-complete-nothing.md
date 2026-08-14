# An OCI client's *successful* standalone OER still completes nothing

## Goal

Teach the two decode paths that still demand the TTC end-of-call bit —
`decodeOERAt` and, through it, `findOERInResponse` — to read the fixed-width
OCI summary object, or decide deliberately that they must not. Today an OCI
session's successful call is completed by the *next* statement's
`flushPendingQuery` rather than by its own OER.

## Why

`2026-08-13-17` gave the decoder the fixed-width encoding
(`decodeOERFieldsAtLayout`) and routed it into `decodeErrorOER` and
`findPlausibleOERInResponse`, which is what stopped an OCI client's **failures**
from being recorded as successes. Two paths were deliberately left on the
compressed-only reading, and each leaves a smaller version of the same hole:

- `handleOERStatus`'s first branch is `decodeOERAt(ttcPayload, 0)`, which
  returns nil for a fixed-width block. A standalone OER reporting **success or
  ORA-01403** therefore completes nothing on an OCI session — the statement stays
  pending until the next one flushes it, so it records no `rows_affected` and a
  `duration_ms` that measures the client's think time. That is exactly the
  symptom the "OER end-of-call bit is not universal" fix removed for
  python-oracledb thin (one live UPDATE logged at 74 s), still present on OCI.
  `testdata/sqlplus_midfetch_fail.pcapng` packet #20 is one: call status 1,
  ORA-01403, cursor 1, read fixed-width.
- `handleResponse`'s **mid-row-stream** branch calls `findOERInResponse`, which
  is `decodeOERAt` at every offset. An OCI OER embedded in a Response mid-fetch
  is invisible to it. (Outside a row stream the `findPlausibleOERInResponse`
  fall-through already covers the fixed-width case, so DML row counts on OCI do
  land.)

The reason this was not folded into `-17` is that the bit is not decoration on
these paths: `decodeOERAt` demands it *because* a status OER carries no
diagnostic to prove itself with, and both of these run where a `0x04` lead byte
can be a row value's length prefix. So this is a judgement call about what
stands in for the bit, not a missing decoder call.

## Implementation

1. Decide what the fixed-width equivalent of the bit is. The candidate is the
   anchor `decodeOERFieldsAtLayout` already applies — the error number repeated
   as the RetCode at the layout's offsets, plus a non-zero call status, plus the
   prefix length — which is a far stronger structural claim than any single bit,
   and which `TestDecodeOERFieldsAtLayout_KeepsItsAnchors` already measures the
   refusals of. Note the fixed-width call status *does* carry the bit when the
   call sets it (`0x10001` on the ORA-00942 in the sqlplus recording), so
   demanding it there is also an option — and is the conservative one.
2. Route the choice into `decodeOERAt` (hence `findOERInResponse`), which means
   both grow an `oerShape` parameter the way `decodeErrorOER` did.
3. Measure the false-positive cost the way `-17` did rather than arguing it:
   `TestDumpReplay_MidStreamOERFalsePositiveRate` already replays the whole
   `testdata/` corpus through a real session and prints the numbers; the
   equivalent stress figure for the *strict* predicate is what this needs.
4. Tests: a replay assertion that the sqlplus recording's successful statements
   complete on their own OER (its `SELECT ... FROM SYS.DUAL` login probe and the
   `CREATE TABLE`/`INSERT` are all in there), and a live one — the OCI fixture in
   `oci_client_integration_test.go` running a DML and asserting `rows_affected`,
   next to `TestIntegration_FailingStatementsRecordTheirORAErrorOCI`.
5. Update the bit table in the "the OER end-of-call bit is not universal" section
   of `docs/oracle.md`, whose first and fourth rows are the two paths above.

No GitHub issue filed (automation does not run `gh issue create` — see
`specs/todos/2026-08-11-06-*.md`); one should be filed by hand.
