---
model: sonnet
effort: low
---

# Clean up orphaned `.dbbat-dump` files left in operational dump directories

## Goal

Make sure deployed instances do not accumulate unreadable, never-expiring
`.dbbat-dump` files after the switch to the pcapng capture format.

## Why

The capture format moved from the bespoke `.dbbat-dump` v2 framing to pcapng
(`specs/todos/2026-08-02-02-pcap-capture-format.md`). The committed code no
longer has a v1/v2 read path, and `internal/dump/cleanup.go` scans for
`dump.FileExt`, which is now `.pcapng`.

Consequence on any instance that ran with `DBB_DUMP_DIR` set before the upgrade:
every pre-existing `.dbbat-dump` file is now

- unreadable by dbbat (the download endpoint looks for `<uid>.pcapng`), and
- invisible to the retention sweep, so it is **never** deleted — the directory
  grows monotonically with dead files.

The test fixtures were converted as part of the format change; operational
directories were not (no access to them from the dev machine).

## Implementation

Two options; pick one, they are not exclusive.

1. **Operational one-liner (preferred, no code).** On each deployment with
   `DBB_DUMP_DIR` set, delete the leftovers once:
   `find "$DBB_DUMP_DIR" -name '*.dbbat-dump*' -delete`.
   Captures are short-lived debugging artefacts with a 24h default retention, so
   there is nothing worth converting — dropping them is the right call. Check
   the dbbat deployments (see the DBBat deployment notes) before assuming there
   are none.

2. **Belt and braces in code.** Extend `CleanupOldFiles` in
   `internal/dump/cleanup.go` to also match the legacy `.dbbat-dump` extension
   so the retention sweep reaps them on its own. Keep it to the cleanup path —
   do *not* reintroduce a legacy read path anywhere else. A short-lived
   `legacyFileExt = ".dbbat-dump"` constant with a comment saying it exists only
   for retention, plus a test case in `dump_test.go`'s `TestCleanupOldFiles`,
   covers it. Remove it again after a release or two.

No GitHub issue exists for this; file one if the cleanup is not done promptly.
