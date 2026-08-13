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

## Resolved open questions

> Strip comments before matching, or refuse a statement whose comments sit
> between keywords?

**Decision (2026-08-13): neither — normalise a scratch copy used only for
matching.** The owner was asked directly during an `/implement-todos` batch and
chose the third option below.

Directives for the implementer:

- Build the normalised form into a **scratch string**. Both `oracleBlockedPatterns`
  and the `IsWriteQuery`/`IsDDLQuery` classification match against that scratch
  copy. **The statement relayed upstream stays byte-identical** — never relay the
  stripped form. Optimizer hints (`/*+ INDEX(t i) */`) must still reach the
  database exactly as the client wrote them; they simply stop being an evasion
  channel for the matchers.
- The stripper must be **literal-aware**: `'…'` (with `''` escaping) and Oracle's
  `q'[…]'` / `q'{…}'` / `q'(…)'` / `q'<…>'` quote-operator forms. A `/*` or `--`
  *inside* a literal is not a comment and must be preserved. Getting this wrong
  turns a parser bug into an authorization bug, so it is the part to test
  hardest.
- Replace each comment with a **single space**, not the empty string, so
  `ALTER/**/SESSION` normalises to `ALTER SESSION` and matches, rather than
  collapsing to `ALTERSESSION` and escaping again.
- Handle both comment syntaxes Oracle accepts: `/* … */` (including unterminated,
  which runs to end of input) and `-- …` to end of line. MySQL additionally
  accepts `# …` and requires whitespace after `--`; cover that when the shared
  validators are reached from the MySQL path.
- Fail **closed** on anything the stripper cannot make sense of (an unterminated
  literal, say): treat the statement as un-normalisable and let the matchers run
  against the raw text rather than silently passing a statement nothing checked.

## Superseded alternatives

The two options originally posed, kept for the record — do **not** implement
either:

- **Strip the statement in place and relay the stripped form.** Rejected: hints
  are comments, so this can silently change query plans on real workloads.
- **Refuse a statement whose comments sit between keywords.** Rejected: it would
  reject legitimate traffic for the same reason — `/*+ INDEX(…) */` is common.

## Implementation

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
