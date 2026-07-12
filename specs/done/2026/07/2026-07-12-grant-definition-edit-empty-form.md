# Grant-definition edit dialog opens empty instead of loading current values

## Goal

When an admin clicks the pencil icon on a grant definition
(`/app/grant-definitions`, e.g. https://dbbat.tools.stonal.io/app/grant-definitions),
the "Edit definition" dialog must be pre-filled with the definition's current
values (name, description, duration, access controls, max queries, max data
transfer). Today it opens as an empty form, so "editing" silently means
re-typing everything — and saving without noticing overwrites the definition
with blank/default values.

Also add an E2E test that locks this behaviour in.

## Why

Classic stale-`useState`-initializer bug in
`front/src/routes/_authenticated/grant-definitions/index.tsx`:

- `DefinitionDialog` is rendered **unconditionally** inside the `<Dialog>`
  (`index.tsx:219-225`), so it mounts once when the page renders, while
  `editing` is still `null`.
- All its form fields seed their state from the `editing` prop via `useState`
  initializers — `useState(editing?.name ?? "")`, the duration value/unit
  derivations, `controls`, `maxQueries`, `maxBytesValue`, `bytesUnit`
  (`index.tsx:272-301`).
- `useState` initializers only run on first mount. When the user clicks Edit,
  `setEditing(d); setDialogOpen(true)` (`index.tsx:180-181`) updates the prop,
  but the already-mounted component never re-reads it → the form shows the
  empty "new definition" defaults.

No GitHub issue filed yet — one should be created and linked here.

## Implementation

1. **Fix** — force a remount of `DefinitionDialog` whenever the target
   changes, so the initializers re-run with the right `editing` value:

   ```tsx
   <DefinitionDialog
     key={editing?.uid ?? "new"}
     editing={editing}
     onClose={...}
   />
   ```

   This is the idiomatic React fix (state keyed to identity) and also covers
   the sibling staleness paths: Edit A → close → New (must be blank), and
   Edit A → close → Edit B (must show B). Alternative: only mount the dialog
   content while `dialogOpen` is true — the `key` approach is smaller and
   keeps the `DialogTrigger` wiring untouched.

2. **E2E test** — no spec covers grant definitions yet; add
   `front/e2e/grant-definitions.spec.ts` (pattern: `front/e2e/grants.spec.ts`,
   authenticated fixture from `front/e2e/fixtures.ts`):
   - As admin, create a definition with distinctive values (name, description,
     duration, a couple of access controls, max queries, max bytes) via
     `create-grant-definition-button`.
   - Click `edit-grant-definition-<uid>` and assert every field of the dialog
     shows the saved values — this is the regression assertion that fails
     today.
   - Change one field, save, re-open and verify the edit stuck and the other
     fields survived.
   - Bonus staleness paths: after editing A, open "New Definition" and assert
     the form is blank; with two definitions, open Edit on A then B and assert
     B's values load.

3. While in there, check the other resource pages for the same
   mounted-dialog + `useState(prop)` pattern (users, databases, grants) and
   fix/backlog any that share it.

## Implementation Plan

Concrete steps taken:

1. **Fix** — add `key={editing?.uid ?? "new"}` to the `<DefinitionDialog>`
   mount in `front/src/routes/_authenticated/grant-definitions/index.tsx`
   (inside the `<Dialog>`). Remounting re-runs every `useState` initializer
   with the current `editing`, so Edit A, Edit A→New, and Edit A→Edit B all
   seed correctly.

2. **Audit of sibling pages** (users, databases, grants):
   - `users/index.tsx` — `EditUserDialog` is already conditionally mounted and
     keyed: `{editUser && <EditUserDialog key={editUser.uid} .../>}`. No bug.
   - `databases/index.tsx` — only a create-only `CreateDatabaseDialog`; the
     `DatabaseDetailsDialog` is a read-only details view, not a
     `useState(prop)` form. No bug.
   - `grants/index.tsx` — only a create-only `CreateGrantDialog`. No bug.
   Grant-definitions was the sole offender; no follow-up spec needed.

3. **E2E** — add `front/e2e/grant-definitions.spec.ts` (authenticated fixture):
   create a definition with distinctive values, reopen via
   `edit-grant-definition-<uid>` and assert every field is prefilled (the
   regression assertion), edit one field + save + reopen to confirm it stuck,
   plus staleness paths (Edit A→New blank, Edit A→Edit B loads B). The needed
   `data-testid` hooks (`create-grant-definition-button`,
   `edit-grant-definition-<uid>`, `grant-definition-name`, `-submit`) already
   exist; added stable testids to the remaining dialog fields as needed.
