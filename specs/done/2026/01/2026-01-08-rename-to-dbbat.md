# Spec: Rename PgLens to DBBat

**Date**: 2026-01-08
**Status**: Draft
**Author**: Claude

## Summary

Rename the project from "PgLens" to "DBBat" to provide a short, memorable name for the product that provides temporary, controlled database access for developers and third parties with complete monitoring.

## Rationale

The name "DBBat" is:
- **Short and memorable**: Only 5 characters
- **Easy to pronounce**: "dee-bee-bat"
- **Database focus**: Clear association with database technology

The domain **dbbat.com** is available for registration.

## Scope of Changes

### 1. Environment Variables

| Current | New |
|---------|-----|
| `PGL_LISTEN_ADDR` | `DBB_LISTEN_PG` |
| `PGL_API_ADDR` | `DBB_LISTEN_API` |
| `PGL_DSN` | `DBB_DSN` |
| `PGL_KEY` | `DBB_KEY` |
| `PGL_KEYFILE` | `DBB_KEYFILE` |

### 2. Go Module

| Current | New |
|---------|-----|
| `github.com/fclairamb/pglens` | `github.com/fclairamb/dbbat` |

### 3. Binary Name

| Current | New |
|---------|-----|
| `pglens` | `dbbat` |

### 4. Directory Structure

| Current | New |
|---------|-----|
| `cmd/pglens/` | `cmd/dbbat/` |

### 5. Code References

All occurrences of "pglens" or "PgLens" in:
- Go source files (comments, strings, identifiers)
- Configuration files
- Documentation (CLAUDE.md, README.md if exists)
- Docker and docker-compose files
- OpenAPI specification
- SQL migrations (comments)
- Test files

### 6. Application Name in Connections

| Current | New |
|---------|-----|
| `pglens-{version}` | `dbbat-{version}` |
| `pglens-{version} / {client}` | `dbbat-{version} / {client}` |

### 7. Database Tables (Optional)

Consider whether internal table names should change. Current tables use generic names (`users`, `databases`, `queries`, etc.) which don't need renaming.

### 8. Default Key File Path

| Current | New |
|---------|-----|
| `~/.pglens/key` | `~/.dbbat/key` |

### 9. Git Repository

| Current | New |
|---------|-----|
| `github.com/fclairamb/pglens` | `github.com/fclairamb/dbbat` |

## Implementation Plan

### Phase 1: Code Changes

#### 1.1 Update go.mod

```go
// Before
module github.com/fclairamb/pglens

// After
module github.com/fclairamb/dbbat
```

#### 1.2 Rename cmd directory

```bash
git mv cmd/pglens cmd/dbbat
```

#### 1.3 Update Environment Variable Prefix

**File**: `internal/config/config.go`

```go
// Before
const envPrefix = "PGL_"

// After
const envPrefix = "DBB_"
```

#### 1.4 Update Default Key File Path

**File**: `internal/config/config.go`

```go
// Before
defaultKeyFile := filepath.Join(home, ".pglens", "key")

// After
defaultKeyFile := filepath.Join(home, ".dbbat", "key")
```

#### 1.5 Update Application Name

**File**: `internal/version/version.go`

```go
// Before
const AppName = "pglens"

// After
const AppName = "dbbat"
```

**File**: `internal/proxy/upstream.go`

```go
// Before
pglensName := "pglens-" + version.Version

// After
appName := "dbbat-" + version.Version
```

#### 1.6 Update All Import Paths

All files with imports from `github.com/fclairamb/pglens` must be updated to `github.com/fclairamb/dbbat`.

### Phase 2: Configuration Files

#### 2.1 docker-compose.yml

```yaml
# Before
services:
  pglens:
    build: .
    environment:
      - PGL_DSN=...
      - PGL_KEY=...

# After
services:
  dbbat:
    build: .
    environment:
      - DBB_DSN=...
      - DBB_KEY=...
```

#### 2.2 Dockerfile (if exists)

```dockerfile
# Before
COPY --from=builder /app/pglens /usr/local/bin/pglens

# After
COPY --from=builder /app/dbbat /usr/local/bin/dbbat
```

### Phase 3: Documentation

#### 3.1 CLAUDE.md

- Update all references to "PgLens" → "DBBat"
- Update all references to "pglens" → "dbbat"
- Update environment variable examples
- Update CLI examples

#### 3.2 OpenAPI Specification

**File**: `docs/openapi.yml`

Update title, description, and any references to PgLens.

### Phase 4: Repository Migration

1. Create new repository `github.com/fclairamb/dbbat`
2. Push updated code
3. Update go.mod module path
4. Archive old repository (optional)

## Search and Replace Summary

### Case-sensitive replacements

| Search | Replace |
|--------|---------|
| `pglens` | `dbbat` |
| `PgLens` | `DBBat` |
| `PGLENS` | `DBBAT` |
| `PGL_` | `DBB_` |
| `Pglens` | `Dbbat` |

### Files to modify

Run the following to identify all files:

```bash
# Find all Go files with pglens references
grep -r -l "pglens\|PgLens\|PGL_" --include="*.go" .

# Find all config/doc files
grep -r -l "pglens\|PgLens\|PGL_" --include="*.yml" --include="*.yaml" --include="*.md" --include="*.sql" .
```

## Backward Compatibility

No backward compatibility is required. This is a clean rename with no support for legacy `PGL_` environment variables or `~/.pglens/` paths. Existing deployments must update their configuration to use the new naming.

## Testing

1. Run all existing tests after renaming
2. Verify environment variables work with new prefix
3. Verify key file is found in new location
4. Verify `application_name` shows `dbbat-{version}` in `pg_stat_activity`
5. Verify docker-compose works with new configuration

## Rollout Checklist

- [ ] Update go.mod module path
- [ ] Rename `cmd/pglens` to `cmd/dbbat`
- [ ] Update environment variable prefix to `DBB_`
- [ ] Update default key file path to `~/.dbbat/key`
- [ ] Update application name in upstream connections
- [ ] Update all import paths
- [ ] Update CLAUDE.md
- [ ] Update docker-compose.yml
- [ ] Update OpenAPI spec
- [ ] Run all tests
- [ ] Create new GitHub repository
- [ ] Register dbbat.com domain
- [ ] Update any CI/CD configurations

## Success Criteria

1. All tests pass with new naming
2. Binary is named `dbbat`
3. Environment variables use `DBB_` prefix
4. Key file defaults to `~/.dbbat/key`
5. Upstream connections show `dbbat-{version}` in `pg_stat_activity`
6. Documentation accurately reflects new naming
