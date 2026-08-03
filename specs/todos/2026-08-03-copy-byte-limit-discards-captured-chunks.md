# PostgreSQL COPY capture still discards everything when the byte limit is hit

## Goal

Make the PostgreSQL COPY-TO capture path keep the prefix of the COPY stream it
already buffered when `max_result_bytes` is reached, the same way the `DataRow`
path now does.

## Why

`2026-08-03-pg-result-capture-keep-rows-on-limit` fixed the `DataRow` path but
deliberately left the COPY **byte**-limit path alone. Today:

- [`internal/proxy/postgresql/intercept.go:541-551`](internal/proxy/postgresql/intercept.go:541)
  (`captureCopyData`) sets `s.copyState.truncated = true` and stops buffering
  chunks once `totalBytes` would exceed `MaxResultBytes`.
- [`internal/proxy/postgresql/intercept.go:359`](internal/proxy/postgresql/intercept.go:359)
  (`logQuery`) then skips `parseCopyDataToRows()` entirely because of the
  `!s.copyState.truncated` guard, so the chunks already buffered are thrown
  away and the query stores **zero** rows.

It is the same all-or-nothing shape as the `DataRow` bug, on a path that is
much rarer (a COPY TO past 100 MB of captured payload). Since the previous spec
landed, the query at least reports `results_truncated = true`, so the outcome is
no longer *ambiguous* — just lossy.

The reason it was not fixed in the same pass: the buffered chunk list almost
always ends mid-line, and `parseCopyDataToRows` splits on `\n`, so parsing the
prefix as-is would emit a final malformed row. The fix needs to drop the
trailing partial line (everything after the last `\n`) before parsing.

The COPY **row**-limit path is fine — it already keeps its prefix and now also
flags `results_truncated`.

No GitHub issue filed yet — one should be opened.

## Implementation

- In `parseCopyDataToRows`, when `s.copyState.truncated` is set, discard the
  bytes after the last `\n` in the concatenated data before splitting, so no
  partial row is emitted.
- In `logQuery`, drop the `!s.copyState.truncated` condition from the branch
  that chooses `parseCopyDataToRows()` (keep the `len(dataChunks) > 0` check).
  `query.ResultsTruncated` is already computed after the rows are built, so it
  stays correct.
- Cover it with a unit test on `parseCopyDataToRows`: a chunk set ending
  mid-line plus `truncated = true` yields only whole rows.

## Files

- `internal/proxy/postgresql/intercept.go` — `captureCopyData`, `logQuery`,
  `parseCopyDataToRows`.
- `internal/proxy/postgresql/intercept_test.go` — the new case.
