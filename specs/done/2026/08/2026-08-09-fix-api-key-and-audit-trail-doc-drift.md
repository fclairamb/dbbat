# Fix doc drift: API keys are hashed, not encrypted; the audit trail is not append-only

## Goal

Correct two security claims that the docs make and the code does not back:

1. **API keys are described as "encrypted blobs".** They are **Argon2id-hashed**,
   with only an 8-character `key_prefix` stored in clear for lookup.
2. **The audit trail is described as "append-only".** Nothing enforces that —
   `audit_log` is an ordinary table with no triggers, no `REVOKE`, and no
   integrity chaining.

## Why

Both statements appear on the public website and in the repository's own
`CLAUDE.md`, so they propagate into every future summary of the product. Neither
is a small imprecision:

- "Encrypted" implies the plaintext key is recoverable by the operator; hashed is
  the *stronger* property and the one the code actually implements
  (`internal/store/api_keys.go` calls `crypto.HashPassword`). Saying the weaker
  thing sells the product short *and* misleads anyone reasoning about key
  recovery or key rotation.
- "Append-only" is a compliance-relevant assertion. `website/docs/compliance.md`
  now states plainly that there is no tamper-evidence on the audit log, which
  directly contradicts the intro page two clicks away. Whichever a reader
  believes, one of the two pages is wrong.

Found while verifying every capability claim for the compliance mapping page.

No GitHub issue yet — file one when picking this up.

## Implementation

Grep for both claims and fix each occurrence; the wording should match what
`internal/store/api_keys.go` and `internal/migrations/sql/*_initial_schema.up.sql`
actually do.

Known sites:

- `website/docs/intro.md` — "**API keys**: encrypted blobs, scoped restrictions"
  (Security section) and "**Audit trails**: append-only record of who did what"
  (Why DBBat section).
- `CLAUDE.md` (repo root) — "API keys: encrypted blobs, prefix `dbb_`".
- Check `website/docs/security.md` and `website/docs/features/user-management.md`
  for the same phrasing.

Suggested replacements:

- API keys: "hashed with Argon2id — only an 8-character prefix is stored in
  clear, so a leaked database cannot yield a usable key."
- Audit trail: "a record of who did what, written by the API for every
  configuration change" — drop "append-only", or qualify it as "append-only by
  convention: DBBat never updates or deletes audit rows, but the table itself is
  not tamper-evident (see [Compliance](/docs/compliance))."

The one genuine exception worth keeping accurate: an API key *is* encrypted
transiently inside a pending device-authorization row (`DeviceAuthAAD`). Do not
let the correction erase that if it is documented anywhere.

## Follow-through

`specs/todos/2026-08-09-tamper-evident-audit-chain.md` would make a real
append-only claim true. Until it ships, the docs must not make it.
