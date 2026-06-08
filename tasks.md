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
- [ ] **Parse describe column-definition records (keystone)** — the highest-value remaining
      item. Yields real column names (instead of synthetic `COLn`) and, more importantly, the
      per-column data type, which unblocks type-aware value decoding and resolves the heuristic
      ambiguities the type-less path can't: all-ASCII negative NUMBERs decoded as text,
      BINARY_FLOAT/BINARY_DOUBLE not decoded, and 7/11/13-byte NUMBERs that can collide with
      DATE/TIMESTAMP. Each record is `ParameterInfo.load` in go-ora: a fixed field sequence
      (type, flag, precision, scale, maxLen, contFlag, charset, name/schema/typeName via
      `GetDlc` = `readCompressedInt` + `readCLR`) followed by a **version-dependent tail** that
      sets the record length — getting that tail wrong silently corrupts every later column, so
      this needs a dedicated session with a test harness iterating against the fixtures, not a
      20-min slot. Build it as a best-effort parser with a fallback to the current scan+pad
      (zero regression).
- [x] Extract Oracle username from TTC AUTH — stale item: already implemented (PR #134,
      `parseAuthPhase1` → `GetUserByUsername` → grant check; no fallback). Docs updated.
- [ ] Multi-key O5LOGON support (only the user's first verifier-bearing API key works today;
      needs per-user salt — the AUTH challenge can only carry one salt, so per-key salts
      can't be validated after the challenge is sent)
