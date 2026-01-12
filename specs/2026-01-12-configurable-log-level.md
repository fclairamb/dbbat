# Configurable Log Level

## Overview

Add the ability to configure the log level for the DBBat application. Currently, the log level is hardcoded to `debug`, which produces verbose output unsuitable for production environments. This feature allows operators to control log verbosity via environment variable or CLI flag.

## Goals

1. **Production-ready logging**: Allow operators to reduce log noise in production
2. **Debugging support**: Maintain ability to enable verbose logging when troubleshooting
3. **Consistency**: Follow existing configuration patterns (environment variables with `DBB_` prefix)
4. **Compliance**: Ensure implementation passes sloglint static analysis

## Requirements

### 1. Environment Variable

**Variable**: `DBB_LOG_LEVEL`

**Accepted values** (case-insensitive):
| Value | slog Level | Description |
|-------|------------|-------------|
| `debug` | `slog.LevelDebug` | Most verbose, includes debug information |
| `info` | `slog.LevelInfo` | Standard operational messages |
| `warn` | `slog.LevelWarn` | Warnings and above |
| `error` | `slog.LevelError` | Errors only |

**Default**: `info` (production-friendly default)

**Invalid values**: Log a warning and fall back to `info`

### 2. CLI Flag

**Flag**: `--log-level` or `-l`

**Scope**: Global flag available on all commands (`serve`, `db migrate`, etc.)

**Priority**: CLI flag takes precedence over environment variable

### 3. Configuration Integration

Add `LogLevel` field to the `Config` struct in `internal/config/config.go`:

```go
type Config struct {
    // ... existing fields ...
    LogLevel string `koanf:"log_level"`
}
```

### 4. Logger Setup Changes

Modify `setupLogger()` in `main.go` to accept the log level:

```go
func setupLogger(runMode config.RunMode, level slog.Level) (*slog.Logger, func()) {
    // ... existing writer setup ...

    logger := slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{
        Level: level,
    }))

    return logger, cleanup
}
```

### 5. Level Parsing

Add a helper function to parse string to slog.Level:

```go
func parseLogLevel(level string) slog.Level {
    switch strings.ToLower(level) {
    case "debug":
        return slog.LevelDebug
    case "info":
        return slog.LevelInfo
    case "warn", "warning":
        return slog.LevelWarn
    case "error":
        return slog.LevelError
    default:
        // Log warning about invalid level, return default
        return slog.LevelInfo
    }
}
```

### 6. Sloglint Compliance

The implementation must pass `sloglint` static analysis. Key requirements:

- Use context.Context variable as first argument in slog calls
- Use consistent key-value pairs in slog calls (no positional arguments mixing)
- Use `slog.String()`, `slog.Int()`, etc. for typed attributes
- Ensure all slog calls have even number of arguments after message

Example of compliant code:
```go
slog.InfoContext(ctx, "server starting", slog.String("address", addr), slog.Int("port", port))
```

## Implementation Plan

### 1. Config Changes (`internal/config/config.go`)

1. Add `LogLevel` field to `Config` struct
2. Add default value `"info"` in `setDefaults()`
3. Map `DBB_LOG_LEVEL` environment variable

### 2. Main Changes (`main.go`)

1. Add `parseLogLevel()` function
2. Add `--log-level` global CLI flag
3. Modify `setupLogger()` signature to accept level
4. Update all `setupLogger()` call sites

### 3. CLI Flag Definition

Add to the root command flags:

```go
&cli.StringFlag{
    Name:    "log-level",
    Aliases: []string{"l"},
    Usage:   "Set log level (debug, info, warn, error)",
    Sources: cli.EnvVars("DBB_LOG_LEVEL"),
}
```

## Usage Examples

```bash
# Via environment variable
DBB_LOG_LEVEL=warn ./dbbat serve

# Via CLI flag
./dbbat --log-level=debug serve

# CLI flag overrides environment variable
DBB_LOG_LEVEL=error ./dbbat --log-level=debug serve  # Uses debug

# Default behavior (info level)
./dbbat serve
```

## Testing

### Unit Tests

1. Test `parseLogLevel()` with valid values (debug, info, warn, error)
2. Test `parseLogLevel()` with case variations (DEBUG, Info, WARN)
3. Test `parseLogLevel()` with invalid values (returns info)
4. Test `parseLogLevel()` with "warning" alias

### Integration Tests

1. Verify debug messages appear when level is `debug`
2. Verify debug messages are suppressed when level is `info`
3. Verify CLI flag overrides environment variable

## Documentation Updates

1. Update `CLAUDE.md` environment variables table to include `DBB_LOG_LEVEL`
2. Update CLI help text

## Security Considerations

None. Log level configuration does not affect security posture.

## Future Enhancements

- Support for log format selection (JSON vs text)
- Support for log output destination (stdout, file, syslog)
- Per-component log levels (e.g., `DBB_LOG_LEVEL_PROXY=debug`)
