# Clean up the fabricated binary query errors already stored

## Goal

Null out the `queries.error` values that were never Oracle diagnostics — raw
column-compressed row bytes written by the legacy TTC Response decoder before
the fix in `2026-08-05-oracle-legacy-response-false-error.md`.

This is a **manual, human-run data change on a live database**. It is written
down here rather than executed because no automation should mutate production
query history unattended.

## Why

Item 5 of `specs/todos/2026-08-05-oracle-legacy-response-false-error.md`. The
decoder fix stops new rows being written, but the historical rows still make
healthy queries read as failed in the UI and the API (one production example:
query `019fcf4e-6774-7293-9f3f-ac023d2a6f4f`, a 57.9 s `SELECT` carrying a
772-byte binary "error"). Volume is low — roughly 1 in 500 recent queries.

No GitHub issue filed yet; one should be, together with the parent spec.

## Implementation

Inspect first, on the target instance:

```sql
SELECT count(*) FROM queries WHERE error !~ '^[[:print:][:space:]]*$';
SELECT uid, executed_at, left(error, 60) FROM queries
 WHERE error !~ '^[[:print:][:space:]]*$'
 ORDER BY executed_at DESC LIMIT 20;
```

Then, once the sample confirms they are all binary noise:

```sql
UPDATE queries SET error = NULL WHERE error !~ '^[[:print:][:space:]]*$';
```

Notes:

- The predicate matches the guard now enforced in code
  (`shared.SanitizeQueryError`: valid UTF-8, no control bytes), so it will not
  touch a genuine multi-line diagnostic.
- Run it per instance; there is no migration for it. A migration would apply
  the same predicate to every deployment's history, which is a heavier hammer
  than a low-volume, opt-in clean-up needs.
- `rows_affected` on the affected rows is also wrong (truncated at the misparse
  point) and cannot be recovered — leave it as is.

## Resolved open questions

**Should this stay a human-only runbook, or may automation run it against the
real production dbbat database?**

Decision (2026-08-07, repository owner): **run it against production.** This
overrides the "no automation should mutate production query history unattended"
note above — the owner has explicitly authorised it for this run. Procedure:

1. Run the two inspection queries first and show the operator the counts and the
   20-row sample before mutating anything.
2. Only then run the `UPDATE`.
3. Report the number of rows affected.

**Should a GitHub issue be filed for this spec?**

Decision: **no.** Do not run `gh issue create`. The spec file is the record.
