# Editing a grant definition opens an empty "create" form instead of the existing values

## Problem

On `/app/grant-definitions`, clicking the ✏️ edit (Pencil) button on a row opens
the dialog but the fields are **blank / defaulted** — as if creating a new
definition — instead of being pre-filled with the row's current name,
description, duration, controls, and quotas. The user cannot see or edit the
existing content.

Root cause is in
[`front/src/routes/_authenticated/grant-definitions/index.tsx`](front/src/routes/_authenticated/grant-definitions/index.tsx).
`DefinitionDialog` seeds every form field from the `editing` prop using
`useState` **initializers**:

```tsx
// lines 272–305
const [name, setName] = useState(editing?.name ?? "");
const [description, setDescription] = useState(editing?.description ?? "");
const [durationValue, setDurationValue] = useState<string>(() => { ... editing ... });
const [durationUnit, setDurationUnit] = useState<"m"|"h"|"d">(() => { ... });
const [controls, setControls] = useState<string[]>(editing?.controls ?? []);
const [maxQueries, setMaxQueries] = useState<string>(
  editing?.max_query_counts != null ? String(editing.max_query_counts) : ""
);
// ...max bytes value + unit, same pattern
```

A `useState` initializer runs **only on the component's first mount**. After
that, changing the `editing` prop does not re-seed the state. Because a single
`DefinitionDialog` instance is shared between the "New Definition" trigger and
the per-row edit buttons — mounted once inside the `<Dialog>` in the
`PageHeader` actions ([lines 206–226](front/src/routes/_authenticated/grant-definitions/index.tsx:206))
— the form keeps whatever values it had on first render (the empty "create"
state) and never picks up the `editing` definition that was selected afterwards.

The edit button itself wires up correctly — it does
`setEditing(d); setDialogOpen(true)` ([lines 178–181](front/src/routes/_authenticated/grant-definitions/index.tsx:178)) —
so `editing` *is* set; the dialog just never re-reads it. Submit then also runs
through the same stale state, so an "edit" can silently overwrite the definition
with the blank/default form values (name reset, controls cleared, quotas
nulled).

## Proposal

Force `DefinitionDialog` to remount whenever the edit target changes, so its
`useState` initializers re-run against the correct `editing` object. The
idiomatic React fix is a `key`:

```tsx
<DefinitionDialog
  key={editing?.uid ?? "new"}
  editing={editing}
  onClose={() => { setDialogOpen(false); setEditing(null); }}
/>
```

Keying by `editing?.uid ?? "new"` guarantees a fresh mount (and therefore fresh
initial state) when switching between create and edit, and between different
rows.

Alternatively (or additionally), reset the fields with a `useEffect` keyed on
`editing` — but the `key` approach is simpler and avoids duplicating the seeding
logic.

### Acceptance / verification

- Open the edit dialog on an existing definition → every field shows that
  definition's current values (name, description, duration value + unit,
  checked controls, max queries, max data + unit).
- Editing a field and saving updates only the intended fields; unchanged fields
  are preserved.
- The "New Definition" flow still opens with empty defaults.
- Opening edit on row A, closing, then opening edit on row B shows B's values
  (no bleed-through from A).
- Add/adjust an E2E case in `front/e2e/` (a `grants`/`grant-definitions` spec)
  asserting the edit dialog is pre-populated, using the existing
  `data-testid="edit-grant-definition-<uid>"` and `grant-definition-name`
  hooks.

## Implementation Plan

1. Add `key={editing?.uid ?? "new"}` to the single `<DefinitionDialog>` render
   inside the `<Dialog>` in the `PageHeader` actions
   (`front/src/routes/_authenticated/grant-definitions/index.tsx`, ~line 219).
   This forces React to remount the dialog whenever the edit target changes, so
   the `useState` initializers re-run against the correct `editing` object.
   Confirmed there is only one render site of `DefinitionDialog` (grep) so no
   second key is needed.
2. Run `bun run lint` from `front/` — no new errors in the touched file.
3. Run `bun run build` (tsc + Vite) from `front/` — types + build green.
4. Extend `front/e2e/grants.spec.ts` (or the grant-definitions spec) with a case
   that opens the edit dialog on a seeded definition via
   `data-testid="edit-grant-definition-<uid>"` and asserts
   `data-testid="grant-definition-name"` is pre-filled with the row's name (not
   empty), guarding against the create/edit state bleed-through.
