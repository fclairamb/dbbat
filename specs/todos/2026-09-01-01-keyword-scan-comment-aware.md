# Oracle: the last-resort keyword scan can still start a "statement" inside a comment

## Goal

`findSQLInPayload` (internal/proxy/oracle/ttc_decode.go) should not lift a
statement out of the middle of a SQL comment when the run's real start is
recoverable.

## Why

The 2026-09-01 fix made the header-anchored decode accept comment-led
statements and chunked (CLR long form) statements, so the frames that used to
fall through to the keyword scan no longer do. But the scan itself is
unchanged: for a frame whose exec header dbbat cannot read at all — and on the
unnameable-piggyback path (`stapledStatements`), where a false reading ends the
session — a payload carrying `-- MERGE s'execute` in any embedded text still
reads as a statement opening at `MERGE`, with the apostrophe then opening a
quoted run that never closes. The production incident showed exactly what that
produces: a `dbbat could not read this statement to its end` refusal for text
that was never a statement, and `/queries` rows whose SQL text starts
mid-comment (`UPDATE d’instances ne pouvait faire le travail --` was recorded
from a French comment on 2026-09-01).

## Implementation

Two candidate directions, to be weighed rather than both taken:

- After the keyword match at `idx`, extend the run **backwards** through
  printable bytes to the start of the printable run, then verb-check the whole
  run with `skipLeadingSQLComments`. Risk: the 2026-08 survey
  (`sql_extraction_survey_test.go`) measured that backward extension swallows
  length-prefix bytes that are themselves printable (a space is 32, `T` is 84),
  which is the exact misread the header-anchored decode was built to remove.
  Any change here must re-run the survey and keep
  `TestBundledOCIFixturesCarryNoStatement` green.
- Cheaper: when the extracted run, walked with the comment-aware scanner,
  turns out to *begin inside* a line comment whose `--` sits earlier in the
  same printable run, either re-anchor at the run start or discard the match
  (scan on from `end`). Discarding is fail-open on the gate but never invents
  a statement, which on the unnameable path is the safer direction.

Key files: `internal/proxy/oracle/ttc_decode.go` (`findSQLInPayload`,
`indexOfAnyKeywordCI`), `internal/proxy/oracle/ttc_exec_statement.go`
(`skipLeadingSQLComments`), `internal/proxy/oracle/session.go`
(`stapledStatements` — the consumer where a false positive costs a session).
