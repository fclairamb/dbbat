---
model: opus
effort: medium
---

# The OALL8 statement rewriter is written, tested and switched off

## Goal

Re-enable `oall8RewriteEnabled` (`internal/proxy/oracle/ttc_statement_rewrite.go`)
once there is a recording of a real OALL8 client in `testdata/` to prove the
rewrite against — and give the path the guards the other two shapes already have
before flipping it.

## Why

`2026-09-16-08` built a TTC statement writer covering three ops. Two of them —
the `03 5e` piggyback execute and its `11 69`-stapled twin, in both the thin and
the OCI wide header shapes — are proven: 159 of 159 statement frames in
`testdata/` locate, rewrite to themselves byte for byte and read back tagged, and
the tag was driven live through go-ora, sqlplus (OCI) and python-oracledb thin
against a real 23ai.

The third, legacy `OALL8` (`0x0e`, pre-v315), is proven by nothing:

- **No recording carries one.** The corpus survey counts zero OALL8 statement
  frames across all 26 `.pcapng` files, and `make test-e2e-oracle` drives no
  client that sends one.
- **It is the least-defended path of the three**, and that is a property of the
  op's layout rather than of how the code is written. It does not go through
  `locateStatementValue`, so it gets none of that function's guards: no
  uniqueness requirement, no boundedness check at the run's far end, and — the
  one that matters most — no `valuePrecededByAnotherLength`. That check exists
  because a stale second copy of the declared length left outside the rewritten
  span is exactly the bug a real 23ai caught once (`ORA-03120: two-task
  conversion routine: integer overflow`, on a single-chunk CLR long form read as
  a bare run).
- **Its round-trip check is half a check.** With `clrKind == stmtClrNone` the
  value half of `stmtRewrite.verify` compares the run against itself, which is
  true by construction. Only the length half does work, so the certainty this
  path gets is strictly weaker than every other shape's.
- **Its offsets come from a decoder nobody has validated.** `decodeOALL8`
  (`ttc_decode.go`) carries this package's own comment: *"This is a simplified
  decoding that handles the most common cases."* It has never been checked
  against a real OALL8 capture, because there is none.

So it was switched off rather than shipped: turning it off costs zero coverage
today, and leaves no unguarded write path in the tree even behind a default-off
flag. The encoder and its unit tests stay as the specification of what to
re-enable.

## Implementation

1. **Get a capture first.** Find a client that still speaks pre-v315 OALL8 (an
   old OCI runtime, or a thin client pinned to a low TTC version) and record it
   the way the rest of `testdata/` was recorded — `go test -tags capture -run
   'TestCapture_…'`, see docs/oracle.md. Without this, nothing below is worth
   doing: the whole reason the path is off is that it is unfalsifiable.
2. **Point the corpus survey at it.** `ttc_statement_rewrite_survey_test.go`
   already walks every `.pcapng` and asserts `located == frames`; a new recording
   makes the OALL8 shape appear in `TestSurveyStatementRewritePerClientShape`'s
   table as `varlen`/`bare`, and the identity and round-trip assertions cover it
   for free.
3. **Give it the guards.** Either route `locateOALL8Rewrite` through
   `locateStatementValue` (which needs the length field expressed as an
   `execSQLLenField` with `kind: stmtLenVarLen`, so the search, the uniqueness
   check and `valuePrecededByAnotherLength` all apply), or add those three checks
   to it explicitly. The first is better: one implementation of "where may a
   statement be" is the whole reason `execSQLLengthField` was shared between the
   gate and the rewriter.
4. **Validate `decodeOALL8` against the recording** before trusting its offsets —
   in particular the `func(1) + options(4) + cursor(2)` walk the rewriter
   hard-codes as `lenAt = 7`, and the bind count sitting immediately behind the
   text, which is what a widened length field shifts.
5. **Flip `oall8RewriteEnabled` and delete `TestOALL8RewriteIsDisabled`**, adding
   a live e2e case that drives the OALL8 client through a tagging proxy and reads
   the tag back out of `V$SQL`, as the three shipped shapes already do.

## Blocked — step 1 was attempted and the client does not appear to exist

Measured 2026-09-19. Nothing below step 1 was done, because step 1 says not to.

**What was driven.** `ojdbc6 11.2.0.4` is the oldest Oracle driver still reachable
from a package repository (Maven Central,
`com.oracle.database.jdbc:ojdbc6:11.2.0.4`; the `com.oracle:ojdbc14:10.2.0.4.0`
coordinate that looks like a 10.2 driver is a **554-byte licence stub**, not a
jar). Oracle 23ai Free still accepts it. It was recorded through the repo's own
capture relay — the harness is committed as
`internal/proxy/oracle/capture_legacy_oall8_test.go`, reproducible with
`OJDBC6_JAR=… go test -tags capture -run TestCapture_LegacyOALL8 ./internal/proxy/oracle/`.

**Result: a genuinely pre-v315 session that sends no OALL8.**

- The session negotiates **TNS version 310** (ACCEPT payload `01 36`), below the
  315 the rest of this tree calls the boundary.
- Every statement it sends is the piggyback exec dbbat already covers: three
  statement frames, all `compressed/bare` — `03 5e` for the first statement,
  `11 69`-stapled for the rest. **Zero frames start with `0x0E`.**

**And Oracle's own driver says `0x0E` is not what OALL8 means.** In that same
jar, `oracle.jdbc.driver.T4C8Oall` — the class named after OALL8 — constructs a
`T4CTTIfun` of message type **3** with function code **94 = 0x5E**
(`javap -c` on the constructor). So "OALL8" in Oracle's vocabulary *is* the
`03 5e` frame this package already locates, rewrites and proves live; the
`TTCFuncOALL8 = 0x0E` constant here names some other, older op, and
`decodeOALL8`'s layout for it remains a guess nothing has ever corroborated.

**What would unblock it**, and why none of it was available:

- a pre-v315 *thin* client: falsified above — pre-v315 does not imply `0x0E`, so
  "pin a thin client to a low TTC version" is not a route to this frame;
- an Oracle 9i/10g client (OCI media or `classes12.jar`): Oracle-licensed, not on
  Maven Central, not installable from any package manager here, and a client that
  old is below what either available server (23ai Free, XE 18.4) accepts.

**So the honest next decision is not "capture it", it is "keep it or delete it".**
Either someone produces a `0x0E` frame from a real client and this spec resumes
at step 2, or the op is accepted as unidentified and the whole `0x0E` statement
path — decoder, rewriter, gate — is deleted rather than carried as defence in
depth against a frame no evidence says exists. The flag stays off either way; it
costs no coverage today.

**A real bug fell out of the attempt** and is filed separately:
`specs/todos/2026-09-19-02-oracle-piggyback-exec-5e-reexec-ungated.md`. ojdbc6
re-executes a prepared statement as `03 5e` **with no statement text**, a shape
`IsPiggybackCursorReexec` does not recognise (it knows only sub-ops `0x4e` and
`0x04`) and `handlePiggybackExec` forwards ungated on its decode failure. That is
`read_only`, `block_ddl` and every approval pattern skipped from the second
execution on — the hole the SQL-less `OALL8` path exists to close, open on the
op clients actually use.
