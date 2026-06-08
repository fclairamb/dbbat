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
- [ ] Single-char column names break capture — `scanColumnNames` requires ≥2-char names,
      so `SELECT x AS n` undercounts columns and corrupts row parsing. Real fix: source the
      column count from the describe metadata instead of name-scanning (avoid widening the
      heuristic name matcher, which risks false positives).
- [x] TIMESTAMP-with-timezone decoding — implemented + unit-tested with real captures.
      ⏳ Live end-to-end re-verification deferred until VPN reconnect (re-run probe, confirm
      captured rows decode instead of hex).
- [x] Extract Oracle username from TTC AUTH — stale item: already implemented (PR #134,
      `parseAuthPhase1` → `GetUserByUsername` → grant check; no fallback). Docs updated.
- [ ] Multi-key O5LOGON support (only the user's first verifier-bearing API key works today;
      needs per-user salt — the AUTH challenge can only carry one salt, so per-key salts
      can't be validated after the challenge is sent)
