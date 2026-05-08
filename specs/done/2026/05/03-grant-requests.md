# Grant requests (in-app workflow)

> Depends on `02-grant-definitions.md`. Implement after Spec 02 ships.

## Goal

Let any authenticated user request access to a database by picking a grant
definition. An admin then approves or denies the request. On approval, dbbat
creates a real `access_grants` row from the definition + the request's
user/database, with `starts_at = now()` and
`expires_at = now() + definition.duration_seconds`.

This closes the self-service loop: definitions (Spec 02) define what's
askable, requests (this spec) let users ask, admins decide.

## Schema

New migration:
`internal/migrations/sql/20260510000000_grant_requests.up.sql`

```sql
CREATE TABLE grant_requests (
    uid                  UUID PRIMARY KEY,
    user_id              UUID NOT NULL REFERENCES users(uid),
    grant_definition_id  UUID NOT NULL REFERENCES grant_definitions(uid),
    database_id          UUID NOT NULL REFERENCES databases(uid),
    justification        TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL CHECK (status IN
                          ('pending','approved','denied','cancelled','expired')),
    requested_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at           TIMESTAMPTZ,
    decided_by           UUID REFERENCES users(uid),
    decision_reason      TEXT,
    resulting_grant_id   UUID REFERENCES access_grants(uid),

    -- Slack notification bookkeeping (populated by Spec 04, NULL until then)
    slack_channel        TEXT,
    slack_message_ts     TEXT
);

CREATE INDEX grant_requests_user_status_idx
    ON grant_requests(user_id, status);
CREATE INDEX grant_requests_status_requested_idx
    ON grant_requests(status, requested_at);
```

Down migration drops the table.

The two `slack_*` columns are added here (rather than in Spec 04) so Spec 04
can ship without another migration. They stay NULL until Spec 04 populates
them.

## Files added / modified

**Add**
- `internal/migrations/sql/20260510000000_grant_requests.up.sql`
- `internal/migrations/sql/20260510000000_grant_requests.down.sql`
- `internal/store/grant_requests.go`
- `internal/store/grant_requests_test.go`
- `internal/api/grant_requests.go`
- `internal/api/grant_requests_test.go`
- `front/src/routes/_authenticated/grant-requests/index.tsx`

**Modify**
- `internal/store/models.go` — append `GrantRequest` struct.
- `internal/store/grants.go` — extract a helper
  `BuildGrantFromDefinition(def, userID, dbID, grantedBy) *Grant` so the
  approval path and any future direct-create paths share construction logic.
- `internal/api/server.go` — register the new routes.
- `internal/api/openapi.yml` — add `GrantRequest` schema and paths.
- `front/src/lib/permissions.ts` — add `canRequestGrant()`,
  `canApproveGrantRequest()`.
- `front/src/routes/_authenticated/databases/index.tsx` (or the database
  detail page) — add a "Request access" button that opens the request modal.
- Sidebar nav — add "Grant Requests" entry visible to all authenticated
  users (admins see all, others see their own).

## Backend

### Model

```go
// internal/store/models.go (append)
type GrantRequestStatus string

const (
    GrantRequestPending   GrantRequestStatus = "pending"
    GrantRequestApproved  GrantRequestStatus = "approved"
    GrantRequestDenied    GrantRequestStatus = "denied"
    GrantRequestCancelled GrantRequestStatus = "cancelled"
    GrantRequestExpired   GrantRequestStatus = "expired"
)

type GrantRequest struct {
    bun.BaseModel `bun:"table:grant_requests,alias:gr"`

    UID                uuid.UUID          `bun:"uid,pk,type:uuid"             json:"uid"`
    UserID             uuid.UUID          `bun:"user_id,notnull,type:uuid"    json:"user_id"`
    GrantDefinitionID  uuid.UUID          `bun:"grant_definition_id,notnull,type:uuid" json:"grant_definition_id"`
    DatabaseID         uuid.UUID          `bun:"database_id,notnull,type:uuid" json:"database_id"`
    Justification      string             `bun:"justification,notnull,default:''" json:"justification"`
    Status             GrantRequestStatus `bun:"status,notnull"               json:"status"`
    RequestedAt        time.Time          `bun:"requested_at,notnull,default:current_timestamp" json:"requested_at"`
    DecidedAt          *time.Time         `bun:"decided_at"                   json:"decided_at,omitempty"`
    DecidedBy          *uuid.UUID         `bun:"decided_by,type:uuid"         json:"decided_by,omitempty"`
    DecisionReason     *string            `bun:"decision_reason"              json:"decision_reason,omitempty"`
    ResultingGrantID   *uuid.UUID         `bun:"resulting_grant_id,type:uuid" json:"resulting_grant_id,omitempty"`

    SlackChannel   *string `bun:"slack_channel"    json:"-"`
    SlackMessageTS *string `bun:"slack_message_ts" json:"-"`
}
```

`slack_*` are JSON-omitted; only the notifier reads them.

### Store

`internal/store/grant_requests.go`:

```go
func (s *Store) CreateGrantRequest(ctx context.Context, req *GrantRequest) error
func (s *Store) GetGrantRequest(ctx context.Context, uid uuid.UUID) (*GrantRequest, error)
func (s *Store) ListGrantRequests(ctx context.Context, f GrantRequestFilter) ([]*GrantRequest, error)

// Atomic transitions. Each returns ErrInvalidTransition if status != pending.
func (s *Store) ApproveGrantRequest(ctx context.Context, uid, decidedBy uuid.UUID) (*Grant, *GrantRequest, error)
func (s *Store) DenyGrantRequest(ctx context.Context, uid, decidedBy uuid.UUID, reason string) (*GrantRequest, error)
func (s *Store) CancelGrantRequest(ctx context.Context, uid, byUser uuid.UUID) (*GrantRequest, error)

// Slack bookkeeping (used by Spec 04)
func (s *Store) SetGrantRequestSlackMessage(ctx context.Context, uid uuid.UUID, channel, ts string) error
```

`ApproveGrantRequest` runs in a single transaction:
1. SELECT FOR UPDATE the request row, fail if not pending
2. SELECT FOR UPDATE the grant_definition (must be `is_active`)
3. Build a `Grant` via `BuildGrantFromDefinition(def, req.UserID, req.DatabaseID, decidedBy)`
4. Insert it
5. UPDATE the request: status, decided_at, decided_by, resulting_grant_id

`GrantRequestFilter` mirrors `GrantFilter` (user_id, status, database_id,
limit, offset).

### Helper extraction

In `internal/store/grants.go`, extract:

```go
func BuildGrantFromDefinition(def *GrantDefinition, userID, databaseID, grantedBy uuid.UUID, now time.Time) *Grant {
    return &Grant{
        UID:                 uuid.New(),
        UserID:              userID,
        DatabaseID:          databaseID,
        Controls:            append([]string{}, def.Controls...),
        GrantedBy:           grantedBy,
        StartsAt:            now,
        ExpiresAt:           now.Add(time.Duration(def.DurationSeconds) * time.Second),
        MaxQueryCounts:      def.MaxQueryCounts,
        MaxBytesTransferred: def.MaxBytesTransferred,
    }
}
```

### API

| Method | Path                                          | Auth                |
|--------|-----------------------------------------------|---------------------|
| POST   | `/api/v1/grant-requests`                      | any user            |
| GET    | `/api/v1/grant-requests`                      | role-aware          |
| GET    | `/api/v1/grant-requests/:uid`                 | role-aware          |
| POST   | `/api/v1/grant-requests/:uid/approve`         | admin               |
| POST   | `/api/v1/grant-requests/:uid/deny`            | admin               |
| POST   | `/api/v1/grant-requests/:uid/cancel`          | requester or admin  |

Role-aware listing/get: admins see all; non-admins see only requests where
`user_id == current user`. Mirror `handleListGrants` in
`internal/api/grants.go`.

State guards: every transition endpoint returns **409 Conflict** with a
typed error if the request is not in `pending`. The exact text:

```
"grant request <uid> is already <status>"
```

Audit (use `LogAuditEvent` in `internal/store/audit.go`):
- `grant_request.created`
- `grant_request.approved` (include `resulting_grant_id`)
- `grant_request.denied` (include `decision_reason`)
- `grant_request.cancelled`

### Validation in `POST /api/v1/grant-requests`

- `grant_definition_id` exists and is active
- `database_id` exists and is visible to the requester (current rules — see
  `handleListDatabases` in `internal/api/databases.go`)
- `justification` ≤ 1000 chars
- Reject if the requester already has an `active_grant` on the same database
  for that definition? **No** — let users top up; admins can deny if they
  disagree. Document this in the API description.
- Reject if the requester already has a `pending` request for the same
  database **and** definition: 409 with a link to the existing request.

## Frontend

`front/src/routes/_authenticated/grant-requests/index.tsx`:

- For non-admins: a list of "My grant requests" with status badges and
  cancel action on pending rows.
- For admins: tabs "Pending" (default) / "All". Pending rows show
  Approve / Deny actions. Deny opens a small dialog asking for an optional
  reason.
- Empty state for users: "You have no grant requests. Visit a database to
  request access."

"Request access" button on the databases index/detail page → modal:
- Definition picker: dropdown listing active definitions with a one-line
  summary (`Read-only 1h — 1000 queries, 100 MB`).
- Database is implied (or picked from a dropdown if launched from the index
  page).
- Justification textarea (optional, ≤ 1000 chars).
- Submit → POST → toast and redirect to the requests page.

`front/src/lib/permissions.ts`:

```ts
export const canRequestGrant = (user: User | null): boolean =>
  !!user; // any authenticated user
export const canApproveGrantRequest = (user: User | null): boolean =>
  user?.roles?.includes("admin") ?? false;
```

## Tests

### Backend
- Store integration: create → approve produces a `Grant` with the right
  controls/quotas/duration; transitions guarded; double-approve returns
  ErrInvalidTransition.
- API: connector creates a request; cannot approve own; admin approves;
  deny with reason persists the reason; cancel by requester works; cancel by
  admin works; cancel by other user → 403; double-cancel → 409.
- Definition deactivation between create and approve → approve fails with a
  clear 409 ("definition no longer active").

### E2E
- Connector creates a request via the UI for a "Read-only 1h" definition on
  test database.
- Admin approves.
- Connector connects via `psql` through the PG proxy and `SELECT 1` works.
- After 1h (or by manipulating clock in test), the grant expires and
  subsequent queries are denied (existing grant-expiry behavior).

## Verification checklist

- [ ] `make lint` clean, `make test` green
- [ ] Migration up/down works
- [ ] Swagger UI lists the new paths
- [ ] Connector → request → admin approves → real grant exists
- [ ] Double-approve returns 409 with a clear message
- [ ] Cancel by requester works; cancel by stranger forbidden
- [ ] Audit log captures all four lifecycle events

## Out of scope

- Slack notifications for these events (Spec 04 covers that — it just reads
  the rows this spec produces).
- Auto-expire of pending requests after N hours (the `expired` status is
  reserved for a future cron; for now requests stay pending until decided
  or cancelled).
- Bulk approve/deny.
