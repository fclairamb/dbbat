# Grant request cancel: authz test, e2e, confirm step

## Goal

Lock down the existing "requester cancels their own pending grant request"
flow with tests, and make the UI action harder to trigger by accident.

## Why

The feature shipped with the original grant request workflow (743fe20):
`POST /api/v1/grant-requests/{uid}/cancel` → `handleCancelGrantRequest`
(`internal/api/grant_requests.go`), `Store.CancelGrantRequest`, audit event
`grant_request.cancelled`, Slack message updated through
`notify.GrantActionCancelled`, and a Cancel button on the grant requests page.

What is missing:
- Only one store test (`TestCancelGrantRequest`) covers the happy path. No API
  test checks the authorization rules: another non-admin user gets 403, an admin
  can cancel, a request that is not pending gets 409.
- No Playwright test drives the Cancel button.
- The button is an icon (`Ban`, tooltip "Cancel") and cancels on a single
  click, with no confirmation. Approve and deny sit right next to it for anyone
  who is both requester and approver.

## Implementation

- API test in `internal/api/` covering: requester → 200, other connector → 403,
  admin (not the requester) → 403,
  request already cancelled or approved → 409, unknown uid → 404.
- E2E in `front/e2e/`: a connector files a request, cancels it, and the row
  shows `cancelled`.
- `front/src/routes/_authenticated/grant-requests/index.tsx`: put
  `cancel.mutate` behind a small confirm dialog (reuse the existing
  `AlertDialog` pattern) and add a visible label or clearer tooltip such as
  "Withdraw request".
