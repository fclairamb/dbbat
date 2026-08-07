# Approval patterns starting with `(` are corrupted on read-back

## Goal

Make a grant definition's `approval_patterns` (and every other `text[]` column)
survive the store round-trip when an element starts with `(` — the canonical
`(?i)…` form the UI placeholder and `docs/approvals.md` both teach.

## Why

Found while implementing the SQL Server ATTENTION-during-a-hold spec. A
definition created with

```go
ApprovalPatterns: []string{`(?i)^DELETE`}
```

is stored correctly — PostgreSQL holds `{(?i)^DELETE}` — but reads back as

```go
[]string{"(?i)", "^DELETE"}
```

The cause is upstream, in `uptrace/bun`'s PostgreSQL array parser
(`dialect/pgdialect@v1.2.18/array_parser.go`, `readNext`): an element whose
first byte is `(` or `[` is parsed as a **range/composite** and terminated at
the matching `)`, so the rest of the element becomes a second element. Only
elements that *start* with `(` are affected — `X(?i)Y`, `a,b` and `he said "hi"`
all round-trip fine.

Reproduced against a real PostgreSQL (testcontainers): insert
`{(?i)FOO, X(?i)Y, A)^B, ?i)Z, (?s)^X}`, read back
`{(?i), FOO, X(?i)Y, A)^B, ?i)Z, (?s), ^X}`.

The impact is not cosmetic. `(?i)` on its own is a regexp that matches **every
statement**, so a definition whose only pattern is `(?i)^DELETE` puts an
approval hold on *every query the grant runs* — and the surviving `^DELETE` half
is now case-sensitive, which is exactly what the `(?i)` was there to prevent.
It fails closed rather than open, but it makes the documented pattern form
unusable in practice, and any operator who followed the docs has a grant
definition that holds everything.

Other `text[]` columns on the same model have the same exposure: `controls`
(values are fixed keywords, so safe in practice), `sample_queries` (a sample
starting with `(` would split), and any future one.

## Implementation

Confirm the parser is still wrong in the newest `bun`, then pick one:

1. **Upgrade / upstream fix.** Check whether a later `bun` release fixes
   `arrayParser.readNext`; if not, file it upstream. This is the right long-term
   answer — the bug affects every `text[]` a bun user has.
2. **Stop relying on bun's array codec for these columns.** Store the patterns
   as `jsonb` (a migration on `grant_definitions.approval_patterns` plus the
   `bun:"...,array"` tags in `internal/store/models.go`), which has an
   unambiguous encoding and no range parsing. Note the archived-version rows:
   the migration has to convert existing arrays, not just the live ones.
3. **Encode defensively.** Wrap the slice in a `driver.Valuer` /
   `sql.Scanner` pair of dbbat's own that quotes every element on the way out
   and parses PostgreSQL array syntax properly on the way in.

Whichever it is, the regression test belongs next to the store, not in a proxy
package: insert a definition with `(?i)^DELETE`, `[abc]`, `(a|b)` and assert the
slice comes back with three elements.

Two follow-ons once it is fixed:

- `internal/proxy/mssql/intercept_test.go` has tests using `(?i)^DELETE` that
  only pass *because* the corrupted `(?i)` matches everything
  (`TestSessionParksAStatementOnAnApprovalHold`,
  `TestApprovalHoldMatchesAPreparedStatement`), plus one that deliberately uses
  `^DELETE` with a comment pointing here
  (`TestAttentionCancelsAStatementParkedOnAnApprovalHold`). The other proxies'
  approval tests should be swept for the same. They will need re-reading, and
  some may start failing for the right reason.
- Existing rows in deployed databases are already split. A data fix (or at least
  an operator-facing note) is needed, since re-joining `["(?i)", "^DELETE"]`
  into one pattern is only unambiguous because the split is deterministic.

Files: `internal/store/models.go` (`GrantDefinition.ApprovalPatterns`),
`internal/store/grant_definitions.go`, `internal/migrations/sql/`,
`docs/approvals.md`.

No GitHub issue exists yet — one should be filed.
