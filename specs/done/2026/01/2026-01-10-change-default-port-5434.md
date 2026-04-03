# Change Default Proxy Port to 5434

## Overview

Change the default PostgreSQL proxy listen port from `5432` to `5434` to avoid conflicts with local PostgreSQL installations and improve the developer experience.

GitHub Issue: [#4](https://github.com/fclairamb/dbbat/issues/4)

## Motivation

The current default port `5432` is the standard PostgreSQL port. This creates friction:

1. **Local development**: Developers often have PostgreSQL running locally on port 5432
2. **Demo environments**: Running DBBat requires stopping local PostgreSQL or specifying a custom port
3. **Quick start experience**: Port conflicts on first run create a poor first impression

Port `5434` (PostgreSQL + 2) is a common convention for proxy/alternate PostgreSQL services and avoids these conflicts while remaining easy to remember.

## Current State Analysis

### Files Using Port 5432

| File | Usage | Action |
|------|-------|--------|
| `internal/config/config.go` | Default `ListenPG: ":5432"` | Change to `:5434` |
| `CLAUDE.md` | Documentation `DBB_LISTEN_PG` default | Update |
| `docker-compose.yml` | Port mapping `5001:5432` | Change to `5001:5434` |
| `website/docs/intro.md` | Quick start examples | Update if present |
| `website/docs/installation/*.md` | Installation guides | Update if present |

### Port Alternatives Considered

| Port | Pros | Cons |
|------|------|------|
| 5433 | PostgreSQL+1, common for replicas | Often used by pgbouncer |
| 5434 | PostgreSQL+2, rarely used | Less intuitive than 5433 |
| 15432 | Clear it's non-standard | Harder to remember |

**Decision:** `5434` - avoids pgbouncer conflicts while staying close to the standard port.

## Implementation

### 1. Configuration Default

**File:** `internal/config/config.go`

```go
// Before
ListenPG: ":5432"

// After
ListenPG: ":5434"
```

### 2. Docker Compose

**File:** `docker-compose.yml`

Update the dbbat service port mapping:

```yaml
# Before
ports:
  - "5001:5432"

# After
ports:
  - "5001:5434"
```

Note: The external port 5001 is used for testing purposes. The internal port should match the default.

### 3. Documentation Updates

**File:** `CLAUDE.md`

Update the environment variables table:
```markdown
| `DBB_LISTEN_PG` | Proxy listen address (default: `:5434`) | No |
```

**File:** `website/docs/` (if applicable)

Search for and update any references to port 5432 in documentation.

## Migration Impact

This is a **breaking change** for users who:
- Rely on the default port 5432 without explicit configuration
- Have connection strings without port specified

**Mitigation:** Users can set `DBB_LISTEN_PG=:5432` to restore the previous behavior.

## Testing

Existing tests should continue to pass as they use explicit ports. Verify:

1. `make test` passes
2. `make test-e2e` passes
3. Manual testing with docker-compose works

## Files to Modify

1. `internal/config/config.go` - Change default port
2. `docker-compose.yml` - Update port mapping
3. `CLAUDE.md` - Update documentation

## Files to Check (may need updates)

1. `website/docs/intro.md`
2. `website/docs/installation/*.md`
3. Any other documentation referencing port 5432

## Acceptance Criteria

- [ ] Default port changed to 5434 in config
- [ ] docker-compose.yml updated
- [ ] CLAUDE.md documentation updated
- [ ] Website documentation updated (if applicable)
- [ ] All existing tests pass
