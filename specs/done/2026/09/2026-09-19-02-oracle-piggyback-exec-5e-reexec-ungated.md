---
model: opus
effort: medium
---

# A prepared statement re-executed as `03 5e` with no SQL goes upstream ungated

## Goal

Recognise the SQL-less piggyback exec (`03 5e`, no statement text) as a cursor
re-execution and put it through the same gate the SQL-less `OALL8` already goes
through, instead of forwarding it because its decode failed.

## Why

Found on 2026-09-19 while trying to record a legacy `OALL8` client for
`2026-09-16-11-oracle-tag-oall8-rewrite.md`. The client recorded there —
**ojdbc6 11.2.0.4**, Maven Central, against Oracle 23ai Free — re-executes a
`PreparedStatement` by sending the exec op **with the statement text omitted**:

```
#09  03 5e 06 02 80 60 01 04 00 00 01 01 0d 00 …   58 bytes, no SQL run
```

Every other client in the corpus re-executes with sub-op `0x4e` (SELECT) or
`0x04` (DML), which `IsPiggybackCursorReexec` knows and the gate refuses under
`refuseUnknownCursor`. This one keeps sub-op `0x5e`. Measured on that frame:

| check | result |
|---|---|
| `IsPiggybackExecSQL` | **true** (it tests the sub-op byte and nothing else) |
| `IsPiggybackCursorReexec` | false — only `0x4e` / `0x04` count |
| `decodePiggybackExecSQL` | error: *"OALL8 message contains empty SQL"* |
| `handlePiggybackExec` | logs at debug and **`return nil` — forwarded ungated** |

So from the second execution on, a prepared statement runs with no `read_only`
check, no `block_ddl` check, no `ValidateOracleQuery`, no approval pattern, no
`queries` row and no quota accounting. The parse is gated; the repeats are not.
That is exactly the hole `OALL8NoSQLError` → `refuseUnknownCursor` exists to
close, open on the op clients actually use, and reachable from a driver anyone
can download.

`frameCarriesStatement` has the same false positive (it delegates to
`IsPiggybackExecSQL`), which is why dropping the ojdbc6 recording into
`testdata/` trips the coverage floor in `TestSurveyStatementRewriteCorpus`: the
locator rightly refuses a frame with no statement in it, and the survey counts
that as a statement frame it failed to locate. Fixing the classifier fixes the
survey too, and lets the recording become a corpus fixture.

Note the blast radius is the opposite of the `03/05` fetch gate that was
deliberately left alone (see docs/oracle.md, "Cursor re-execution"): this is not
"a fetch with nothing in flight", it is an execute whose statement dbbat has
already seen and tracked on this very cursor.

## Implementation

1. **Reproduce.** `internal/proxy/oracle/capture_legacy_oall8_test.go` records
   the session; it needs the jar and a running 23ai:
   ```bash
   docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
   curl -O https://repo1.maven.org/maven2/com/oracle/database/jdbc/ojdbc6/11.2.0.4/ojdbc6-11.2.0.4.jar
   OJDBC6_JAR=$PWD/ojdbc6-11.2.0.4.jar go test -tags capture -run TestCapture_LegacyOALL8 -v ./internal/proxy/oracle/
   DUMP_PATH=<path it logs> go test -tags capture -run TestAnalyzeDump -v ./internal/proxy/oracle/
   ```
2. **Tell the two cases apart on the wire, not by sub-op.** A `03 5e` whose
   statement length is zero is a re-execution; one with a statement is a parse.
   The decoder already distinguishes them — it returns the "empty SQL" error —
   so the fix is to stop treating that error as "undecodable, forward it".
   Decide it the way `decodeOALL8` does: report *no statement, cursor id X* as
   its own condition (an error type carrying the cursor id, as
   `OALL8NoSQLError` does) rather than as a decode failure. The cursor id is in
   the frame (`01 04` above is a candidate — confirm it against
   `decodeCursorReexec`'s compressed-int walk before trusting it).
3. **Route it through the existing gate.** `handlePiggybackExec` should hand
   that condition to the same `refuseUnknownCursor` path the SQL-less `OALL8`
   and the `0x4e`/`0x04` re-executions use, so a known cursor replays its
   tracked statement through the validators and an unknown one is refused under
   a grant that cares. No new policy — the policy already exists, the frame just
   never reached it.
4. **Fix `frameCarriesStatement`** to agree (a `03 5e` with no statement text is
   not a statement frame), then add the ojdbc6 recording to `testdata/` and let
   `TestSurveyStatementRewriteCorpus` cover it — it is the only pre-v315 client
   in the corpus and its `compressed/bare` frames locate, rewrite to themselves
   and round-trip today.
5. **Pin it with a replay test** off that fixture, next to
   `cursor_reexec_replay_test.go`: the SQL-less `03 5e` is refused under
   `read_only` / `block_ddl` exactly as the `0x4e` shape is, and a
   statement-carrying `03 5e` is untouched.
6. **Update `docs/oracle.md`** — the client table under "Cursor re-execution"
   claims every thin client re-executes with `0x4e`; ojdbc6 is the
   counter-example, and "Those two frames are the whole gate" becomes three.
