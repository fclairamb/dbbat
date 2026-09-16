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
