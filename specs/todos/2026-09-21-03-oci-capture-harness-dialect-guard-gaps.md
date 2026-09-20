---
model: opus
effort: low
---

# The OCI capture harness's dialect guard has three loose ends

## Goal

Close the remaining gaps in the capture-tooling dialect guard added by
`2026-09-20-03-oracle-wide64-oci-refcursor-and-drive.md`, so a future capture
run can't silently overwrite audited fixture evidence and so the guard itself
is regression-tested rather than only manually exercised.

## Why

The final audit of that spec (now in `specs/done/2026/09/`) verified the
production security fix as solid across three rounds, but flagged three
non-blocking follow-ups in the test/tooling evidence layer:

1. **No automated regression test for `requireRecordedDialect` /
   `recordedDialectLooksWide64`.** Both refusal directions (a `container`
   client recording 4-byte-shaped bytes, and a `path`/unset client recording
   wide64-shaped bytes) were verified only by the implementer manually running
   the `capture`-tagged harness against a live container. Nothing catches a
   future regression to this guard.
2. **`-tags capture` is never built in CI** (`.github/workflows/ci.yml` only
   runs `go vet -tags integration ./...`), so the guard isn't even
   compile-checked automatically.
3. **A sibling capture harness still has the unchecked case the guard was
   built to close.** `capture_oci_describe_test.go`'s `TestCapture_SQLPlusDescribe`
   writes `testdata/oci_describe.hex` via `writeDescribeHexFixture` with no
   `requireRecordedDialect` call at all. A 64-bit-dialect sqlplus on `PATH`
   would silently overwrite that fixture with 64-bit bytes, stamped with the
   4-byte "Regenerate with" header — the exact audited-evidence-replaced
   symptom the guard exists to prevent, just via a different entry point.

## Implementation

1. Add a synthetic-dump unit test for `recordedDialectLooksWide64` (and
   `usesWide64OpHeader`'s reuse in `requireRecordedDialect`): write a
   `dump.Writer` in a `t.TempDir()` with one frame taken from
   `oci64_refcursor_drives.hex` and one from `oci_refcursor_drives.hex`, and
   assert the predicate returns `true`/`false` respectively. Cheap, no Docker
   required.
2. Wire `requireRecordedDialect` (or an equivalent call) into
   `TestCapture_SQLPlusDescribe` before `writeDescribeHexFixture` runs, the
   same way `capture_refcursor_test.go` gates its own writes.
3. Add `go vet -tags capture ./...` (or the package-scoped equivalent) to
   `.github/workflows/ci.yml` so this whole file group is at least
   compile-checked on every push, not just when someone happens to run the
   capture harness locally.

## Files

- `internal/proxy/oracle/capture_refcursor_test.go` — `requireRecordedDialect`,
  `recordedDialectLooksWide64`
- `internal/proxy/oracle/capture_oci_describe_test.go` — `TestCapture_SQLPlusDescribe`
- `.github/workflows/ci.yml`
