---
model: opus
effort: medium
---

# A statement at Oracle's own length ceiling gets 50 bytes longer when tagged

## Goal

Decide, and then enforce, what `DBB_QUERY_TAGGING_ORACLE=user` does with a
statement that is already as long as the server will accept.

## Why

The tag from `shared.NewUserQueryTagger` is ~50 bytes, prepended to the statement
text on the wire. Everywhere else in the rewriter that growth is accounted for —
the TTC length field is re-encoded (and may widen), the CLR value may change
format, the TNS packets are re-cut to the negotiated SDU. What is *not* accounted
for is the server's own limit on how long a SQL statement may be.

So there is a band, however narrow, where a statement a client can run today
stops running the moment an operator turns the setting on, and the error comes
from Oracle rather than from dbbat — which is the hardest kind to attribute. The
`queries` row would show a statement that looks perfectly legal, because the row
holds the client's text and the tag is not in it.

Nobody has measured where that band starts. `execMaxSQLLen` (1MB) is dbbat's own
reading bound, not Oracle's parsing bound, and the two are not the same number.
The tagging integration suite's largest probe is 20KB and it runs fine, so the
ceiling is somewhere above that.

## Implementation

- **Measure first.** Extend `statement_tagging_integration_test.go` with a probe
  that walks statement length upward against a real 23ai (and, via
  `ORACLE_TEST_IMAGE`, against 18c XE) until the *untagged* statement is
  refused. That number is the ceiling; record it in `docs/oracle.md` next to the
  tagging section the way the shared-pool numbers are recorded.
- **Then pick a rule.** Two candidates, and the choice should be written down
  rather than defaulted into:
  - refuse the rewrite for that frame (`locateStatementRewrite` answers, the
    tagger declines) once `len(run)+len(prefix)` would cross the measured
    ceiling. Cheap, and the per-statement determinism the design relies on still
    holds — the same statement always gets the same verdict — but it is a silent
    hole unless it logs.
  - refuse and log once per session, the way a session that cannot be certified
    already does (`decideStatementTagging`), so an operator hunting a missing tag
    finds the reason.
- Pin it with a unit test in `ttc_statement_rewrite_test.go` alongside the other
  refusals (`TestLocatorRefusesAnAmbiguousFrame` and friends), plus the live
  probe above.

## Decision

### What the measurement said, which is not what the spec assumed

`TestIntegration_OracleStatementLengthCeiling` walks statement length upward
through a proxy with tagging **off** — doubling from 32 KB, bisecting on the
first refusal — so what it exercises is Oracle and not dbbat. On
`gvenzl/oracle-free:23-slim` (23.26.3.0.0) it reached **128 MB without a single
refusal**: every probe returned its row. There is no ceiling to run into.

So the band this spec was written about — a statement that runs today and stops
running when tagging is turned on — **does not exist on 23ai**. The "64K" figure
`execMaxSQLLen`'s own comment quoted as Oracle's limit is folklore, and wrong by
four orders of magnitude; that comment now carries the measurement instead.

18c XE has no number, and it was attempted rather than skipped: the image is
amd64-only, and under emulation on the Apple Silicon host this was measured on it
brings the listener up and then dies with `ORA-00442` / `ORA-27300 … sxecheck4`,
which is the documented reason `defaultOracleImage` is the 23ai one. The test is
image-agnostic, so the nightly `18c XE (pinned)` leg of
`.github/workflows/integration.yml` (amd64) will produce that number on its next
run — and will fail if 18c has a ceiling under 1 MB where 23ai has none. Until
then 18c is unverified, not agreeing. 23ai is what is deployed and 23ai is the
number that matters.

### The rule, and why it is still worth having

The two candidates the spec offered were both framed around the server's
ceiling, so neither is quite the answer. What the walk turned up instead is that
the bound which actually binds is **dbbat's own**, and that there was a real
hole at it — measured, not reasoned: before this change a statement of exactly
`execMaxSQLLen` bytes *was* tagged, putting a TTC length field declaring
1 048 634 bytes on the upstream wire. That is a length dbbat's own decoders call
implausible and would refuse to read back. Oracle did not mind; the invariant
did.

**Rule: a statement is tagged only while the tagged text stays inside
`maxTaggableStatementBytes`, which is `execMaxSQLLen`.** dbbat does not write a
statement dbbat would not read. The one constant, used by the reader and the
writer both, is what keeps the two from drifting apart —
`TestTaggableBoundIsTheReadingBound` fails if someone moves one without the
other. Should a future server turn out to have a real ceiling below 1 MB, this
constant is the single place it goes, and
`TestIntegration_OracleStatementLengthCeiling` is the test that fails on the day
that becomes true.

**Enforcement: per frame, logged once per session** — the spec's first candidate,
with the second's logging discipline. Not a session demotion, and for the reason
already written down for the locator's own frame-level skip: demoting the
session would untag every ordinary statement that followed, splitting each
across a tagged and an untagged SQL_ID, which is exactly the cursor doubling the
per-user tag was measured to avoid. A statement within a tag's width of 1 MB is a
rare outlier, and the session issuing one is usually issuing a hundred normal
statements too — demoting it trades a bounded loss (this statement's tag) for an
unbounded one. Per-statement determinism, the property the SQL_ID argument needs,
still holds: the verdict is a pure function of the statement's length and the
session's tag.

The log line is not optional and not shared: a refusal nobody can see is a hole
rather than a rule, and it gets its **own** message (`logMsgStatementTooLongToTag`)
and its own latch rather than reusing `warnStatementFrameSkipped`'s, so an
operator hunting a statement missing from `V$SQL` can tell "too long to grow"
from "a client shape dbbat cannot model". The message is a constant for the
reason `TestCountingHandlerWatchesTheMessagesTheGateEmits` spells out — a test
that counts a log line by copying its text goes vacuous the day it is reworded.

### What was built

- `maxTaggableStatementBytes` + `stmtRewrite.fitsTagged` in
  `ttc_statement_rewrite.go`; enforced in `session.rewriteStatementMessage`,
  which is where the tag's width is known (`locateStatementRewrite` is a pure
  function of the frame and never sees it).
- `TestRewriteRefusesToGrowAStatementPastWhatDbbatReads` — a frame the locator
  answers for, refused for its length: to the byte, both sides of the boundary.
- `TestTaggableBoundIsTheReadingBound` — the reader's and writer's bounds tied
  together.
- `TestStatementTaggingRefusesAStatementItCannotGrow` — session level: the frame
  forwards untagged, under its own reason, and the session keeps tagging.
- `TestStatementTaggingLogsTheLengthRefusalOnce` — five executions, one line.
- `TestIntegration_OracleStatementLengthCeiling` — the measurement, kept live.
- `TestIntegration_StatementTagStopsAtDbbatsOwnBound` — the boundary on a real
  23ai, to the byte, with the tag's width read off the live session rather than
  recomputed.
- `docs/oracle.md`, "How long a statement Oracle will actually parse".
