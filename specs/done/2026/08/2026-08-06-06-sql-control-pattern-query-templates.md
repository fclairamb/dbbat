---
model: sonnet
effort: medium
---

# Pattern authoring is blind — let operators load SQL query templates to validate their control patterns against

## Problem

SQL control patterns (the RE2 `ApprovalPatterns` on grants and grant
definitions, [models.go:512](internal/store/models.go:512) and
[models.go:598](internal/store/models.go:598)) are written blind. An operator
types a regex into the grant-definition form
([grant-definitions/index.tsx](front/src/routes/_authenticated/grant-definitions/index.tsx))
and only finds out whether it does what they meant when a live statement
hits — or fails to hit — the proxy.

Two things make this worse than ordinary regex-authoring pain:

- Matching runs against **normalized** SQL, not the raw text the client sent:
  `ApprovalGate.Match` applies `NormalizeSQL` before testing
  ([approval.go:220-231](internal/proxy/shared/approval.go:220)), and the
  static validators do the same
  ([validation.go:131](internal/proxy/shared/validation.go:131)). An operator
  has no way to see the normalized form their pattern must match.
- An invalid pattern is **silently skipped** at gate construction with only a
  server-side warning log ([approval.go:194-203](internal/proxy/shared/approval.go:194)),
  so a typo'd pattern degrades to "no hold" without the author ever knowing.

The ask: when defining SQL control patterns, allow loading **SQL query
templates** — representative sample statements — and validate the patterns
against them before saving. The author should be able to assemble a small
test bench ("these queries must hold, these must pass through") instead of
shipping an untested regex into an access-control path.

## Proposal

1. **Validation endpoint** — e.g. `POST /api/v1/grant-definitions/validate-patterns`
   (name open), body `{patterns: [...], queries: [...]}`. For each pattern:
   report compile errors (surfacing what [approval.go:196](internal/proxy/shared/approval.go:196)
   currently swallows); for each query: the `NormalizeSQL` output and which
   pattern (first match, same semantics as `ApprovalGate.Match`) it matches.
   Pure function over `internal/proxy/shared` — no DB access needed. Update
   `internal/api/openapi.yml` and the route-parity test.
2. **Loading templates** — the "load" part of the ask. Sources, in likely
   order of value:
   - Paste/edit sample statements directly in the dialog (baseline).
   - Import from **query history**: pick recent queries logged for the same
     database/definition as ready-made templates (the store already has the
     data; an existing queries listing endpoint may suffice).
   - Possibly a reusable saved set (template library) per definition so the
     bench survives edits — see open questions.
3. **UI** — in the grant-definition create/edit dialog, a "Test patterns"
   panel: template list + per-template match/no-match result and the
   normalized SQL, plus inline compile errors on each pattern field.
4. **Tests** — API handler test for the validation endpoint (compile error,
   match, no-match, normalization visible); front e2e coverage if the dialog
   flow is non-trivial.

### Open questions

- The originating description is one line ("when defining SQL control
  patterns for validation we should allow to load SQL query templates").
  The reading above — templates as *test fixtures* for pattern authoring —
  is the most literal. An alternative reading is templates as *first-class
  matchers* (define a parameterized SQL template instead of a regex, matched
  by fingerprint). That would be a much bigger feature touching the proxy hot
  path; confirm intent before scoping it in.
- Are the loaded templates ephemeral (dialog-only) or persisted with the
  definition as a regression bench that re-runs on every edit? Persisting
  pairs naturally with the definitions-as-source-of-truth direction of
  [2026-08-06-04-grants-reference-definitions-only.md](specs/todos/2026-08-06-04-grants-reference-definitions-only.md).
- No GitHub issue exists yet — one should be filed.

## Resolved open questions

Answered by the repository owner, 2026-08-06. Binding.

**Interpretation → test fixtures, not first-class matchers.** RE2 patterns remain the
matching mechanism; the templates are sample statements you validate those patterns
against. The proxy hot path is not touched by this spec.

**Where the samples live → in the query-matching config itself.** The owner's words: "sample
values to put in the query matching config for validation". So the samples are a persisted
field on the grant definition, sitting next to `approval_patterns`
([models.go:598](internal/store/models.go:598)) — not a dialog-only scratchpad, and not a
separate bench entity with its own table.

Concretely:

- Add a `sample_queries` (string array) column to `grant_definitions`, alongside
  `approval_patterns`, with a migration. It is part of the definition's matching config.
- **This spec runs after [2026-08-06-04](2026-08-06-04-grants-reference-definitions-only.md)**,
  which makes grant definitions immutably versioned (an edit archives the current row and
  inserts a new one). Because the samples are just another column on that row, they version
  along with it for free — a definition's saved samples always describe the patterns of that
  exact version. Build on the versioned model; do not add a parallel versioning scheme.
- The validation endpoint stays a pure function over `internal/proxy/shared` as proposed:
  compile errors per pattern (surfacing what
  [approval.go:196](internal/proxy/shared/approval.go:196) currently swallows), and per
  sample the `NormalizeSQL` output plus which pattern matches first, matching
  `ApprovalGate.Match` semantics exactly.
- The dialog validates the patterns against the saved samples when the definition is saved,
  so a typo'd pattern is caught at authoring time instead of degrading silently to "no hold".
  Do not make a failing sample block the save outright unless the pattern fails to *compile*
  — an author may legitimately save a bench that is currently red while iterating; surface
  it loudly instead.

Whether the "import from query history" source in Proposal step 2 lands in this spec is left
to the implementer's judgement: paste/edit is the baseline and must work; history import is
worth doing only if the existing queries listing endpoint makes it cheap. Say which you did.
