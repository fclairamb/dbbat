---
model: sonnet
effort: medium
---

# Auto-approve is hard to enable from the grants management UI

## Problem

The request: "update the grants management interface to allow to enable
auto-approve".

State of the branch (`feat/auto-approve-grants-ssh-tunnels`, commit `78faffa`):
auto-approve for grant definitions is already implemented end-to-end per the
archived spec `specs/done/2026/07/2026-07-16-01-auto-approved-grant-definitions.md`:

- `DefinitionDialog` on `/grant-definitions` has an "Auto-approve requests"
  checkbox for both create and edit
  (`front/src/routes/_authenticated/grant-definitions/index.tsx:458-475`),
  and the table shows an Auto-approve column (`:154-157`).
- The request flow requires a justification when the selected definition is
  auto-approved (`front/src/routes/_authenticated/grant-requests/index.tsx:257`).

So the literal capability exists — but only buried inside the definition
create/edit dialog on `/grant-definitions`. Nothing on the pages where grants
are actually *managed* (`/grants`, `/grant-requests`) lets an admin see or
enable auto-approve, and the definitions table column is display-only.

## Proposal

Make auto-approve enableable from the grants management surfaces, not just the
definition dialog:

1. **Inline toggle on `/grant-definitions`**: turn the read-only "Auto-approve"
   table column into a switch (admin-only) that PATCHes the definition via the
   existing update mutation — no need to open the dialog. Keep a confirmation
   or explanatory tooltip since this removes human review.
2. **Surface on `/grant-requests` (admin view)**: for a pending request, show
   whether its definition is auto-approved; optionally offer an
   "Approve + enable auto-approve for this definition" action so admins can
   promote a recurring request pattern in one step.
3. **Tests**: E2E covering toggling auto-approve from the table and observing
   that a subsequent request is approved instantly (test ids exist:
   `grant-definition-auto-approve`).

## Open questions

- The core toggle already ships on this branch — if the ask came from testing
  an instance running `main`, the real action item may just be "merge/deploy
  the branch". Confirm which surface the requester was looking at.
- Is item 2 (approve-and-enable from a pending request) wanted, or is the
  inline table toggle (item 1) enough?

## Implementation Plan

1. **Inline toggle on `/grant-definitions`** (`front/src/routes/_authenticated/grant-definitions/index.tsx`):
   - Replace the read-only "Auto-approve" badge cell with a `Switch` (admin +
     active-definition only; non-admin/inactive rows keep the static badge).
   - Wrap it in a `Tooltip` explaining that requests skip admin review when on.
   - Turning it ON routes through a confirmation `AlertDialog` (mirrors the
     existing "Deactivate definition?" dialog); turning it OFF applies
     immediately (no confirmation needed since it re-adds review, not removes
     it).
   - The toggle handler reconstructs the full `CreateGrantDefinitionRequest`
     body from the row's current fields (name, description, duration_seconds,
     controls, max_query_counts, max_bytes_transferred) with `auto_approve`
     flipped, and calls the existing `useUpdateGrantDefinition` mutation
     (same one `DefinitionDialog` already uses) — no backend change needed.
   - Test id: extend the existing `grant-definition-auto-approve` convention
     to `grant-definition-auto-approve-${uid}` for the per-row switch.

2. **Surface on `/grant-requests`** (`front/src/routes/_authenticated/grant-requests/index.tsx`):
   - Definition column: show a small "auto-approve" badge next to the
     definition name when `defMap[r.grant_definition_id]?.auto_approve` is
     true — this matters for requests that were submitted (and are still
     pending) *before* an admin flipped the definition to auto-approve.
   - Admin actions for a pending request: add a second action button
     ("Approve and enable auto-approve for this definition", `ShieldCheck`
     icon) next to the existing Approve/Deny buttons, shown only when the
     request's definition is not already auto-approved. It chains the
     existing `useUpdateGrantDefinition` mutation (flip `auto_approve` to
     true) followed by the existing `useApproveGrantRequest` mutation on
     success — both mutations already exist, so this is cheap to add per the
     spec's optional-item guidance.

3. **Tests**: add Playwright coverage in
   `front/e2e/grant-definitions.spec.ts` (or a new file) for toggling
   auto-approve from the table and observing a subsequent request auto-approve
   instantly, using the `grant-definition-auto-approve-*` test id.

4. **QA**: `bun run build` (typecheck) and `bun run lint` in `front/`; no Go
   changes expected since the update-definition endpoint already exists.
