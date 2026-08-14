# An inline SQL comment evades every `oracleBlockedPatterns` entry

**No GitHub issue filed yet — one should be.** (Automation must not run
`gh issue create`; see `specs/todos/2026-08-11-06-*.md`.)

## Goal

Decide how `ValidateOracleQuery` should handle SQL comments before it runs
`oracleBlockedPatterns`, so that a statement cannot walk through an outright
block by putting `/**/` between two keywords.

## Why

Found while auditing `2026-08-13-14` (the `ALTER SESSION SET CONTAINER` block).
The patterns match the raw statement — `ValidateOracleQuery` passes `sql`
straight to `pattern.MatchString` with no normalisation
(`internal/proxy/shared/validation.go`) — and Oracle ignores `/* … */` between
keywords. Measured on the current tree:

| statement | blocked today |
|---|---|
| `ALTER SESSION SET CONTAINER=PDB2` | yes |
| `ALTER SESSION /* x */ SET CONTAINER=PDB2` | **no** |
| `ALTER/**/SESSION SET CONTAINER=PDB2` | **no** |
| `ALTER SYSTEM /* x */ FLUSH SHARED_POOL` | yes (comment falls outside the two-keyword pattern) |

This is **pre-existing and class-wide**, not a regression from `2026-08-13-14`:
every multi-keyword entry in the list has it. `ALTER\s+SYSTEM` is only narrowly
safe because it spans two adjacent keywords, and `ALTER/**/SYSTEM` evades it
just the same. `CREATE\s+DATABASE\s+LINK` spans three and is the softest of the
lot.

It matters more now than it did: `CONTAINER` was promoted to an *outright*
block precisely because a full-write grant could otherwise step outside the
database its grant covers, and an outright block that a comment defeats is not
one. The same reasoning reaches `ValidateQuery`'s read_only/block_ddl
classification (`IsWriteQuery`/`IsDDLQuery`), which is regex-shaped too — a
comment before the leading keyword changes what a statement looks like to
dbbat but not to Oracle.

## Open questions

> Strip comments before matching, or refuse a statement whose comments sit
> between keywords?

Stripping is the obvious move but is not free: a correct stripper has to know
Oracle's string literals (`'…'`, `q'[…]'`) so that a `/*` *inside* a literal is
not treated as a comment, and getting that wrong turns a parser bug into an
authorization bug. Refusing is cruder and would reject legitimate traffic —
optimizer hints (`/*+ INDEX(…) */`) are comments and are common in real
workloads, so a blanket refusal is not viable.

A third option is to normalise only for the *matching* pass (strip to a scratch
copy used for the regexes, relay the original untouched), which keeps hints
working on the wire while denying them as an evasion channel.

**This needs a decision before implementation.**

## Implementation

Sketch, pending the decision above:

- `internal/proxy/shared/validation.go`: normalise into a scratch string —
  comments to a single space, literals preserved verbatim — and run both
  `oracleBlockedPatterns` and the `IsWriteQuery`/`IsDDLQuery` classification
  against it. The statement relayed upstream stays byte-identical.
- Check the same exposure on the other four protocols: the shared
  `IsWriteQuery`/`IsDDLQuery` are used by all of them, and MySQL additionally
  accepts `-- ` and `#` line comments, MongoDB none.
- Tests in `internal/proxy/shared/validation_test.go`, next to
  `TestValidateOracleQuery_BlocksAlterSessionContainer`: the four rows of the
  table above, a hint (`SELECT /*+ INDEX(t i) */ …`) that must stay allowed, and
  a literal containing `/*` that must not be mistaken for a comment.
