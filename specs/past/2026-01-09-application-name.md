# Spec: Application Name in Upstream Connections

**Date**: 2026-01-08
**Status**: Draft
**Author**: Claude

## Summary

When PgLens connects to upstream PostgreSQL databases, it should identify itself using the `application_name` connection parameter. This allows DBAs to easily identify connections originating from PgLens in PostgreSQL's `pg_stat_activity` and logs.

## Problem Statement

### Current Behavior

PgLens currently does not set a specific `application_name` when connecting to upstream PostgreSQL databases. This makes it difficult to:

1. Identify which connections in `pg_stat_activity` are coming through PgLens
2. Filter logs for PgLens-proxied connections
3. Distinguish between direct connections and proxied connections

### Desired Behavior

PgLens should set a descriptive `application_name` that:

1. Identifies the connection as coming from PgLens
2. Includes the PgLens version for debugging and auditing
3. Preserves the original client's `application_name` when provided

## Design

### Application Name Format

When connecting to upstream PostgreSQL, PgLens should set `application_name` as follows:

| Client `application_name` | Upstream `application_name` |
|---------------------------|------------------------------|
| Not set / empty | `pglens-1.0.0` |
| `psql` | `pglens-1.0.0 / psql` |
| `myapp` | `pglens-1.0.0 / myapp` |

Format: `pglens-{version}` or `pglens-{version} / {client_app_name}`

### Why Include Version

Including the version in `application_name` helps with:

- Debugging issues related to specific PgLens versions
- Auditing which version was used for a particular query
- Identifying outdated proxy instances in multi-instance deployments

### Why Preserve Client Application Name

Preserving the client's `application_name` helps with:

- Tracing queries back to the originating application
- Maintaining visibility into what tools/applications are using the database
- Debugging application-specific issues

## Implementation

### 1. Define Version Constant

**File**: `internal/version/version.go` (new file)

```go
package version

// Version is the current PgLens version.
// This should be updated for each release.
var Version = "0.1.0"
```

### 2. Build Application Name

**File**: `internal/proxy/upstream.go`

Add a helper function to build the application name:

```go
import "github.com/fclairamb/pglens/internal/version"

// buildApplicationName constructs the application_name for upstream connections.
// If the client provided an application_name, it is appended after the PgLens identifier.
func buildApplicationName(clientAppName string) string {
    pglensName := "pglens-" + version.Version

    clientAppName = strings.TrimSpace(clientAppName)
    if clientAppName == "" {
        return pglensName
    }

    return pglensName + " / " + clientAppName
}
```

### 3. Extract Client Application Name from StartupMessage

**File**: `internal/proxy/session.go`

When processing the client's StartupMessage, extract the `application_name` parameter:

```go
func (s *Session) handleStartup(msg *pgproto3.StartupMessage) error {
    // Extract client application name
    clientAppName := ""
    for key, value := range msg.Parameters {
        if key == "application_name" {
            clientAppName = value
            break
        }
    }

    // Store for later use when connecting upstream
    s.clientApplicationName = clientAppName

    // ... rest of startup handling
}
```

### 4. Set Application Name in Upstream Connection

**File**: `internal/proxy/upstream.go`

When building the upstream connection config:

```go
func (s *Session) connectUpstream(dbConfig *store.Database) error {
    connConfig, err := pgx.ParseConfig(dbConfig.ConnectionString())
    if err != nil {
        return fmt.Errorf("failed to parse connection string: %w", err)
    }

    // Set application_name with PgLens prefix
    appName := buildApplicationName(s.clientApplicationName)
    connConfig.RuntimeParams["application_name"] = appName

    // ... establish connection
}
```

### 5. Add Session Field

**File**: `internal/proxy/session.go`

Add field to Session struct:

```go
type Session struct {
    // ... existing fields

    // clientApplicationName is the application_name provided by the client
    clientApplicationName string
}
```

## Testing

### Unit Tests

**File**: `internal/proxy/upstream_test.go`

```go
func TestBuildApplicationName(t *testing.T) {
    tests := []struct {
        name          string
        clientAppName string
        want          string
    }{
        {
            name:          "empty client app name",
            clientAppName: "",
            want:          "pglens-0.1.0",
        },
        {
            name:          "whitespace only",
            clientAppName: "   ",
            want:          "pglens-0.1.0",
        },
        {
            name:          "psql client",
            clientAppName: "psql",
            want:          "pglens-0.1.0 / psql",
        },
        {
            name:          "custom app name",
            clientAppName: "myapp",
            want:          "pglens-0.1.0 / myapp",
        },
        {
            name:          "app name with spaces",
            clientAppName: "My Application",
            want:          "pglens-0.1.0 / My Application",
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := buildApplicationName(tt.clientAppName)
            if got != tt.want {
                t.Errorf("buildApplicationName(%q) = %q, want %q", tt.clientAppName, got, tt.want)
            }
        })
    }
}
```

### Integration Test

Verify that `pg_stat_activity` shows the correct `application_name`:

```go
func TestUpstreamApplicationName(t *testing.T) {
    // Connect through PgLens with application_name=testclient
    // Query pg_stat_activity on the target database
    // Verify application_name matches "pglens-X.Y.Z / testclient"
}
```

### Manual Verification

After connecting through PgLens, run on the target database:

```sql
SELECT application_name, usename, client_addr
FROM pg_stat_activity
WHERE application_name LIKE 'pglens-%';
```

Expected output:

```
      application_name       | usename | client_addr
-----------------------------+---------+-------------
 pglens-0.1.0 / psql         | dbuser  | 10.0.0.1
```

## Files to Modify

| File | Changes |
|------|---------|
| `internal/version/version.go` | New file: version constant |
| `internal/proxy/session.go` | Add `clientApplicationName` field, extract from StartupMessage |
| `internal/proxy/upstream.go` | Add `buildApplicationName()`, set `application_name` in connection config |
| `internal/proxy/upstream_test.go` | Add unit tests for `buildApplicationName()` |

## Considerations

### Maximum Length

PostgreSQL's `application_name` has a maximum length of `NAMEDATALEN - 1` (typically 63 characters). If the combined name exceeds this limit, it will be truncated by PostgreSQL. Consider truncating the client application name if needed:

```go
const maxAppNameLen = 63

func buildApplicationName(clientAppName string) string {
    pglensName := "pglens-" + version.Version

    clientAppName = strings.TrimSpace(clientAppName)
    if clientAppName == "" {
        return pglensName
    }

    combined := pglensName + " / " + clientAppName
    if len(combined) > maxAppNameLen {
        // Truncate client app name to fit
        maxClientLen := maxAppNameLen - len(pglensName) - 3 // " / " = 3 chars
        if maxClientLen > 0 {
            clientAppName = clientAppName[:maxClientLen]
            combined = pglensName + " / " + clientAppName
        } else {
            combined = pglensName
        }
    }

    return combined
}
```

### Version at Build Time

For production builds, the version should ideally be set at build time using ldflags:

```bash
go build -ldflags "-X github.com/fclairamb/pglens/internal/version.Version=1.2.3"
```

## Success Criteria

1. All upstream connections show `pglens-{version}` prefix in `pg_stat_activity`
2. Client `application_name` is preserved and appended after the PgLens prefix
3. Empty/missing client `application_name` results in just `pglens-{version}`
4. Application name does not exceed PostgreSQL's 63-character limit
5. All unit tests pass
