# "New Definition" dialog retains the previously-submitted values

## Goal

On `/app/grant-definitions`, after an admin creates a definition and then
clicks **New Definition** again without leaving the page, the form should open
blank. Today it re-opens pre-filled with the values just submitted.

## Why

Follow-up discovered while fixing the edit-dialog-opens-empty bug
(`specs/todos/2026-07-12-grant-definition-edit-empty-form.md`).

The edit fix keys the dialog on `editing?.uid ?? "new"`
(`front/src/routes/_authenticated/grant-definitions/index.tsx`). That remounts
the dialog whenever the target changes — Edit A → New and Edit A → Edit B both
work. But two consecutive **New** opens share the same `"new"` key, so the
`DefinitionDialog` instance is *not* remounted and its `useState` values (name,
description, controls, quotas) survive from the previous create session.

Impact is minor (values were just submitted; the admin can clear them), and it
only bites when creating several definitions back-to-back without navigating —
but the form ideally starts blank each time.

## Implementation

Reset the create form when the dialog transitions closed → open with
`editing == null`. Options, cheapest first:

1. Only mount `DefinitionDialog` while the dialog is open, so it always remounts
   fresh: wrap it in `{dialogOpen && (...)}` (keeps the existing
   `key={editing?.uid ?? "new"}`). The `DialogTrigger` stays outside the guard.
2. Or derive a per-open key (e.g. bump a counter in `onOpenChange` when opening
   for New) so each New open gets a distinct key.

Add an E2E path in `front/e2e/grant-definitions.spec.ts`: create a definition,
then open **New Definition** again and assert the name/description/controls are
blank (note: the existing "Edit A then New" test already asserts blank, but via
the uid→"new" key change, which does not exercise the New→New path).

No GitHub issue filed yet — one should be created and linked here.

## Implementation Plan

1. Adopt Option 1 (cheapest): only mount `DefinitionDialog` while the dialog is
   open. In `front/src/routes/_authenticated/grant-definitions/index.tsx`, wrap
   the `<DefinitionDialog .../>` in `{dialogOpen && (...)}`, keeping the existing
   `key={editing?.uid ?? "new"}`. The `DialogTrigger` stays outside the guard so
   the button still renders. Because the component unmounts on close and remounts
   on every open, its `useState` (name, description, controls, quotas) always
   starts fresh — New→New now opens blank while Edit A→New and Edit A→Edit B stay
   correct.
2. Extend `front/e2e/grant-definitions.spec.ts` with a New→New test: create a
   definition, reopen **New Definition**, and assert name/description/controls
   are blank (the existing Edit→New test only exercises the uid→"new" key path).
3. QA: `make build-front` and `cd front && bun run lint` must pass with no new
   errors in touched files. Run E2E if the local devloop is in test mode.
