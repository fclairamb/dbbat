# Role-Based Access Control (RBAC) Redesign

## Status: Approved

## Summary

Replace the current binary `is_admin` model with a three-role system that enforces separation of concerns and principle of least privilege.

## Current State

The existing model uses a single `is_admin` boolean on users:
- Admin users: Full access to everything (create users, databases, grants, view all data, connect)
- Non-admin users: Can only connect through the proxy (with valid grants)

## Proposed Model

### Three Independent Rights

| Right | Capabilities |
|-------|-------------|
| **admin** | Create/update/delete users, databases, and grants |
| **viewer** | View all connections, queries (including result rows), and audit logs |
| **connector** | Connect through the PostgreSQL proxy (requires valid grant) |

### Key Principles

1. **Rights are independent and combinable** - A user can have any combination of the three rights
2. **No implicit inheritance** - Admin does not imply viewer or connector rights
3. **Self-grant requirement** - Even admins must explicitly grant themselves database access to connect
4. **Minimal information disclosure** - Sensitive connection details restricted to admins
5. **Universal password change** - All authenticated users can change their own password

### Information Visibility Matrix

| Field | admin | viewer | connector (with grant) |
|-------|-------|--------|------------------------|
| Database proxy name | Yes | Yes | Own grants only |
| Database description | Yes | Yes | Own grants only |
| Target hostname | Yes | No | No |
| Target port | Yes | No | No |
| Target database name | Yes | No | No |
| Target username | Yes | No | No |
| Target password | **Never** | **Never** | **Never** |
| SSL mode | Yes | No | No |

### API Authorization Matrix

| Endpoint | admin | viewer | connector |
|----------|-------|--------|-----------|
| `POST /api/users` | Yes | No | No |
| `GET /api/users` | Yes | No | No |
| `PATCH /api/users/:id` | Yes | Own password | Own password |
| `DELETE /api/users/:id` | Yes | No | No |
| `POST /api/databases` | Yes | No | No |
| `GET /api/databases` | Full details | Name + description | Own grants only |
| `PUT /api/databases/:id` | Yes | No | No |
| `DELETE /api/databases/:id` | Yes | No | No |
| `POST /api/grants` | Yes | No | No |
| `GET /api/grants` | Yes | Yes | Own grants only |
| `DELETE /api/grants/:id` | Yes | No | No |
| `GET /api/connections` | Yes | Yes | Own connections |
| `GET /api/queries` | Yes | Yes | No |
| `GET /api/audit` | Yes | Yes | No |

### Self-Service Capabilities

**All authenticated users (regardless of rights):**
- Change own password

**Connector right adds:**
- View own grants (to know which databases they can access)
- View own connections (to verify no unauthorized usage)

**Connectors cannot:**
- View their own queries (prevents reverse-engineering what the system logs)

---

## Design Decisions

### Strict Separation of Admin and Viewer

Admin does **not** imply viewer. Rationale:
- Maximum separation of concerns
- Admin actions are purely management operations
- Small teams can grant both rights to the same user
- Larger organizations benefit from the separation

### Viewer Full Access to Query Data

Viewers can see:
- All connections
- All queries including SQL text, timing, and result rows
- All audit events

This enables comprehensive security auditing and compliance monitoring.

### Audit Log Restricted to Viewers

Regular connectors cannot see audit events, even their own. Only users with viewer rights can access the audit log.

---

## Implementation

### Schema Change

Edit the existing initial migration directly (no new migration file). The project is pre-release, so we modify the schema in place rather than creating migration chains.

Use a PostgreSQL array of enums for flexibility:

```sql
CREATE TYPE user_role AS ENUM ('admin', 'viewer', 'connector');

CREATE TABLE users (
    -- ... other columns ...
    roles user_role[] NOT NULL DEFAULT ARRAY['connector']::user_role[],
    -- Remove: is_admin BOOLEAN NOT NULL DEFAULT FALSE
);
```

**Note**: Do not create a separate migration. Update the existing `20260107000000_initial_schema.up.sql` (or equivalent) to include the `roles` column instead of `is_admin`.

### API Response Filtering

Database list responses are filtered by role:

```go
func (h *Handler) ListDatabases(c *gin.Context) {
    user := getCurrentUser(c)
    databases, _ := h.store.ListDatabases()

    var response []DatabaseResponse
    for _, db := range databases {
        if user.HasRole(RoleAdmin) {
            // Full details (except password)
            response = append(response, DatabaseResponse{
                ID:          db.ID,
                Name:        db.Name,
                Description: db.Description,
                Host:        db.Host,
                Port:        db.Port,
                Database:    db.Database,
                Username:    db.Username,
                SSLMode:     db.SSLMode,
            })
        } else if user.HasRole(RoleViewer) {
            // Name and description only
            response = append(response, DatabaseResponse{
                ID:          db.ID,
                Name:        db.Name,
                Description: db.Description,
            })
        } else if user.HasRole(RoleConnector) {
            // Only databases with active grants
            if hasGrantForDatabase(user.ID, db.ID) {
                response = append(response, DatabaseResponse{
                    ID:          db.ID,
                    Name:        db.Name,
                    Description: db.Description,
                })
            }
        }
    }
}
```

---

## Bootstrap Flow

1. Default admin created on first startup with `roles = ['admin', 'connector']`
2. Admin creates database configuration via API
3. Admin grants themselves access to the database
4. Admin (now with grant) can test connection through proxy
5. All actions create audit trail from day one
