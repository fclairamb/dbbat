---
model: sonnet
effort: medium
pr: batch
automerge: false
---

# Deleting a session capture writes no audit entry

## Problem

Found by the security scan of 2026-11-23 (finding L2). Severity: Low. An admin (or an admin API key) can trigger it.

`DELETE /api/v1/connections/{uid}/dump` (`internal/api/observability.go:823-861`, `handleDeleteConnectionDump`) removes the local spool file and the bucket object, and clears `dump_key`. It writes no `audit_log` entry, only a process log line, which the HMAC chain does not cover. Other evidence-shaped actions are chained: archive creation and deletion (`internal/store/archive.go:169, 318`), server edits (`database.updated`), connection terminations. Deleting a capture leaves no trace, and `DBB_RETENTION_DUMPS_DELETE_AFTER` provides a plausible explanation for its absence.

## Proposal

1. In `handleDeleteConnectionDump`, after a successful delete, write a chained `dump.deleted` entry. Include the connection UID, the actor (`PerformedBy`), where it was deleted from (`local`, `remote` or both), the object key if any, and the capture's size if known.
2. Have the retention sweeper's dump expiry write the same entry, with `source: retention` and no actor, batched through `Store.LogAuditEvents`. That way a capture's disappearance is always accounted for.
3. Add the event type to the audit filter list in the UI and to `docs/dump-format.md`.

## Tests

- `internal/api`: delete a dump (local, then remote with a `file://` bucket), and one `dump.deleted` entry exists with the actor and location. Fails before the change.
- A failed delete (missing file, bucket error) writes no entry.
- Retention expiry of a local capture writes one entry with `source: retention`.
