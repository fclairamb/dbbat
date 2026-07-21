---
sidebar_position: 2
---

# Grant Requests & Auto-Approval

[Grants](/docs/features/access-control) can be created directly by an admin, but that puts an admin in the loop for every access. The request workflow lets users ask for access themselves, against templates an admin has pre-approved — and, when the shape of access is routine enough, skip the approval step entirely.

```
grant definition (admin-authored template)
        │
        ├─► grant request (user picks definition + server)
        │        │
        │        ├─► pending ──► admin approves ──► grant
        │        └─► auto-approved ────────────────► grant
```

## Grant Definitions

A definition describes a *shape* of grant — controls, quotas, duration — without naming a user or a server. Users can only request access by picking an active definition, so the set of definitions is exactly the set of access shapes your organisation permits.

```bash
curl -X POST http://localhost:4200/api/v1/grant-definitions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "read-only-1h",
    "description": "One hour of read-only access",
    "controls": ["read_only"],
    "max_query_counts": 1000,
    "max_bytes_transferred": 10485760,
    "auto_approve": false
  }'
```

Requires the admin role. A duplicate name returns `409 DUPLICATE_NAME`.

Definitions are **soft-deleted**: deactivating one sets `is_active: false` rather than removing the row, so historical requests keep pointing at the definition they were granted under. Non-admins only ever see active definitions.

Direct admin grant creation via `POST /api/v1/grants` bypasses definitions entirely.

## Requesting Access

Any authenticated user can submit a request:

```bash
curl -X POST http://localhost:4200/api/v1/grant-requests \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "grant_definition_id": "550e8400-e29b-41d4-a716-446655440000",
    "database_id": "660e8400-e29b-41d4-a716-446655440000",
    "justification": "Investigating ticket SUP-4821"
  }'
```

The target must be a database server — requesting access to an [SSH bastion](/docs/features/ssh-tunnels) is rejected, since there is nothing to proxy. Only servers marked `listable` appear in the request dropdown for non-admin users.

A second pending request for the same user, server and definition returns `409`.

### Decisions

```
POST /api/v1/grant-requests/:uid/approve
POST /api/v1/grant-requests/:uid/deny
POST /api/v1/grant-requests/:uid/cancel
```

Approval atomically transitions `pending → approved` and builds a real grant from the definition plus the request's user and server, linked back as `resulting_grant_id`. It returns `409` if the request is no longer pending, or if its definition has since been deactivated.

Requests carry one of: `pending`, `approved`, `denied`, `cancelled`, `expired`.

## Auto-Approval

Some access is routine enough that admin review is theatre — read-only access to a staging database, say. Flagging a definition `auto_approve` makes requests against it resolve instantly:

```bash
curl -X PUT http://localhost:4200/api/v1/grant-definitions/$DEF_UID \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"auto_approve": true}'
```

A request against an auto-approving definition is created and approved in the same call: the response comes back `approved` with `resulting_grant_id` already populated, and the user can connect immediately.

What stays the same, and what changes:

| | Normal request | Auto-approved request |
|---|---|---|
| Justification | Optional | **Required** — the request is rejected without one |
| Admin decision | Required | None |
| `decided_by` | The approving admin | `null` — no human decided |
| Slack notification | With ✅ Approve / ❌ Deny buttons | Sent **without** buttons |
| Audit trail | `grant_request.created` + the decision event | `grant_request.created` + a decision tagged `auto_approve` |

:::note
Auto-approval removes the *gate*, not the *record*. Every auto-approved request still requires a written justification, still produces its own audit events, still notifies Slack, and still produces a time-windowed grant with the definition's controls and quotas. It is faster access, not unaudited access.
:::

Since `decided_by` is left null, an auto-approved grant is always distinguishable from one an admin waved through — in the audit log, in the API, and in Slack (where it renders as ⚡ *auto-approved* rather than ✅ *approved*).

If auto-approval fails for an unexpected reason — for instance the definition is deactivated in the moment between validation and approval — the request degrades gracefully to the normal pending flow rather than erroring out.

### From the UI

The web UI exposes this in two places:

- An inline **auto-approve toggle** on each row of the grant-definitions table.
- An **"approve & enable auto-approve"** action on a pending request, which approves that request *and* flips its definition to auto-approve, so requests of the same shape are instant from then on.

## Choosing What to Auto-Approve

Auto-approval is appropriate when the definition itself is the control — when you would approve every request against it without thinking. That usually means:

- `read_only` controls, so the blast radius is bounded
- A short window, so access expires on its own
- Query and byte quotas set, so a runaway export is cut off
- Non-production or low-sensitivity targets

Keep human review for write access, for production databases holding personal data, and for anything where *who* is asking changes the answer.

## See Also

- [Access Control](/docs/features/access-control) — controls, quotas, time windows, revocation
- [Configuration](/docs/configuration) — Slack notification and approval-button setup
- [API Reference](/docs/api) — grant definition and grant request endpoints
