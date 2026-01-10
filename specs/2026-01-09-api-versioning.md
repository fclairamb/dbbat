# API Versioning

## Overview

Rename all REST API endpoints from `/api/*` to `/api/v1/*` to introduce explicit API versioning. This prepares the API for future backwards-incompatible changes while maintaining a stable interface for existing clients.

## Motivation

- **Future-proofing**: Enable introducing breaking changes in future API versions (v2, v3, etc.) without disrupting existing clients
- **Clear contracts**: Make the API version explicit in the URL, improving clarity for API consumers
- **Migration path**: Provide a clear upgrade path when introducing incompatible changes
- **Industry standard**: Follow REST API best practices for versioning (e.g., Stripe, GitHub, AWS)

## Design

### URL Structure

All API endpoints will be prefixed with `/api/v1/` instead of `/api/`:

| Current Path | New Path |
|--------------|----------|
| `GET /api/health` | `GET /api/v1/health` |
| `POST /api/auth/login` | `POST /api/v1/auth/login` |
| `POST /api/auth/logout` | `POST /api/v1/auth/logout` |
| `GET /api/auth/me` | `GET /api/v1/auth/me` |
| `GET /api/users` | `GET /api/v1/users` |
| `POST /api/users` | `POST /api/v1/users` |
| `GET /api/users/:uid` | `GET /api/v1/users/:uid` |
| `PUT /api/users/:uid` | `PUT /api/v1/users/:uid` |
| `PUT /api/users/:uid/password` | `PUT /api/v1/users/:uid/password` |
| `DELETE /api/users/:uid` | `DELETE /api/v1/users/:uid` |
| `GET /api/databases` | `GET /api/v1/databases` |
| `POST /api/databases` | `POST /api/v1/databases` |
| `GET /api/databases/:uid` | `GET /api/v1/databases/:uid` |
| `PUT /api/databases/:uid` | `PUT /api/v1/databases/:uid` |
| `DELETE /api/databases/:uid` | `DELETE /api/v1/databases/:uid` |
| `GET /api/grants` | `GET /api/v1/grants` |
| `POST /api/grants` | `POST /api/v1/grants` |
| `GET /api/grants/:uid` | `GET /api/v1/grants/:uid` |
| `DELETE /api/grants/:uid` | `DELETE /api/v1/grants/:uid` |
| `GET /api/keys` | `GET /api/v1/keys` |
| `POST /api/keys` | `POST /api/v1/keys` |
| `GET /api/keys/:id` | `GET /api/v1/keys/:id` |
| `DELETE /api/keys/:id` | `DELETE /api/v1/keys/:id` |
| `GET /api/connections` | `GET /api/v1/connections` |
| `GET /api/queries` | `GET /api/v1/queries` |
| `GET /api/queries/:uid` | `GET /api/v1/queries/:uid` |
| `GET /api/audit` | `GET /api/v1/audit` |
| N/A | `GET /api/v1/version` |

### Exception: Non-Versioned Endpoints

The following endpoints remain outside of versioning as they provide documentation about the API:

| Endpoint | Purpose | Notes |
|----------|---------|-------|
| `GET /api/openapi.yml` | OpenAPI specification | Documentation endpoint |
| `GET /api/docs` | Swagger UI | Documentation viewer |

### Version Endpoint

Add a versioned endpoint that returns build and version information:

```
GET /api/v1/version

Response (200 OK):
{
    "api_version": "v1",
    "build_version": "1.2.3",
    "build_commit": "abc1234",
    "build_time": "2026-01-09T12:00:00Z"
}
```

This endpoint provides both API version and build information, which is useful for debugging and compatibility checks.

### Backwards Compatibility

**Option 1: Hard Cut-Over (Recommended)**
- Remove all `/api/*` routes (except metadata endpoints)
- Only serve `/api/v1/*`
- Update all documentation to use v1 paths
- Clients must update to use new paths

**Option 2: Temporary Redirect (Transition Period)**
- Keep `/api/*` routes temporarily
- Return `308 Permanent Redirect` to `/api/v1/*` equivalents
- Add deprecation headers to old endpoints
- Remove after transition period (e.g., 6 months)

```
HTTP/1.1 308 Permanent Redirect
Location: /api/v1/users
Deprecation: true
Sunset: Mon, 01 Jul 2026 00:00:00 GMT
Link: </api/v1/users>; rel="alternate"

{
    "error": "endpoint_moved",
    "message": "This endpoint has moved to /api/v1/users",
    "new_location": "/api/v1/users"
}
```

**Recommendation**: Use Option 1 (hard cut-over) since this is an early-stage project with no production users yet. This keeps the codebase simpler.

**Choice**: Option 1

## Implementation

### Backend Changes

#### 1. Update API Router (`internal/api/server.go`)

Change the route group from `/api` to `/api/v1`:

```go
// Current (line ~100):
api := router.Group("/api")
{
    // Health check
    api.GET("/health", s.handleHealth)
    // ... all routes
}

// New:
// Documentation endpoints (not versioned)
api := router.Group("/api")
{
    api.GET("/openapi.yml", s.handleOpenAPISpec)
    api.GET("/docs", s.handleSwaggerUI)
    api.GET("/docs/*any", s.handleSwaggerUI)
}

// Versioned API endpoints
v1 := router.Group("/api/v1")
{
    // Health check and version info (unauthenticated)
    v1.GET("/health", s.handleHealth)
    v1.GET("/version", s.handleVersion)

    // Auth endpoints (login is unauthenticated)
    auth := v1.Group("/auth")
    auth.POST("/login", s.handleLogin)

    // All other routes require authentication
    authenticated := v1.Group("")
    authenticated.Use(s.authMiddleware())
    // ... rest of authenticated routes
}
```

#### 2. Create Version Package (`internal/version/version.go`)

Create a new package to hold build information:

```go
// Package version contains build version information set via ldflags.
package version

// Build information variables (set via ldflags during build)
var (
    Version   = "dev"
    Commit    = "unknown"
    BuildTime = "unknown"
)
```

The build variables are set during compilation using ldflags:

```bash
go build -ldflags "-X 'github.com/fclairamb/dbbat/internal/version.Version=1.2.3' \
                   -X 'github.com/fclairamb/dbbat/internal/version.Commit=$(git rev-parse --short HEAD)' \
                   -X 'github.com/fclairamb/dbbat/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)'"
```

#### 3. Add Version Handler (`internal/api/server.go`)

Add a handler that uses the version package:

```go
import "github.com/fclairamb/dbbat/internal/version"

// handleVersion returns API and build version information.
func (s *Server) handleVersion(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{
        "api_version":   "v1",
        "build_version": version.Version,
        "build_commit":  version.Commit,
        "build_time":    version.BuildTime,
    })
}
```

#### 4. Update OpenAPI Specification (`internal/api/openapi.yml`)

Update the `servers` section to use the versioned base URL:

```yaml
# Current (line ~54-56):
servers:
  - url: /api
    description: API server

# New:
servers:
  - url: /api/v1
    description: API v1 server
```

**Note**: All path definitions in the OpenAPI spec are already relative (e.g., `/users`, `/databases`), so they don't need to change. The `servers.url` change handles the prefix.

Also add version info endpoint definition outside the versioned paths (or document it separately):

```yaml
# Add to info section
info:
  title: DBBat API
  version: 1.0.0  # Already exists
```

#### 5. Update Swagger UI URL Reference (`internal/api/server.go`)

The Swagger UI handler references the OpenAPI spec URL. This should continue to point to `/api/openapi.yml` (non-versioned):

```go
// handleSwaggerUI (line ~195-223) - no change needed
// It already uses: url: "/api/openapi.yml"
```

### Frontend Changes

#### 1. Update API Base URL (`front/src/api/client.ts`)

Change the default API base URL:

```typescript
// Current (line ~4):
export const apiBaseUrl: string = import.meta.env.VITE_API_BASE_URL || "/api";

// New:
export const apiBaseUrl: string = import.meta.env.VITE_API_BASE_URL || "/api/v1";
```

#### 2. Regenerate API Schema Types

After updating the OpenAPI spec, regenerate the TypeScript types:

```bash
cd front
bun run generate-client
```

This will regenerate `front/src/api/schema.ts` with the updated paths.

**Note**: Since the frontend uses `openapi-fetch` with the `baseUrl` option, the actual API call paths in `front/src/api/queries.ts` don't need to change. They use relative paths like `/users` which get combined with the `baseUrl`.

### Documentation Changes

#### 1. Update CLAUDE.md

Update API endpoint examples in the REST API section:

```markdown
### Users
- `POST /api/v1/users` - Create user
- `GET /api/v1/users` - List users
- `PUT /api/v1/users/:id` - Update user
- `DELETE /api/v1/users/:id` - Delete user

### Databases
- `POST /api/v1/databases` - Create database configuration
- `GET /api/v1/databases` - List databases
...
```

Also update the manual testing section:

```bash
# Create a test user
curl -u admin:admin -X POST http://localhost:8080/api/v1/users \
  -H "Content-Type: application/json" \
  -d '{"username": "testuser", "password": "testpass", "roles": ["connector"]}'
```

### Test Changes

#### 1. Update E2E Tests

E2E tests don't directly call API endpoints (they go through the UI), so no changes needed.

#### 2. Update Integration Tests (if any)

If there are Go integration tests that make direct API calls, update them to use `/api/v1/`:

```go
// Before:
req := httptest.NewRequest("GET", "/api/users", nil)

// After:
req := httptest.NewRequest("GET", "/api/v1/users", nil)
```

## Migration Checklist

### Backend
- [x] Create `internal/version/version.go` package with build variables
- [x] Update Makefile to set ldflags for `internal/version` package
- [x] Update router in `internal/api/server.go` to use `/api/v1` group for versioned endpoints
- [x] Keep `/api/openapi.yml` and `/api/docs` outside versioning (documentation only)
- [x] Add `/api/v1/version` endpoint handler using `internal/version` package
- [x] Update `internal/api/openapi.yml` servers URL to `/api/v1`
- [x] Add version endpoint to OpenAPI spec
- [ ] Update any integration tests to use new paths

### Frontend
- [x] Update `apiBaseUrl` default in `front/src/api/client.ts` from `/api` to `/api/v1`
- [x] Run `bun run generate-client` to regenerate schema types
- [ ] Verify all API calls work with new base URL

### Documentation
- [x] Update `CLAUDE.md` with new API paths in REST API section
- [x] Update `CLAUDE.md` manual testing examples
- [ ] Update any docker-compose examples/scripts

### Verification
- [x] Run `make test` to verify backend tests pass
- [ ] Run `make test-e2e` to verify E2E tests pass
- [ ] Manual verification:
  - [ ] `curl http://localhost:8080/api/v1/version` returns version and build info
  - [ ] `curl http://localhost:8080/api/openapi.yml` returns spec
  - [ ] `curl http://localhost:8080/api/docs` shows Swagger UI
  - [ ] `curl -u admin:admin http://localhost:8080/api/v1/users` returns users
  - [ ] Frontend login and navigation works correctly

## Future Considerations

### When to Create v2

Create a new API version (v2) when introducing backwards-incompatible changes such as:
- Removing required fields from request bodies
- Changing response structure significantly
- Renaming fields or endpoints
- Changing authentication mechanisms
- Changing HTTP status codes for existing operations

### Supporting Multiple Versions

When v2 is introduced:

```go
// Documentation (not versioned)
api := router.Group("/api")
{
    api.GET("/openapi.yml", s.handleOpenAPISpec)
    api.GET("/docs", s.handleSwaggerUI)
}

// v1 routes
v1 := router.Group("/api/v1")
{
    v1.GET("/version", s.handleVersionV1)
    // ... v1 handlers
}

// v2 routes
v2 := router.Group("/api/v2")
{
    v2.GET("/version", s.handleVersionV2)
    // ... v2 handlers (can share some handlers with v1)
}
```

Version-specific logic can be handled in handlers or use separate handler functions when divergence is significant.

### Version Deprecation

When deprecating a version:
1. Announce deprecation with sunset date
2. Add deprecation headers to all responses
3. Update `/api/version` endpoint
4. Provide migration guide
5. Remove version after sunset date

```
Deprecation: true
Sunset: Mon, 01 Jan 2027 00:00:00 GMT
Link: <https://docs.dbbat.example.com/migration/v1-to-v2>; rel="deprecation"
```

## Testing

### Manual Testing

After implementation, verify all endpoints work with v1 prefix:

```bash
# OpenAPI spec (not versioned - documentation)
curl http://localhost:8080/api/openapi.yml

# Swagger UI (not versioned - documentation)
curl http://localhost:8080/api/docs

# Health (versioned)
curl http://localhost:8080/api/v1/health

# Version and build info (versioned)
curl http://localhost:8080/api/v1/version

# Auth
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "password": "admin"}'

# Users (with auth)
curl -u admin:admin http://localhost:8080/api/v1/users

# Databases
curl -u admin:admin http://localhost:8080/api/v1/databases

# Grants
curl -u admin:admin http://localhost:8080/api/v1/grants

# Connections
curl -u admin:admin http://localhost:8080/api/v1/connections

# Queries
curl -u admin:admin http://localhost:8080/api/v1/queries

# Audit
curl -u admin:admin http://localhost:8080/api/v1/audit

# API Keys
curl -u admin:admin http://localhost:8080/api/v1/keys
```

### Verify Old Paths Return 404

```bash
# Should return 404
curl -u admin:admin http://localhost:8080/api/users
curl -u admin:admin http://localhost:8080/api/databases
```

### Integration Tests

Add tests to verify:
- All v1 endpoints respond correctly (including `/api/v1/version`)
- Documentation endpoints (`/api/openapi.yml`, `/api/docs`) work without authentication
- Old `/api/*` paths return 404 (except documentation endpoints)
- Swagger UI renders correctly with new base URL
- Frontend can login and perform CRUD operations
