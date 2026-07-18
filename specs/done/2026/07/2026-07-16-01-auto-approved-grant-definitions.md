---
model: sonnet
effort: medium
---

# Grant definitions can be flagged as auto-approved

## Problem

Every grant request today sits in `pending` until an admin approves it, even for
low-risk, routine access patterns (e.g. short read-only windows). For those, the
approval step is pure friction: the requester waits, an admin gets pinged on
Slack, and the outcome is always "approve".

We want a per-definition flag so that some grant definitions are **automatically
approved** at request time. The requester still provides a justification (the
"why"), and the request is still announced on Slack for visibility — but it is
instantly approved instead of waiting for a human decision.

## Proposal

1. **Schema / model** — add an `auto_approve boolean not null default false`
   column to `grant_definitions` (new migration in `internal/migrations/sql/`),
   and the matching field on `store.GrantDefinition`
   ([models.go:388](internal/store/models.go:388)). Expose it in the grant
   definition create/update API handlers
   ([grant_definitions.go](internal/api/grant_definitions.go)) and
   `internal/api/openapi.yml`.

2. **Request flow** — in `handleCreateGrantRequest`
   ([grant_requests.go:117](internal/api/grant_requests.go:117)): after creating
   the request, if `def.AutoApprove`, immediately approve it through the same
   path admins use (`store.ApproveGrantRequest`,
   [grant_requests.go:119](internal/store/grant_requests.go:119)) so the grant
   is created atomically with the status flip. Open question: who is recorded
   as the decider — likely a nil/`system` decider rather than the requester;
   the store/API layer may need to accept that.

3. **Slack notification** — still notify, but as a single "requested and
   auto-approved" message (or the created event immediately followed by the
   approved event) so the channel keeps its audit trail. The message must not
   render Approve/Deny buttons since there is nothing to decide
   (`internal/notify/`).

4. **Audit** — log both `grant_request.created` and an approval audit event,
   marking the approval as automatic.

5. **UI** — grant definition form gets an "Auto-approve requests" toggle
   (admin-only); the request flow should tell the requester their access is
   active immediately. Consider requiring a non-empty justification when
   `auto_approve` is set, since the justification is the only human check left
   — open question.

6. **Tests** — store + API tests covering: auto-approved request yields an
   active grant instantly, notification carries no action buttons, audit trail
   records the automatic decision.

No GitHub issue filed yet — one should be created.

## Implementation Plan

1. **Migration**: `20260716000000_grant_definitions_auto_approve.{up,down}.sql` — add
   `auto_approve boolean not null default false` to `grant_definitions`.
2. **Model**: `store.GrantDefinition.AutoApprove bool`.
3. **API DTOs**: add `auto_approve` to `CreateGrantDefinitionRequest` (shared with update);
   thread through create/update handlers and their audit `details`.
4. **OpenAPI**: add `auto_approve` (boolean, default false) to `GrantDefinition` and
   `CreateGrantDefinitionRequest` schemas; regenerate `front/src/api/schema.ts`.
5. **Store — decider handling (resolves the spec's open question)**: refactor
   `ApproveGrantRequest` into a shared `approveGrantRequestTx(ctx, uid, decidedBy *uuid.UUID,
   grantedBy uuid.UUID)`. Add `AutoApproveGrantRequest(ctx, uid, requesterID)` which calls it
   with `decidedBy = nil` (no human decided) and `grantedBy = requesterID` (the
   `access_grants.granted_by` column is NOT NULL, so the self-service grant is attributed to
   the requester). The audit trail's `via: auto_approve` detail is what distinguishes this from
   an admin-approved grant, not `decided_by`/`granted_by`.
6. **Request flow**: in `handleCreateGrantRequest`, after creating the pending request:
   - If `def.AutoApprove` and `req.Justification == ""` → 400 validation error. Decision:
     require justification for auto-approved definitions since it's the only human check left
     once the approval step is skipped.
   - If `def.AutoApprove` is true, call a new `s.autoApproveGrantRequest` (mirrors
     `approveGrantRequest`/`denyGrantRequest`) which calls `store.AutoApproveGrantRequest`, logs
     a `grant_request.approved` audit event with `details.via = "auto_approve"` and
     `PerformedBy = nil` (no human decider), fires the notify event with
     `Action = GrantActionApproved` and `Decider = nil`, and returns the approved request.
   - On auto-approve store failure (rare race), fall back to the normal pending path (log the
     error, send the `Created` notify event, return the still-pending request) rather than
     failing the whole HTTP request — the request row already exists.
   - Skip firing the `GrantActionCreated` notify event when auto-approving — only the
     `Approved` event goes out, so there's a single Slack message and no dead Approve/Deny
     buttons to race against.
7. **Slack notification**: `buildBlocks` already only renders Approve/Deny buttons for
   `Action == GrantActionCreated`, and `mainSectionText` already tolerates a nil `Decider` — no
   button-suppression change needed. Add one cosmetic change: `statusLabel`/`statusEmoji` render
   `"auto-approved"` (📥) instead of `"approved"` when `Action == GrantActionApproved` and
   `Decider == nil`, so the message reads as "requested and auto-approved" rather than looking
   like an admin decided.
8. **Audit**: `grant_request.created` (existing) + `grant_request.approved` with
   `details.via = "auto_approve"` and no `performed_by` (nil — nobody made a human decision).
9. **UI**:
   - `grant-definitions/index.tsx`: add an "Auto-approve requests" `Checkbox` to
     `DefinitionDialog`, thread into the create/update body, add an "Auto-approve" table column.
   - `grant-requests/index.tsx`: when the selected definition has `auto_approve`, make
     justification required (client-side) and show a note that access will be active
     immediately; the existing `approved` status badge already covers the resulting state, no
     new badge needed. Success toast varies based on the returned request's `status`.
10. **Tests**:
    - Store: `TestAutoApproveGrantRequest_CreatesGrant` (mirrors
      `TestApproveGrantRequest_CreatesGrant` but via the new method, asserting `decided_by` is
      nil and the grant's `granted_by` is the requester).
    - API: extend `handleCreateGrantRequest` coverage (new
      `internal/api/grant_requests_test.go`) — auto-approved request yields an active grant
      instantly (status approved, resulting_grant_id set), and a non-auto-approved request is
      unaffected; missing justification on an auto-approve definition is rejected.
    - Notify: `slack_test.go` — auto-approved event (`Action=Approved`, `Decider=nil`) renders no
      action buttons and uses the "auto-approved" status label.
