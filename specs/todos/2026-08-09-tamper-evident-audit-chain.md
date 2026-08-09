# Tamper-evident audit log (HMAC chaining)

## Goal

Make dbbat's audit trail verifiable: each record carries an HMAC over its
content plus the previous record's HMAC, so deleting, altering, or reordering
any entry breaks the chain, and `dbbat audit verify` reports exactly where.

## Why

The competitive landscape doc lists "compliance packaging" as an honest gap:
dbbat has the audit substance but not the proof story. Auditors (ISO 27001
A.8.15, SOC 2 CC7.x, PCI DSS 10.5) ask specifically about protecting logs
from tampering — and an admin with direct PostgreSQL access can today rewrite
`audit_log` or `queries` invisibly. AccessFlow's HMAC-chained log shows this
is becoming table stakes in the space. A keyed chain is a small feature that
upgrades "we log everything" to "and we can prove nobody edited it."

Companion spec: `2026-08-09-compliance-mapping-page.md` (the website page
that cites this feature).

No GitHub issue yet — file one when picking this up.

## Implementation

### Keying

HMAC-SHA-256, key derived from the existing AES-256 encryption key
(`DBB_KEY` / `DBB_KEYFILE`, see `internal/crypto/`) via HKDF with a
dedicated info string (e.g. `dbbat-audit-chain-v1`). A plain hash chain can
be recomputed by whoever tampers; keying it means DB access alone is not
enough — the attacker also needs the key the app holds.

### Phase 1 — chain `audit_log`

Low volume, never auto-deleted: the easy, high-value start.

- Migration (`internal/migrations/sql/`): add `prev_mac bytea` and
  `mac bytea` to `audit_log`; a genesis/anchor row marks where chaining
  begins (rows predating the migration are documented as unverifiable).
- `Store.LogAuditEvent` (`internal/store/audit.go`) computes the MAC over a
  canonical serialization of (uid, event_type, user_id, performed_by,
  details, created_at, prev_mac). Serialization must be deterministic —
  fixed field order, RFC 3339 nano timestamps, raw `details` bytes.
- Concurrency: the chain head is a serialization point. Insert inside a tx
  holding a PostgreSQL advisory lock (volume is admin-action-level, so
  contention is negligible). Keep the current head cached in memory,
  re-reading it under the lock on conflict/restart.
- Verification: `dbbat audit verify` CLI (wire into `main.go` next to the
  `db`/`dump` commands) walks the chain oldest→newest, reports the first
  break (or success + head MAC to note down externally). Optionally an
  admin-only API endpoint returning the same result.

### Phase 2 — chain `queries` per connection

The wire audit is where the compliance value peaks, but the table is
high-volume, written by a batched writer, and subject to
`DBB_QUERY_STORAGE_RETENTION` deletion — a single global chain would break
the moment retention deletes old rows.

- Chain per **connection**: each query's MAC covers the previous query on
  the same connection; on session close, stamp the final chain head onto the
  connection row (`internal/store/connections.go`). Retention deletes whole
  connections' histories, so surviving connections stay verifiable
  end-to-end.
- The per-connection ordering already exists naturally in the proxy session;
  compute MACs in the query-recording path (`internal/store/queries.go` and
  the batched result writer) where ordering is known, not in SQL.
- `dbbat audit verify --queries [--connection <uid>]` extends the CLI.

### Key files

- `internal/store/audit.go`, `internal/store/models.go` (`AuditLog` ~line
  871, `Query` ~line 396), `internal/store/queries.go`
- `internal/crypto/` — HKDF derivation helper
- `internal/migrations/sql/` — new columns + anchor
- `main.go` — `audit verify` subcommand
- Docs: short page on the chain design + how to run verification during an
  audit
