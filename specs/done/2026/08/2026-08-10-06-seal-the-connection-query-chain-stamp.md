---
model: opus
effort: high
---

# Seal the connection's query-chain head stamp

## Goal

Make `connections.query_chain_mac` a **keyed** stamp — `HMAC(chain key, domain ‖
connection_uid ‖ query_chain_len ‖ head_mac)` — instead of a verbatim copy of
the last statement's MAC, and compare `query_chain_len` against the surviving
statement count during verification. This is the same fix
`2026-08-10-chain-captured-result-rows.md` already applied to the row-chain
stamp (`queries.row_chain_mac`, `Store.rowChainStampMAC`).

## Why

`connections.query_chain_mac` is the *only* thing that detects statements
deleted from the **end** of a session: the surviving prefix of a chain always
verifies against itself, so without the stamp a trailing deletion is invisible.

The stamp does not survive the threat model the audit chain is built for.
`Store.CloseConnection` (`internal/store/connections.go:91`) writes the head MAC
verbatim, and `checkStampedHead` (`internal/store/chain_verify.go:459`) compares
it to the last surviving statement's raw `mac`. That value is **readable from
the `queries` table**. So an attacker with write access to the storage database
— precisely the attacker the chain exists to catch, and who cannot forge a row
MAC without the key — can:

```sql
DELETE FROM queries WHERE uid = '<the last few statements>';
UPDATE connections
   SET query_chain_mac = (SELECT mac FROM queries
                           WHERE connection_id = '<conn>' AND chain_seq IS NOT NULL
                           ORDER BY chain_seq DESC LIMIT 1),
       query_chain_len = (SELECT count(*) FROM queries WHERE connection_id = '<conn>')
 WHERE uid = '<conn>';
```

…and `dbbat audit verify --queries` reports a clean chain. **No key required.**
Nothing else covers the stamp either: `queryChainPayload`
(`internal/store/chain.go`) seals a statement's identity and does not reach the
connection row, so editing `query_chain_mac` does not break the query chain.

`TestQueryChainDetectsTrailingDeletion` (`internal/store/chain_test.go:580`)
only exercises the attacker who forgets the second statement. `query_chain_len`
is stored and printed in the break message but never actually compared, so even
a length that contradicts the surviving rows passes.

This is a real hole in a **shipped, documented** compliance claim (ISO/IEC
27001:2022 A.8.15, PCI DSS v4.0 req. 10.3). The docs were corrected in the same
pass that filed this todo — `docs/audit-chain.md`,
`website/docs/features/audit-chain.md` and `website/docs/compliance.md` now say
the per-connection stamp is forgeable without the key and point here — so the
current state is honest, but the gap is open.

No GitHub issue yet — file one when picking this up.

## Implementation

The mechanism is a straight copy of the row-chain one:

- Add `queryStampDomain = "dbbat-query-chain-stamp-v1"` and a
  `Store.queryChainStampMAC(connectionUID uuid.UUID, count int64, headMAC []byte) []byte`
  next to `rowChainStampMAC` in `internal/store/chain.go`.
- `Store.CloseConnection` (`internal/store/connections.go`) writes that MAC
  rather than `mac`.
- `checkStampedHead` (`internal/store/chain_verify.go`) compares
  `conn.QueryChainLen` against `result.Verified` first (precise message), then
  the stamp against `queryChainStampMAC(connUID, result.Verified, result.HeadMAC)`.
- Test the **re-stamping** attacker, not just the naive one — see
  `TestRowChainDetectsRestampedTrailingDeletion` for the shape.

### There is a second writer of this stamp, and it is pure SQL

`2026-08-10-stamp-chain-head-on-orphaned-connections.md` made the reconcile that
closes crash-orphaned sessions stamp the head too, so `CloseConnection` is no
longer the only writer. It does it as a **correlated subquery in the reconcile's
`UPDATE`** (`Store.orphanCloseQuery`, `internal/store/connections.go`), reading
`mac` straight out of `queries` — which is a verbatim copy *by construction*.

A keyed stamp cannot be computed in SQL at all: the chain key lives only in the
process. So sealing means this path stops being one pure-SQL statement. It has
to become: select the in-scope orphans and their heads, seal each in Go, write
them back — all inside **one transaction with the close**, because the reason
the current design stamps in the same statement is to avoid opening a second
window between marking a row disconnected and sealing its tail. Budget for it;
`TestQueryChainOrphanStampCostScalesWithOrphans` (`internal/store/chain_test.go`)
is the cost guard the rework has to keep honest, and the caveat about the stamp
sealing what survived at reconcile time (documented in `docs/audit-chain.md`)
stays true either way.

## The compatibility problem — this is the whole reason it is a separate task

Unlike the row stamp (which shipped sealed and never existed in the old format),
`connections.query_chain_mac` is **already written in the old format in every
deployment running 0.23.x or later**. Flipping the format makes every existing
stamp fail verification: `dbbat audit verify --queries` would report a break on
every closed session in the store, which is exactly the cry-wolf outcome the
truncated-prefix handling was written to avoid.

Options, roughly in order of preference:

1. **Verify against both formats, write only the new one.** `checkStampedHead`
   accepts either `queryChainStampMAC(...)` *or* the legacy raw `HeadMAC`, and
   reports the legacy match as a weaker result (a new counter — e.g.
   `legacy_stamps` on `QueryChainsResult`, surfaced by the CLI and
   `GET /api/v1/audit/verify/queries`) so an operator can see how much of the
   store is still on the forgeable stamp. Simple, no migration, but the weak
   check lingers for as long as the old rows do — and an attacker can always
   *downgrade* a stamp to the legacy format, which means the hole is only really
   closed once the acceptance is removed. Pick a release to drop it in and say
   so in the docs.
2. **Migrate the existing stamps.** A migration cannot do it — the chain key
   lives in the process, never in the database. It would have to be a one-shot
   command (`dbbat audit reseal --queries`) that walks each connection, verifies
   the chain, and rewrites the stamp in the new format. Honest, but a command
   whose job is "rewrite integrity metadata" is itself an awkward thing to hand
   an assessor, and it can only re-seal chains that still verify.
3. **Version the column.** Add `query_chain_stamp_version SMALLINT NOT NULL
   DEFAULT 0`, write `1` from now on, and let verification pick the check by
   version. Explicit and self-describing; costs a migration. Note the version
   column must itself be inside the new MAC, or an attacker just sets it back
   to `0` and gets option 1's downgrade for free.

Whichever is chosen, the docs corrected alongside this todo have to be updated
again to match, and the "what it detects" table in
`website/docs/features/audit-chain.md` needs its trailing-deletion row fixed
back once the stamp is genuinely unforgeable.

## Resolved open questions

> Options, roughly in order of preference: 1. Verify against both formats,
> write only the new one. 2. Migrate the existing stamps. 3. Version the
> column.

**Decision: option 3 — version the column.** Option 1 was listed first, but it
leaves the hole permanently open: an attacker who can write the database can
always *downgrade* a stamp to the legacy format, so the forgery this spec
exists to stop still works. A spec whose entire purpose is an unforgeable stamp
should not ship with a standing downgrade path.

Implement it as:

- A migration adding `query_chain_stamp_version SMALLINT NOT NULL DEFAULT 0`
  to `connections` (up **and** down, per the repo's migration convention).
- **The version is inside the MAC.** `queryChainStampMAC` covers
  `domain ‖ connection_uid ‖ version ‖ query_chain_len ‖ head_mac`. This is the
  point of the option — without it, setting the column back to `0` buys the
  attacker option 1's downgrade for free.
- Write version `1` from now on, from **both** writers (`Store.CloseConnection`
  and the reconcile path).
- `checkStampedHead` branches on the stored version: version `1` compares
  `conn.QueryChainLen` against `result.Verified` first (precise message) and
  then the keyed stamp; version `0` keeps today's legacy raw-`HeadMAC`
  comparison so existing 0.23.x stores do not cry wolf.
- Count the legacy rows and surface them. Add a `legacy_stamps` counter to
  `QueryChainsResult`, printed by `dbbat audit verify --queries` and returned
  by `GET /api/v1/audit/verify/queries`, so an operator can see how much of the
  store is still on the forgeable stamp. A version-`0` row is **not** a break —
  it is a weaker result, reported as such.
- Test the **re-stamping** attacker (the shape of
  `TestRowChainDetectsRestampedTrailingDeletion`), plus a downgrade attacker who
  rewrites a sealed row back to version `0` with a raw head MAC — that must be
  detected, and it is the specific thing option 1 could not do.

> [the reconcile stamp] does it as a correlated subquery in the reconcile's
> `UPDATE` … A keyed stamp cannot be computed in SQL at all.

**Confirmed, and in scope.** Rework `Store.orphanCloseQuery` into
select-seal-write in Go, **inside one transaction with the close**, exactly as
the spec describes — do not leave a second window between marking a row
disconnected and sealing its tail.
`TestQueryChainOrphanStampCostScalesWithOrphans` stays the cost guard and must
still pass; if the rework changes its constant factor, adjust the bound
deliberately and say so in the commit body, rather than deleting the test.

**Docs.** Update `docs/audit-chain.md`, `website/docs/features/audit-chain.md`
and `website/docs/compliance.md`: the per-connection stamp is now keyed, the
trailing-deletion row in the "what it detects" table goes back to detected
(qualified for version-`0` rows), and the pointer to this todo is replaced by a
note that pre-upgrade stamps remain legacy until their session closes.

## Implementation Plan

1. **Migration** `20260810020000_connections_query_chain_stamp_version` —
   `ALTER TABLE connections ADD COLUMN query_chain_stamp_version SMALLINT NOT
   NULL DEFAULT 0`, dropped on the way down. Every row that exists at upgrade
   time is therefore version `0`, which is exactly what it is: an unkeyed
   legacy stamp.

2. **`queryChainStampMAC`** in `internal/store/chain.go`, next to
   `rowChainStampMAC`, under a new `queryStampDomain =
   "dbbat-query-chain-stamp-v1"`. It covers, through `canonicalPayload` (every
   field tagged and length-prefixed, so no two distinct inputs share a byte
   string): `domain ‖ connection_id ‖ stamp_version ‖ query_chain_len ‖
   head_mac`. The version is *inside* the MAC — that is the whole point of
   versioning the column rather than accepting both formats forever.

3. **What `query_chain_len` means, and why it is the head's `chain_seq`.** The
   row-chain stamp seals a *count* because captured rows are never partially
   deleted. A session's statements are: `DBB_QUERY_STORAGE_RETENTION` reaps the
   oldest ones, which is what `TruncatedPrefix` exists to tolerate. Sealing a
   surviving-count would therefore make every retention-truncated closed
   session report a break — the exact cry-wolf outcome the truncation handling
   was written to avoid. `chain_seq` is dense from 1, so the head's `chain_seq`
   *is* the number of statements the session logged, and it is invariant under
   prefix truncation. It is also what both writers already store today, so the
   column's meaning does not change. Verification compares it against
   `result.HeadSeq`.

4. **Both writers stamp version 1.** `Store.CloseConnection` computes the MAC in
   Go and sets the three columns in its existing single `UPDATE`.

5. **Reconcile rework** (`Store.closeOrphans` /
   `Store.orphanCloseQuery`): the keyed stamp cannot be computed in SQL, so the
   pure-SQL correlated subquery becomes select-seal-write in Go, inside **one**
   transaction with the close:
   `UPDATE … SET disconnected_at = last_activity_at … RETURNING uid` →
   one `LEFT JOIN LATERAL` head lookup over just those uids (still built from
   `queryChainHeadSelect`, so the reconcile's notion of "the head" cannot drift
   from `CloseConnection`'s) → one bulk `UPDATE … FROM (VALUES …)` writing the
   sealed stamps. Three round trips, no second window. A session that logged
   nothing is left unstamped, as today.

6. **`checkStampedHead`** (`internal/store/chain_verify.go`) — order matters:
   - a version above the highest this binary knows is reported as
     "stamped by a newer build", not as tampering;
   - for a keyed version, `conn.QueryChainLen` vs `result.HeadSeq` first, for a
     precise message;
   - then the keyed stamp, computed **with the stored version** and with the
     length/head *derived from the surviving statements*;
   - only if that fails, and only when the stored version is `0`, the legacy
     raw-`HeadMAC` comparison — which sets `LegacyStamp` on the result instead
     of passing silently.

   Checking the keyed stamp *before* branching to legacy is what makes the
   version-in-the-MAC load-bearing: relabelling a sealed row as version `0`
   while keeping its stamp is a **break**, because the stamp no longer seals
   the version the row now claims. Without the version in the MAC that
   relabelling would verify clean, which is the downgrade option 1 could not
   see.

7. **`legacy_stamps`** — `QueryChainResult.LegacyStamp` (bool) and
   `QueryChainsResult.LegacyStamps` (count), surfaced by
   `dbbat audit verify --queries` and by `GET /api/v1/audit/verify/queries`
   (`internal/api/openapi.yml` + regenerated front client).

8. **Tests** in `internal/store/chain_test.go`: the re-stamping attacker; the
   version relabel (the mutation-sensitive one); the full downgrade to a raw
   legacy stamp, which must not count as a keyed verification; a genuine legacy
   row still verifying (no cry-wolf) and being counted; the length column
   edited on its own; the orphan reconcile stamping version 1; and the reworked
   cost guard.

### Residual, stated rather than hidden

A full downgrade — replacing the whole stamp with a raw head MAC *and* setting
the version to `0` — still verifies, because accepting unkeyed legacy stamps is
by construction a path an attacker can take. What version `1` buys is that the
attack can no longer hide: the row is reported as a legacy stamp, so an
operator watching `legacy_stamps` fall to zero as sessions rotate sees it come
back. The hole closes for good only when version `0` acceptance is removed; a
follow-up todo picks the release.
