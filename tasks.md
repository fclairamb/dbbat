# DBBat — Outstanding Tasks

## In-flight: python-oracledb verification (PR #185)
- [x] Verify python-oracledb thin works end-to-end through dbbat (live, real Oracle 19c)
- [x] Add `patchAuthSvrResponse` unit tests
- [x] Update `docs/oracle.md` (Python now works; document the real fix)
- [x] Fix `lint` failure on PR #185 (prealloc in test helper) — pushed, CI re-running
- [x] Get PR #185 CI green and merged — merged 2026-06-06

## Housekeeping
- [ ] Tear down test dbbat instance (`:1522`/`:4200`), `dbbat-postgres` container, `dbbat_oratest` DB
      (keep for now — reused while working on the Oracle tasks below)

## Oracle: remaining protocol work
- [ ] **sqlplus 23c (OCI) → `ORA-12630`**: implement Oracle Native Services (NS) negotiation
      (OOB break/reset markers after the AUTH challenge). Large, needs an OCI client to verify.

## Oracle: observability gaps (best-effort, incremental — not blockers)
- [x] Capture DML row counts (INSERT/UPDATE/DELETE) from v315+ responses — implemented via
      the OER status block (`ttc_oer.go`); unit-tested + live-capture replay
      (`testdata/go_ora_dml.dbbat-dump`, ground truth 1/5/3/2 rows + ORA-00942).
- [x] Large result-set row capture — QueryResult (func 0x10) and continuation (func 0x06)
      paths now share `parseRowStream`, which walks the full compressed row stream
      (length-prefixed values + `0x15` column-compression descriptors) with no row cap.
      Previously `scanRowValues` stopped at the first `0x15` descriptor (≈2 rows) and capped
      at 100. Verified end-to-end against a live-Oracle ground-truth fixture
      (`testdata/go_ora_largeresult.dbbat-dump`, 400 rows, `TestDumpReplay_LargeResultRows`).
      ⏳ Multi-TNS-packet (small-SDU/JDBC) results reuse the same decoder but per-row
      correctness there is not yet ground-truth-verified.
- [x] Column-compression + NULL row capture — `parseRowStream` now carries unchanged
      columns forward and decodes the `0x15` descriptor structurally (bitmask is
      `ceil(numCols/8)` bytes), fixing corruption when a bitmask byte is itself `0x07`
      (all-columns-change boundary). Verified against a live-Oracle ground-truth fixture
      (`testdata/go_ora_compressed.dbbat-dump`, `TestDumpReplay_CompressedRows`:
      repeated column runs, NULLs, GRP change boundary).
- [x] Undetectable column names no longer break capture — the column count now comes from
      the describe header (`describeColumnCount`) instead of name-scanning, so single-char
      aliases (`SELECT level AS n`) and unnamed expressions (`SELECT 1`, `SELECT level*10`)
      capture all rows. Verified with a live-Oracle ground-truth fixture
      (`testdata/go_ora_colcount.dbbat-dump`, `TestDumpReplay_ColCount`); also lifts
      go_ora/python_thin replay column+row counts with no dbeaver regression.
      ⏳ Residual: undetectable columns get synthetic `COLn` labels (values correct); proper
      names need parsing the describe column-definition records.
- [x] NUMBER decimal/sign decoding — the heuristic row-capture path used a decoder that only
      handled non-negative integers (3.14 captured as "314"); the type-aware path
      (`decodeOracleNumber`) separately dropped the leading zero on sub-1 fractions ("0.5"→".5").
      Both now share `formatOracleNumber` (sign + base-100 mantissa + decimal placement),
      gated by `isOracleNumber` on the type-less path. Cross-checked against go-ora's reference
      decoder (`TestDecodeOracleNumberToString_Goora`) and verified end-to-end
      (`testdata/go_ora_numbers.dbbat-dump`, `TestDumpReplay_Numbers`).
      ⏳ Residual: all-printable-ASCII negative NUMBERs (e.g. -42) are still captured as text on
      the type-less path; needs the column type from the describe column-definition records.
- [x] TIMESTAMP-with-timezone decoding — implemented + unit-tested with real captures, and
      now re-verified live end-to-end (`testdata/go_ora_temporal.dbbat-dump`,
      `TestDumpReplay_Temporal`: DATE, TIMESTAMP, TIMESTAMP WITH TIME ZONE). The live run
      surfaced two bugs against Oracle Free 23ai, now fixed: the tz hour was read from the
      whole byte instead of `byte11 & 0x3f` (23ai sets bit 0x40), and the `0x40` "time in zone"
      flag was ignored — when set, the 7-byte prefix is the local wall clock and must not be
      shifted from UTC. Both the heuristic and type-aware decoders were corrected.
- [x] Combined-types row capture — integration fixture exercising NUMBER decimals, a
      compressed-away repeated column, NULLs, and DATEs together across 6 rows
      (`testdata/go_ora_mixed.dbbat-dump`, `TestDumpReplay_Mixed`). Locks in the interplay of
      the individual decoder fixes (newly covers DATE in row capture and 4-column compression).
- [x] Parse describe column-definition records — implemented in `describe.go`
      (`parseColumnDescribes`): walks each `ParameterInfo.load` record and returns the column
      name + TTC type code. Conservative — returns nil (caller falls back to scan+pad) on any
      misalignment, gated by an `isKnownTNSType` check. Key encoding details: scale/length are
      compressed ints whose length byte can have the high bit set for a negative value (the
      NUMBER `-127` float-scale sentinel), and this server's record tail is just two version
      ints. Verified against all six live-Oracle fixtures (`describe_test.go`): exact names +
      types, including the single-char `N` and unnamed `LEVEL*10` the heuristic scanner misses,
      and the temporal type codes. **Not yet wired into the live path** — see next item.
- [x] Wire describe column names into row capture — `decodeQueryResultV2` now prefers
      `parseColumnDescribes` for real names (single-char `N`, unnamed `LEVEL*10`, `COUNT(*)`),
      falling back to scan+pad when the records don't parse. Verified the parser handles the
      other-version fixtures too (dbeaver parsed 50/50 QueryResults, go_ora 4/4, python 3/4 with
      one graceful fallback) and that `decodeQueryResultV2` returns the true names
      (`TestDecodeQueryResultV2_RealColumnNames`); no regression in the existing replay tests.
- [ ] **Type-aware value decoding** — thread the per-column data type from `parseColumnDescribes`
      into the row-capture value decode (via `decodeOracleValue`) instead of the type-less
      `decodeOracleRawValue` heuristic. Resolves the remaining ambiguities: all-ASCII negative
      NUMBERs captured as text, BINARY_FLOAT/BINARY_DOUBLE, and 7/11/13-byte NUMBERs that can
      collide with DATE/TIMESTAMP. Keep the heuristic as the fallback when no type is available
      (continuation packets, parser fallback).
- [x] Extract Oracle username from TTC AUTH — stale item: already implemented (PR #134,
      `parseAuthPhase1` → `GetUserByUsername` → grant check; no fallback). Docs updated.
- [ ] Multi-key O5LOGON support (only the user's first verifier-bearing API key works today;
      needs per-user salt — the AUTH challenge can only carry one salt, so per-key salts
      can't be validated after the challenge is sent)
