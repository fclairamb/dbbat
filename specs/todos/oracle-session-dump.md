# Oracle Session TNS Dump

## Goal

Add the ability to capture raw TNS packet traffic for Oracle proxy sessions into binary dump files. These dumps enable offline replay and analysis to improve TTC protocol parsing without needing live Oracle connections.

## Prerequisites

- Oracle proxy working (current state)

## Motivation

The TTC binary protocol varies across Oracle client drivers (Python oracledb thin, JDBC thin, go-ora, sqlplus), versions, and query types. The current heuristic-based SQL extraction and row capture produces garbage for some clients (e.g., DBeaver/JDBC). Raw dumps let us:

1. Capture real traffic from any client
2. Replay it in unit tests to build correct parsers
3. Debug issues without needing live Oracle access
4. Build a library of test fixtures from different client/version combinations

## Design

### File Format

Each dump is a binary file containing the full TNS conversation between client and upstream Oracle, with packet-level framing.

```
┌───────────────────────────────────────────────┐
│ Header (fixed)                                │
│   Magic:       "DBBAT_DUMP\x00\x00\x00\x00\x00\x00" (16 bytes)  │
│   Version:     uint16 BE (1)                  │
│   SessionUID:  [16]byte (UUID)                │
│   ServiceLen:  uint8                          │
│   ServiceName: [ServiceLen]byte               │
│   UpstreamLen: uint8                          │
│   UpstreamAddr:[UpstreamLen]byte              │
│   StartTime:   int64 BE (unix nanoseconds)    │
├───────────────────────────────────────────────┤
│ Packet 1                                      │
│   RelativeNs:  int64 BE (ns since start)      │
│   Direction:   uint8 (0=C→S, 1=S→C)          │
│   Length:      uint32 BE                      │
│   Data:        [Length]byte (raw TNS bytes)    │
├───────────────────────────────────────────────┤
│ Packet 2                                      │
│   ...                                         │
├───────────────────────────────────────────────┤
│ EOF marker                                    │
│   RelativeNs:  int64 BE                       │
│   Direction:   0xFF                           │
│   Length:      0                               │
└───────────────────────────────────────────────┘
```

File extension: `.dbbat-dump`

### Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_ORACLE_DUMP_DIR` | Directory for dump files. Empty = disabled. | (disabled) |
| `DBB_ORACLE_DUMP_MAX_SIZE` | Max dump file size per session (bytes) | `10485760` (10 MB) |
| `DBB_ORACLE_DUMP_RETENTION` | Auto-delete dumps older than this | `24h` |

Dumps are **disabled by default**. Setting `DBB_ORACLE_DUMP_DIR` to a directory path enables them.

### Storage

```
$DBB_ORACLE_DUMP_DIR/
  <connection-uid>.dbbat-dump
  <connection-uid>.dbbat-dump
  ...
```

One file per Oracle proxy session. Filename is the connection UID (same as in the `connections` table). Files are written sequentially as packets flow — no buffering of the entire session in memory.

### Implementation

#### Dump Writer — `internal/proxy/oracle/dump.go`

```go
type DumpWriter struct {
    file      *os.File
    startTime time.Time
    maxSize   int64
    written   int64
    mu        sync.Mutex
}

func NewDumpWriter(path string, sessionUID uuid.UUID, serviceName, upstreamAddr string, maxSize int64) (*DumpWriter, error)

// WritePacket writes a single TNS packet to the dump file.
// direction: 0 = client→upstream, 1 = upstream→client
func (d *DumpWriter) WritePacket(direction byte, data []byte) error

// Close writes the EOF marker and closes the file.
func (d *DumpWriter) Close() error
```

The writer:
- Creates the file and writes the header on construction
- Each `WritePacket` call appends a framed packet (timestamp + direction + length + data)
- Stops writing (silently) when `maxSize` is reached
- Is goroutine-safe (mutex-protected)

#### Dump Reader — `internal/proxy/oracle/dump.go`

```go
type DumpReader struct {
    file      *os.File
    Header    DumpHeader
}

type DumpHeader struct {
    SessionUID   uuid.UUID
    ServiceName  string
    UpstreamAddr string
    StartTime    time.Time
}

type DumpPacket struct {
    RelativeNs int64
    Direction  byte   // 0=C→S, 1=S→C, 0xFF=EOF
    Data       []byte
}

func OpenDump(path string) (*DumpReader, error)
func (r *DumpReader) ReadPacket() (*DumpPacket, error) // returns io.EOF at end
func (r *DumpReader) Close() error
```

The reader is used by:
- Unit tests (replay captured traffic through the parser)
- The API endpoint (stream the file to the client)
- Future: CLI tool for offline analysis

#### Integration with Session — `internal/proxy/oracle/session.go`

```go
// In session.run(), after connecting to upstream:
if s.dumpDir != "" {
    dumpPath := filepath.Join(s.dumpDir, s.connectionUID.String()+".dbbat-dump")
    s.dump, _ = NewDumpWriter(dumpPath, s.connectionUID, s.serviceName, upstreamAddr, s.dumpMaxSize)
}

// In clientToUpstream, after reading each packet:
if s.dump != nil {
    _ = s.dump.WritePacket(0, pkt.Raw) // client→upstream
}

// In upstreamToClient, after reading each packet:
if s.dump != nil {
    _ = s.dump.WritePacket(1, pkt.Raw) // upstream→client
}

// In cleanup():
if s.dump != nil {
    _ = s.dump.Close()
}
```

Dumping is fully transparent — it captures the exact raw bytes flowing in each direction without modifying them.

#### Cleanup — `internal/proxy/oracle/dump.go`

```go
// CleanupOldDumps deletes dump files older than retention period.
// Called periodically (e.g., every hour).
func CleanupOldDumps(dir string, retention time.Duration) (int, error)
```

### API

#### `GET /api/v1/connections/:uid/dump`

Download the raw TNS dump for a connection.

**Authentication**: Required (admin or viewer role)

**Response**:
- `200 OK` with `Content-Type: application/octet-stream` and `Content-Disposition: attachment; filename="<uid>.dbbat-dump"`
- `404 Not Found` if no dump exists for this connection

**Implementation**: Stream the file directly from disk (no loading into memory).

```go
func (s *Server) handleGetConnectionDump(c *gin.Context) {
    uid, _ := parseUIDParam(c)
    
    dumpPath := filepath.Join(s.config.OracleDumpDir, uid.String()+".dbbat-dump")
    if _, err := os.Stat(dumpPath); os.IsNotExist(err) {
        writeError(c, http.StatusNotFound, ErrCodeNotFound, "no dump available for this connection")
        return
    }
    
    c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.dbbat-dump"`, uid))
    c.File(dumpPath)
}
```

#### `DELETE /api/v1/connections/:uid/dump`

Delete a dump file. Admin only.

#### `GET /api/v1/connections/:uid`

Add a `has_dump` boolean field to the connection response:

```json
{
    "uid": "...",
    "has_dump": true,
    ...
}
```

### OpenAPI

```yaml
/connections/{uid}/dump:
  get:
    summary: Download Oracle session TNS dump
    description: Download the raw TNS packet capture for an Oracle proxy session.
    operationId: getConnectionDump
    tags: [Observability]
    parameters:
      - in: path
        name: uid
        required: true
        schema:
          type: string
          format: uuid
    responses:
      '200':
        description: TNS dump file
        content:
          application/octet-stream:
            schema:
              type: string
              format: binary
      '404':
        $ref: '#/components/responses/NotFound'
  delete:
    summary: Delete Oracle session TNS dump
    operationId: deleteConnectionDump
    tags: [Observability]
    parameters:
      - in: path
        name: uid
        required: true
        schema:
          type: string
          format: uuid
    responses:
      '204':
        description: Dump deleted
      '404':
        $ref: '#/components/responses/NotFound'
```

### Replay Tool

A Python script for analyzing dumps (shipped in `scripts/`):

```python
#!/usr/bin/env python3
"""Replay and analyze a .dbbat-dump file."""

import struct, sys

def read_dump(path):
    with open(path, 'rb') as f:
        # Read header
        magic = f.read(16)
        assert magic[:10] == b'DBBAT_DUMP'
        version = struct.unpack('>H', f.read(2))[0]
        session_uid = f.read(16)
        svc_len = f.read(1)[0]
        service = f.read(svc_len).decode()
        up_len = f.read(1)[0]
        upstream = f.read(up_len).decode()
        start_ns = struct.unpack('>q', f.read(8))[0]
        
        print(f"Session: {session_uid.hex()}")
        print(f"Service: {service}")
        print(f"Upstream: {upstream}")
        
        # Read packets
        n = 0
        while True:
            rel_ns = struct.unpack('>q', f.read(8))[0]
            direction = f.read(1)[0]
            if direction == 0xFF:
                break
            length = struct.unpack('>I', f.read(4))[0]
            data = f.read(length)
            n += 1
            
            dir_str = "C→S" if direction == 0 else "S→C"
            pkt_type = data[4] if len(data) > 4 else 0
            ms = rel_ns / 1_000_000
            print(f"  #{n:3d} [{ms:8.1f}ms] {dir_str} type={pkt_type:2d} {len(data)} bytes")
        
        print(f"\nTotal: {n} packets")

if __name__ == '__main__':
    read_dump(sys.argv[1])
```

### Tests

```go
func TestDumpWriter_WritesHeader(t *testing.T) {
    // Create dump, verify magic + header fields
}

func TestDumpWriter_WritePacket(t *testing.T) {
    // Write packets, read back with DumpReader, verify
}

func TestDumpWriter_MaxSize(t *testing.T) {
    // Write beyond maxSize, verify file doesn't grow
}

func TestDumpWriter_RoundTrip(t *testing.T) {
    // Write multiple packets in both directions
    // Read back with DumpReader
    // Verify timestamps, directions, data match
}

func TestDumpReader_InvalidFile(t *testing.T) {
    // Open non-dump file, verify error
}

func TestCleanupOldDumps(t *testing.T) {
    // Create old files, run cleanup, verify deleted
}
```

### Usage Workflow

1. Enable dumps: `DBB_ORACLE_DUMP_DIR=/tmp/dbbat-dumps`
2. Connect with any Oracle client (DBeaver, sqlplus, Python, etc.)
3. Run queries
4. Find the connection in the UI or API
5. Download the dump: `curl -H "Authorization: Bearer $TOKEN" https://dbbat.../api/v1/connections/<uid>/dump -o session.dbbat-dump`
6. Analyze with the replay script: `python3 scripts/replay_dump.py session.dbbat-dump`
7. Use in Go tests: feed the dump through `decodeQueryResultV2` to iterate on parsing

### Files

| File | Type | Description |
|------|------|-------------|
| `internal/proxy/oracle/dump.go` | New | DumpWriter + DumpReader + cleanup |
| `internal/proxy/oracle/dump_test.go` | New | Tests |
| `internal/proxy/oracle/session.go` | Modified | Integrate dump writer |
| `internal/proxy/oracle/server.go` | Modified | Pass dump config to sessions |
| `internal/api/observability.go` | Modified | Add dump download/delete endpoints |
| `internal/api/server.go` | Modified | Register dump routes |
| `internal/api/openapi.yml` | Modified | Document dump endpoints |
| `internal/config/config.go` | Modified | Add dump config fields |
| `scripts/replay_dump.py` | New | Python replay/analysis tool |

### Acceptance Criteria

1. Setting `DBB_ORACLE_DUMP_DIR=/path` creates dump files for Oracle sessions
2. Dump files contain all raw TNS packets with timestamps and direction
3. `GET /api/v1/connections/:uid/dump` downloads the dump file
4. `DumpReader` can parse dump files written by `DumpWriter`
5. Dumps are capped at `DBB_ORACLE_DUMP_MAX_SIZE` (default 10MB)
6. Old dumps are cleaned up after `DBB_ORACLE_DUMP_RETENTION` (default 24h)
7. Dump writing doesn't impact proxy latency (no blocking I/O in the relay path)
8. `scripts/replay_dump.py` can read and display dump contents
9. Existing Oracle proxy functionality unaffected when dumps are disabled

### Estimated Size

~200 lines dump writer/reader + ~100 lines tests + ~50 lines session integration + ~30 lines API + ~50 lines Python script = **~430 lines total**
