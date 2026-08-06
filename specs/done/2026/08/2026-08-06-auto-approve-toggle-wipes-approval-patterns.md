---
model: sonnet
effort: low
---

# The auto-approve toggle silently wipes a definition's approval patterns

## Goal

Flipping the Auto-approve switch on a grant definition must not destroy the
definition's `approval_patterns` and `approver_group_uids`.

## Why

`toggleAutoApprove` in
`front/src/routes/_authenticated/grant-definitions/index.tsx` builds a *full*
`CreateGrantDefinitionRequest` body for the PATCH, but only copies a subset of
the definition's fields — `approval_patterns` and `approver_group_uids` are
missing. `handleUpdateGrantDefinition`
(`internal/api/grant_definitions.go`) is a full replace, not a partial update:

```go
def.ApprovalPatterns = normalizeStrings(req.ApprovalPatterns)
def.ApproverGroupUIDs = normalizeUUIDs(req.ApproverGroupUIDs)
```

An absent field decodes to nil and normalizes to an empty array, so a single
click on the list-view switch quietly removes every four-eyes approval pattern
from the definition. Nothing warns the operator, and the audit event for
`grant_definition.updated` does not record patterns either, so the loss is
invisible after the fact.

The comment right there — "Preserve scope: this is a targeted toggle, not a
full edit" — shows the intent was already to preserve everything; two fields
were simply never added when approval gating shipped.

Found while adding the `priority` field (which was added to this body, so it is
not affected).

## Implementation

Either of:

1. **Frontend (smallest):** add `approval_patterns: d.approval_patterns` and
   `approver_group_uids: d.approver_group_uids` to the body in
   `toggleAutoApprove`. Cheap, but leaves the same trap for the next field
   someone adds.
2. **Backend (durable):** make `PATCH /grant-definitions/{uid}` a genuine
   partial update — pointer/optional fields on
   `UpdateGrantDefinitionRequest`, only assigning the ones actually present in
   the body. This removes the whole class of "forgot a field in a targeted
   toggle" bug. Note `UpdateGrantDefinitionRequest` is currently a type alias
   of `CreateGrantDefinitionRequest`, so it needs its own type.

Option 2 is the right fix; option 1 is a safe stopgap if the partial-update
surface feels too large for one change.

Add a regression test either way: toggle auto-approve on a definition that has
approval patterns, then re-read it and assert the patterns survived
(`internal/api/grant_definitions_test.go` has the router + helpers), plus a
`front/e2e/grant-definitions.spec.ts` case if the fix stays frontend-side.

No GitHub issue filed yet — one should be.
