# Grant request cancel: e2e test, web confirm step

## Goal

Cover the web UI's "requester withdraws their own pending grant request" flow
end to end, and make its button harder to trigger by accident.

## Why

Cancel is requester-only on every path: `POST /api/v1/grant-requests/{uid}/cancel`
(`handleCancelGrantRequest`), the Slack 🚫 Withdraw button, and the Cancel
button on the grant requests page. All go through `mayCancelGrantRequest` and
`cancelGrantRequest` in `internal/api/grant_requests.go`. The REST rule is
pinned by `TestCancelGrantRequest_RequesterOnly`.

What is missing:
- No Playwright test drives the Cancel button.
- The button is an icon (`Ban`, tooltip "Cancel") and cancels on a single
  click, with no confirmation. Approve and deny sit right next to it in the
  same row. The Slack Withdraw button already has a confirm dialog.

## Implementation

- E2E in `front/e2e/`: a connector files a request, cancels it, and the row
  shows `cancelled`. An admin viewing the same pending request sees no Cancel
  button.
- `front/src/routes/_authenticated/grant-requests/index.tsx`: put
  `cancel.mutate` behind a small confirm dialog (`front/src/components/ui/alert-dialog.tsx`)
  and use a clearer label or tooltip such as "Withdraw request", matching the
  Slack button.
