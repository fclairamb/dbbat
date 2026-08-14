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
  -H "Authorization: Bearer $DBBAT_API_KEY" \
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

A definition may also carry an optional `priority`, stamped verbatim on every
grant built from it. Leave it out — the normal case — and each grant computes
its own from its controls; see [Overlapping
grants](./access-control.md#overlapping-grants).

Definitions are **immutably versioned**. Editing one archives the current row and inserts a successor carrying the change; grants keep pointing at the exact version they were issued from, so tightening — or loosening — a definition never retroactively changes access that is already live. It changes what gets issued from then on. A slug always resolves to the current version; older versions stay readable by uid so a grant issued from one can still show its shape.

**Deactivating** a definition (`DELETE /api/v1/grant-definitions/{uid}`) is different from that archival, and much stronger: it withdraws every version at once and **fails closed** — grants issued from any of them stop authorising new connections. The API reports how many grants that affected, and the UI shows the number before you confirm. Hard deletion (`?hard=true`) is refused with a `409` as soon as anything references the definition; deactivation is the way to retire one that has been used.

**Every grant is an instance of a definition.** An admin assigning access directly (`POST /api/v1/grants`) picks a definition too — there is no way to create a grant with an ad-hoc shape, which is what makes the set of definitions an exhaustive list of the access shapes your organisation permits.

## Requesting Access

Any authenticated user can submit a request:

```bash
curl -X POST http://localhost:4200/api/v1/grant-requests \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
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

### Who Decides

Approval is **not admin-only**. Every admin decides everything, and in addition each server names the user groups allowed to decide requests targeting *it*:

```
target server's access_approver_user_group_uids
        │  (empty?)
        ▼
union of the access_approver_user_group_uids of the
server groups that server currently belongs to
        │  (still empty?)
        ▼
admins only
```

Set neither and nothing changes: only admins decide, exactly as before. Set the list on one server and you have delegated that server's access requests — say, to the ops group that owns it — without handing anyone the admin role. Set it on a server group instead and it covers every member server that names no approvers of its own; a server naming its own overrides the group's rather than adding to it, and several groups holding the same server union their lists.

```bash
# The ops group decides requests for this server, alongside admins.
curl -X PUT http://localhost:4200/api/v1/servers/$DB_UID \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"access_approver_user_group_uids": ["'$OPS_GROUP_UID'"]}'
```

Three things are worth knowing before you rely on it:

- **Nobody approves their own request.** A named access approver filing their own request gets a `403` on it, whatever list they appear in. (Admins keep their long-standing ability to self-approve, since `POST /api/v1/grants` already lets them issue the same access directly — refusing them here would be a formality, not a control.)
- **Resolution is live.** The lists are read when a decision is made, not copied onto the request when it is filed, so editing one immediately changes who may decide requests already waiting. That is on purpose: a departing lead's replacement should not have to wait for anything.
- **This approver kind covers access only.** A *query* approver — the person who releases a pattern-triggered approval hold on a live statement — is configured separately in `query_approver_user_group_uids`, and neither role implies the other. See [Access Control](/docs/features/access-control#approvers-on-servers-and-server-groups).

Each row of `GET /api/v1/grant-requests` reports `approver_role` for the calling user — `admin`, `server_approver`, or empty — and the UI shows it as a "you decide as" badge, so a delegated approver can see why the buttons are there on one row and not the next.

### Who Gets Notified

When Slack notifications are configured, the pending message @-mentions every admin **and** every member of the access-approver groups resolved for the target server, de-duplicated. The Approve / Deny buttons authorize against exactly the rule above — clicking one as somebody who may not decide returns an ephemeral refusal — and both Slack transports (the inbound signing-secret endpoint and Socket Mode) go through the same check. The message's deep link lands on the grant-requests page, which now lists an approver's decidable requests alongside their own.

## Auto-Approval

Some access is routine enough that admin review is theatre — read-only access to a staging database, say. Flagging a definition `auto_approve` makes requests against it resolve instantly:

```bash
curl -X PUT http://localhost:4200/api/v1/grant-definitions/$DEF_UID \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
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
