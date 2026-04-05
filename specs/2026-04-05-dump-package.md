# Protocol-Agnostic Dump Package

## Overview

Extract dump file reading/writing into a dedicated, protocol-agnostic package (`internal/dump`) that any proxy (Oracle, PostgreSQL, future protocols) can import. The current implementation lives inside `internal/proxy/oracle/` and has Oracle-specific fields baked into the binary header. The new package uses a JSON metadata header to support any protocol without format changes.

See also: [docs/dump-format.md](../docs/dump-format.md) for the binary format specification.

## Goals

1. **Protocol-agnostic** — one package, one format, any database protocol
2. **Zero coupling** — `internal/dump` has no dependency on proxy packages or `internal/store`
3. **Drop-in migration** — Oracle proxy switches from its embedded dump code to `internal/dump` with minimal changes
4. **No v1 support** — the new package only reads/writes v2 format. Existing v1 dumps become unsupported

## Package Location

```
internal/dump/
├── writer.go       # Writer and Header types
├── reader.go       # Reader
├── cleanup.go      # File retention cleanup
├── dump.go         # Shared constants, errors, types
├── dump_test.go    # Tests
```

## Public API

### Types

```go
// Header holds the JSON-serializable session metadata.
type Header struct {
    SessionID  string         `json:"session_id"`
    Protocol   string         `json:"protocol"`
    StartTime  time.Time      `json:"start_time"`
    Connection map[string]any `json:"connection"`
}
```

`Connection` is a `map[string]any` — each protocol puts whatever it needs. The dump package does not interpret it. Callers are responsible for populating and reading the right keys.

Protocol constants for convenience:

```go
const (
    ProtocolOracle     = "oracle"
    ProtocolPostgreSQL = "postgresql"
)
```

```go
// Packet represents a single captured packet.
type Packet struct {
    RelativeNs int64  // Nanoseconds since session start
    Direction  byte   // DirClientToServer or DirServerToClient
    Data       []byte // Raw protocol bytes
}

const (
    DirClientToServer byte = 0x00
    DirServerToClient byte = 0x01
)
```

### Writer

```go
// NewWriter creates a dump file and writes the file header + JSON header.
func NewWriter(path string, header Header, maxSize int64) (*Writer, error)

// WritePacket appends a single packet. Thread-safe.
// Silently skips if maxSize would be exceeded.
func (w *Writer) WritePacket(direction byte, data []byte) error

// Close writes the EOF marker and closes the file.
func (w *Writer) Close() error
```

Key behaviors:
- `NewWriter` writes the magic, version (2), header length, and JSON header immediately
- `WritePacket` records `time.Since(startTime)` as the relative timestamp
- `maxSize` applies to the entire file. `0` means unlimited
- `WritePacket` is safe for concurrent use (internal mutex)

### Reader

```go
// OpenReader opens a dump file and parses the header.
func OpenReader(path string) (*Reader, error)

// Header returns the parsed JSON header.
func (r *Reader) Header() Header

// ReadPacket reads the next packet.
// Returns io.EOF after the EOF marker.
func (r *Reader) ReadPacket() (*Packet, error)

// Close closes the underlying file.
func (r *Reader) Close() error
```

Key behaviors:
- `OpenReader` validates magic, checks version == 2, reads JSON header length, parses JSON
- Returns `ErrInvalidMagic` or `ErrUnsupportedVersion` on failure

### Cleanup

```go
// CleanupOldFiles deletes .dbbat-dump files older than retention in the given directory.
// Returns the number of files deleted.
func CleanupOldFiles(dir string, retention time.Duration) (int, error)
```

Moved from `oracle.CleanupOldDumps` — identical logic, same file extension filter.

### Errors

```go
var (
    ErrInvalidMagic       = errors.New("invalid dump file magic")
    ErrUnsupportedVersion = errors.New("unsupported dump format version")
)
```

## Constants

```go
const (
    Magic     = "DBBAT_DUMP\x00\x00\x00\x00\x00\x00" // 16 bytes
    Version   = uint16(2)
    FileExt   = ".dbbat-dump"
    EOFMarker = byte(0xFF)
)
```

## Binary Layout (summary)

```
[16B magic][2B version=2][4B header_len][header_len bytes JSON]
[13B frame + data]*
[13B EOF marker]
```

Packet frame: `[8B relative_ns BE][1B direction][4B data_len BE][data_len bytes]`

Full details in [docs/dump-format.md](../docs/dump-format.md).

## Migration from Oracle Dump Code

### What changes in `internal/proxy/oracle/`

1. **Delete** `dump.go` and `dump_test.go`
2. **In `session.go`**: replace `*DumpWriter` with `*dump.Writer`, update the creation call:

```go
// Before
dw, err := NewDumpWriter(dumpPath, s.connectionUID, s.serviceName, upstreamAddr, s.dumpConfig.MaxSize)

// After
dw, err := dump.NewWriter(dumpPath, dump.Header{
    SessionID:  s.connectionUID.String(),
    Protocol:   dump.ProtocolOracle,
    StartTime:  time.Now(),
    Connection: map[string]any{
        "service_name":  s.serviceName,
        "upstream_addr": upstreamAddr,
    },
}, s.dumpConfig.MaxSize)
```

3. **In `session.go`**: replace `DumpDirClientToServer`/`DumpDirServerToClient` with `dump.DirClientToServer`/`dump.DirServerToClient`
4. **In `server.go`**: replace `CleanupOldDumps` with `dump.CleanupOldFiles`
5. **Move `dump_replay_test.go`** to `internal/dump/` or keep in oracle with the new import

### What changes in config

The `OracleDumpConfig` struct stays in `internal/config/` for now — it's configuration, not dump logic. When PostgreSQL dump support is added, consider generalizing to a shared `DumpConfig`.

## Testing

### Unit Tests

| Test | Description |
|------|-------------|
| `TestWriter_Header` | Write + read back, verify all JSON fields round-trip |
| `TestWriter_WritePacket` | Write packets, read back, verify direction + data + timestamps |
| `TestWriter_MaxSize` | Exceed max size, verify packets are silently dropped |
| `TestWriter_RoundTrip` | Multiple packets both directions, full read-back |
| `TestReader_InvalidMagic` | Garbage file returns `ErrInvalidMagic` |
| `TestReader_UnsupportedVersion` | Valid magic + version=99 returns `ErrUnsupportedVersion` |
| `TestReader_EmptyConnection` | Header with empty connection map works |
| `TestReader_ExtraJSONFields` | Unknown JSON keys are preserved (no error, ignored on read) |
| `TestCleanupOldFiles` | Old files deleted, recent files and non-dump files kept |
| `TestWriter_ConcurrentWrites` | Multiple goroutines calling WritePacket simultaneously |

### Integration

The Oracle proxy's existing `dump_replay_test.go` (which replays real captured sessions) should keep working after switching to the new import — the test just needs to use `dump.OpenReader` instead of `oracle.OpenDump`.

## Scope Boundaries

**In scope:**
- `internal/dump` package with Writer, Reader, CleanupOldFiles
- Migration of Oracle proxy to use the new package
- Tests

**Out of scope:**
- PostgreSQL dump capture (future work — the format supports it, wiring it up is a separate task)
- Dump analysis/inspection CLI command
- Compression (can be added later as a format v3 feature)
- v1 backward compatibility
