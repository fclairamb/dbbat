---
model: opus
effort: medium
pr: batch
automerge: false
---

# Approval patterns can be evaded by putting SQL comments in a statement

## Problem

Found by the security scan of 2026-11-16 (finding M1). Severity: Medium.
Any authenticated proxy user whose grant definition carries approval patterns
can trigger it, on all five protocols. It matters with `DBB_APPROVAL_ENABLED`
on, which is off by default.

An approval hold is the four-eyes control on a grant that otherwise allows
writes: a statement that matches one of the definition's RE2 patterns is
parked until a second human releases it. The gate matches the **raw client
text**, and only trims it:

- `ApprovalGate.Match` (`internal/proxy/shared/approval.go:233-239`) calls
  `CompiledApprovalPatterns.Match` (`:356-370`), which applies `NormalizeSQL`
  (`:254-256`, `strings.TrimSpace` and nothing else).
- The call sites hand it the client's bytes:
  `internal/proxy/postgresql/approval.go:151`,
  `internal/proxy/mysql/intercept.go:473`,
  `internal/proxy/oracle/intercept.go:1597`,
  `internal/proxy/mssql/intercept.go:518`,
  `internal/proxy/mongodb/intercept.go:393`.

The static validators, by contrast, run on `matchableSQL`
(`internal/proxy/shared/sqlcomments.go:52`), which strips comments with the
dialect's comment syntax. The doc comment on `ApprovalGate.Match` even says
matching "runs on the same normalized SQL text the static validators use",
which is not true.

So a pattern written the way `docs/approvals.md` and the definition form
teach, e.g. `(?i)DELETE\s+FROM\s+orders`, matches `DELETE FROM orders` but not
the same statement with a block comment or a line comment between the
keywords. The database runs both the same way. Checked against `main` with a
validator-level test: the plain spelling matches, both commented spellings do
not. A user can therefore run the very statement the hold exists for, with
no human decision. The `queries` row shows it after the fact.

Case is a separate matter: `NormalizeSQL` documents that patterns must carry
`(?i)`. That is the operator's choice and is out of scope here. Comments are
not something an operator can reasonably defend against in a regexp.

## Proposal

1. In `CompiledApprovalPatterns.Match` (`internal/proxy/shared/approval.go:356`),
   test every pattern against **both** the trimmed raw text (current
   behaviour, so a pattern that matches what `/queries` shows still matches)
   **and** a comment-stripped copy with runs of whitespace collapsed to a
   single space. Hold if either matches. Holding on either can only add holds,
   never remove one.
2. The comment syntax is per dialect (`syntaxStandard`, `syntaxMySQL`, …).
   Either give `Match` a dialect argument, threaded from each `ApprovalGate`
   (the gate knows its protocol when it is built), or strip with the most
   inclusive syntax (standard plus `#` line comments). Prefer the dialect
   argument: MySQL's `/*! … */` is executed, not ignored, and must stay in
   the text the way `matchableSQL` keeps it.
3. MongoDB has no comments in the extended-JSON text it matches; keep its
   behaviour, but route it through the same function so the paths don't
   drift.
4. Make the "validate patterns" API endpoint that shares
   `CompiledApprovalPatterns.Match` report the same answer. It already
   delegates, so this follows from step 1.
5. Fix the doc comment on `ApprovalGate.Match`, and state the rule in
   `docs/approvals.md`: patterns are matched against the statement as sent
   and against it with comments removed.

## Tests

- `internal/proxy/shared/approval_test.go`: the pattern
  `(?i)DELETE\s+FROM\s+orders` must match the plain statement, the same
  statement with `/**/` between the keywords, with `/* x */`, and with a `--`
  line comment and newline between them. The commented cases fail before the
  fix.
- MySQL dialect: a `#` line comment between the keywords matches. A
  `/*!` versioned comment is kept as text.
- Positive control: a statement that only *mentions* the pattern inside a
  string literal still matches the raw text as today (no behaviour lost). An
  unrelated statement (`SELECT 1 /* DELETE FROM orders */`) gives the same
  answer as before the fix. Write the expected value down explicitly; see the
  open question.
- One proxy-level test (PostgreSQL is enough) showing a commented statement
  is parked by the gate.

## Open questions

- A comment that *contains* the pattern text (`SELECT 1 /* DELETE FROM
  orders */`) matches the raw text today and so is held. Keep that
  (recommended: yes, an over-hold is safe and the operator sees why), or
  match only the stripped copy?
