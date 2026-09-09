---
model: opus
effort: high
---

# One retention window governs both queries and connections; they need separate TTLs

## Problem

`DBB_QUERY_STORAGE_RETENTION` is the only history TTL, and it reaps two very
different things with one cutoff
([internal/store/queries.go:952](internal/store/queries.go:952)):

1. **closed `connections`** whose `disconnected_at` is past the cutoff, which
   cascade to their `queries` and `query_rows`;
2. **`queries`** whose `executed_at` is past the cutoff and that still hang off
   a surviving (open) connection.

The two carry different weight and answer different questions:

- `queries` + `query_rows` are the bulk of the store: statement text and up to
  `max_result_rows`/`max_result_bytes` of captured results per statement. This
  is what an operator wants to expire after 30 or 90 days, for cost and for
  data-minimisation (captured rows can hold customer data).
- `connections` are one small row per session: who, from where, to which
  database, under which grant, when, plus lifetime counters (`queries`,
  bytes). This is the **session ledger** — the thing a security review asks for
  a year later ("who touched prod in March?") — and it costs almost nothing to
  keep.

Today an operator cannot have both. Setting `720h` deletes the ledger with the
statements; leaving it at `0` keeps every captured row forever. The
`connection.opened` / `connection.closed` audit entries survive a sweep
([internal/store/connection_audit.go](internal/store/connection_audit.go)),
but they are chained `audit_log` rows, not something the connections UI or
`GET /api/v1/connections` can list, filter or link to, so they are evidence
"by comparison", not a usable ledger.

This spec is about **cleanup**, i.e. deletion, on two independent TTLs. A
connection whose statements have been reaped but whose own row is still
retained is simply a session record with lifetime counters and no statement
bodies; nothing is exported or moved elsewhere before a row is deleted.

## Proposal

### Two windows, ordered

Keep `DBB_QUERY_STORAGE_RETENTION` (`query_storage.retention`) with its exact
meaning for **queries and their rows**. Add a second setting for the ledger:

| Variable | Config key | Meaning | Default |
|---|---|---|---|
| `DBB_CONNECTION_RETENTION` | `connection.retention` | Delete **closed** connections (cascading to whatever queries/rows they still have) once `disconnected_at` is older than this Go duration | unset → **same as the query window** |

Rules, all enforced in `internal/config`:

- **Unset means "as today".** The connection window inherits the query window,
  so an upgraded deployment sweeps exactly what it swept before. This is the
  backward-compatibility rule and the reason the default is not `0`.
- **The connection window must be ≥ the query window.** Deleting a connection
  cascades to its queries, so a shorter connection window would delete
  statements *before* the query TTL the operator wrote down — "delete more
  than asked", which this codebase treats as the one unacceptable failure mode
  (`QueryStorageConfig.RetentionDuration` already disables the sweep on a typo
  for that reason, [internal/config/config.go](internal/config/config.go)).
  A connection window shorter than the query window is therefore a
  **misconfiguration that disables both sweeps** with a startup WARN naming
  both values, mirroring `RetentionMisconfigured`. Not a startup failure: the
  existing precedent is "keep everything and warn", and a proxy that refuses
  to start over a retention typo is a worse outcome than one that keeps history.
- **Query window `0` (forever) forces the connection window to `0`.** If
  statements are kept forever their parent rows must be too; an explicit
  non-zero connection window with queries at `0` is the same misconfiguration
  and gets the same WARN.
- **`0` explicitly on the connection window with queries > 0 is valid** and is
  the interesting case: reap statements after N days, keep the ledger forever.

### Store: two cutoffs, one sweep

Change `Store.CleanupOldQueryRows(ctx, olderThan)` to take both windows
(`queryOlderThan, connectionOlderThan time.Duration`) and run its two existing
batched deletes against **different cutoffs**, still in the same order
(connections first, then queries) so a closed connection past the ledger window
never survives as a shell after its own reaping pass, exactly as the comment on
the method promises today. `RetentionSweepResult` and `deleteInBatches` stay as
they are; the sweeper in [retention.go](retention.go) logs both windows.

The consequence to document loudly, because it inverts an assumption the
current comment states: with two windows, a **closed** connection can now
outlive all of its queries. Today only an *open* session can. Every place that
reasons "closed connection with no queries ⇒ something deleted them by hand"
must be revisited:

- `Store.checkEmptiedChain` / `retentionCouldEmpty`
  ([internal/store/chain_verify.go:751](internal/store/chain_verify.go:751))
  already excuse a session on `connected_at < now - queryRetention`, and
  `store.Options.QueryRetention` must keep receiving the **query** window, not
  the connection one — the query window is what empties chains. With the split
  this "excused, counted as truncated prefix" state becomes the *normal*
  condition of every session between the two windows rather than a rare edge, so the verify
  report's `chains_with_retention_truncated_prefix` should be relabelled or
  split so an operator can tell "statements reaped by design" from "a live chain lost
  its head". The `docs/audit-chain.md` paragraphs at lines ~421–460 that
  describe the excuse need rewriting around the two windows.
- The UI connection detail (`front/src/components/shared/ConnectionQueryFeed.tsx`)
  shows an empty feed for such a session. It should say the statements are
  past retention (the `queries` counter is a lifetime count, already documented
  as such) rather than an ambiguous "no queries". Expose enough for that:
  either the two windows on the existing public config/health payload the
  front already reads, or a derived `statements_retained: false` on the
  connection resource. Pick whichever the front already has a hook for; do not
  add a new endpoint for one boolean.

### Wiring and docs

- `internal/config`: `ConnectionConfig{Retention string}` alongside
  `QueryStorageConfig`, env → key mapping next to the `query_storage_` prefix
  rule ([internal/config/config.go:1040](internal/config/config.go:1040)),
  the inherit/ordering rules above, and a `RetentionMisconfigured`-style
  accessor the sweeper can log from. Tests mirror
  `internal/config/query_storage_retention_test.go`: default inherits, explicit
  `0`, shorter-than-queries disables both, queries `0` + connections set
  disables both.
- `retention.go` / `main.go:260,362,1672`: pass both durations; keep
  `store.Options.QueryRetention` = query window.
- Store tests in `internal/store/queries_retention_test.go`: a connection
  closed 40d ago with queries survives a (30d queries, 365d connections) sweep
  with **zero** queries; a connection closed 400d ago is gone entirely; an open
  connection is still never touched; equal windows reproduce today's exact
  result counts.
- Docs: env table in `CLAUDE.md` and `README.md:231`, the retention section of
  `website/docs/features/query-logging.md` ("Retention"), the config reference
  at `website/docs/configuration/index.md:241`, and `docs/audit-chain.md`.
  Session dumps keep their own `DBB_DUMP_RETENTION`; note that the uploaded
  capture's object key lives on the connection row, so a ledger window shorter
  than the bucket lifecycle orphans objects (they remain, but nothing can find
  them without a LIST).

## Resolved open questions

Answered by the user on 2026-09-09. These are decisions, not options — implement
them as written.

- **Q: "Should `connection.closed` audit entries be surfaced as the ledger instead?
  They already outlive both windows. If the answer is 'yes, add a listing', the
  connection window could default to the query window permanently and this spec
  shrinks to the config split. Decide before implementing the UI half."**

  **Decision: no.** Do **not** surface `connection.closed` audit entries as the
  ledger, and do **not** shrink this spec to the config split. Build the full
  two-window design above, including the UI half: `connections` (which
  `GET /api/v1/connections` and the front already read) stays the ledger, and it
  gets its own independent TTL. Add no audit-entry listing endpoint in this spec.

- **Q: "Naming: `DBB_CONNECTION_STORAGE_RETENTION` follows `DBB_QUERY_STORAGE_*`;
  `DBB_CONNECTION_RETENTION` is shorter but breaks the `<table>_storage` pattern.
  Either is fine; pick one and keep it."**

  **Decision: `DBB_CONNECTION_RETENTION`.** Config key `connection.retention`,
  Go type `ConnectionConfig{Retention string}`. Because this deliberately does
  **not** follow the `<table>_storage` shape, the env → key mapping needs its own
  case rather than riding the `query_storage_` prefix rule
  ([internal/config/config.go:1040](internal/config/config.go:1040)) — add a test
  that `DBB_CONNECTION_RETENTION` actually reaches `connection.retention`, since a
  silently-unmapped env var would read as "unset" and inherit the query window,
  which is exactly the failure this spec exists to prevent. Use this name
  everywhere: config, sweeper logs, `CLAUDE.md`, `README.md`, and the website docs.
