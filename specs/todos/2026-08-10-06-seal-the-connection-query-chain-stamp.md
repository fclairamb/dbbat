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
