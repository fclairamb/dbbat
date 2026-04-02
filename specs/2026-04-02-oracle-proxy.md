# Oracle Protocol Proxy

## Overview

Extend DBBat to support Oracle Database connections alongside PostgreSQL. Same value proposition: transparent proxy for query observability, access control, and safety — but for Oracle's TNS/TTC wire protocol.

## Why This Is Hard

The PostgreSQL proxy (~1,800 lines) benefits from:
- A clean, well-documented wire protocol with ~30 message types
- `pgproto3` — a purpose-built Go codec library for the PG wire protocol
- Simple auth (cleartext/MD5) with clear message boundaries
- SQL text visible directly in `Query`/`Parse` messages

Oracle uses two layered proprietary protocols:
- **TNS** (Transparent Network Substrate): transport layer — connection setup, packet framing, redirects
- **TTC** (Two-Task Common): data layer — rides inside TNS Data packets, uses numbered opcodes (70+) for all operations

There is no `pgproto3` equivalent for Oracle. The closest is [go-ora](https://github.com/sijms/go-ora), which is a client driver — not a protocol codec. We'd need to extract and adapt its encoding/decoding.

## Phased Approach

### Phase 1: Connection & Authentication
### Phase 2: Transparent Message Proxying with Query Interception
### Phase 3: Result Capture & Storage

---

## Phase 1: Connection & Authentication

### Goal
Accept Oracle client connections on a dedicated listen port, authenticate against DBBat's user store, then establish an authenticated upstream connection to the target Oracle database.

### TNS Packet Structure

Every TNS message has an 8-byte header:

```
Offset  Size  Field
0       2     Packet length (big-endian, includes header)
2       2     Packet checksum (usually 0x0000)
4       1     Packet type
5       1     Reserved / flags
6       2     Header checksum (usually 0x0000)
8       ...   Payload
```

Packet types we care about:
| Type | Code | Direction | Purpose |
|------|------|-----------|---------|
| Connect | 1 | Client → Server | Initial connection with connect descriptor |
| Accept | 2 | Server → Client | Connection accepted |
| Refuse | 3 | Server → Client | Connection refused (with reason) |
| Redirect | 4 | Server → Client | Redirect to different address |
| Data | 6 | Bidirectional | TTC payload carrier |
| Marker | 5 | Bidirectional | Break/reset signals |
| Resend | 11 | Server → Client | Request packet retransmission |

### Connection Flow

```
┌────────┐                    ┌───────────┐                    ┌──────────────┐
│ Client │                    │   DBBat   │                    │ Oracle DB    │
│(sqlplus)│                    │  (proxy)  │                    │ (upstream)   │
└───┬────┘                    └─────┬─────┘                    └──────┬───────┘
    │                               │                                 │
    │  1. TNS Connect               │                                 │
    │  (service_name, user hint)    │                                 │
    │──────────────────────────────>│                                 │
    │                               │                                 │
    │                               │  Parse connect descriptor:     │
    │                               │  - Extract SERVICE_NAME        │
    │                               │  - Map to DBBat database       │
    │                               │  - Validate database exists    │
    │                               │                                 │
    │  2. TNS Accept                │                                 │
    │  (negotiated params)          │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │  3. TTC: Set Protocol         │                                 │
    │──────────────────────────────>│                                 │
    │  4. TTC: Set Protocol Resp    │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │  5. TTC: Set Data Types       │                                 │
    │──────────────────────────────>│                                 │
    │  6. TTC: Set Data Types Resp  │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │  7. TTC: AUTH Phase 1         │                                 │
    │  (username, terminal, etc.)   │                                 │
    │──────────────────────────────>│                                 │
    │                               │                                 │
    │                               │  DBBat authenticates:          │
    │                               │  - Look up user by username    │
    │                               │  - Check active grant          │
    │                               │  - Check quotas                │
    │                               │                                 │
    │  8. TTC: AUTH Challenge       │                                 │
    │  (session key, salt, iter)    │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │  9. TTC: AUTH Phase 2         │                                 │
    │  (encrypted password proof)   │                                 │
    │──────────────────────────────>│                                 │
    │                               │                                 │
    │                               │  DBBat verifies password       │
    │                               │  against its own store         │
    │                               │                                 │
    │                               │  10. TNS Connect               │
    │                               │  (to upstream Oracle)          │
    │                               │──────────────────────────────>│
    │                               │  11. TNS Accept                │
    │                               │<──────────────────────────────│
    │                               │  12. TTC: Set Protocol         │
    │                               │──────────────────────────────>│
    │                               │  13. TTC: Set Protocol Resp   │
    │                               │<──────────────────────────────│
    │                               │  14. TTC: Set Data Types      │
    │                               │──────────────────────────────>│
    │                               │  15. TTC: Set Data Types Resp │
    │                               │<──────────────────────────────│
    │                               │  16. TTC: AUTH (stored creds) │
    │                               │──────────────────────────────>│
    │                               │  17. TTC: AUTH OK             │
    │                               │<──────────────────────────────│
    │                               │                                 │
    │  18. TTC: AUTH OK             │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │         === Proxy mode: bidirectional relay ===                 │
```

### TNS Connect Descriptor Parsing

The client sends a connect string in the TNS Connect packet payload:

```
(DESCRIPTION=
  (ADDRESS=(PROTOCOL=TCP)(HOST=proxy-host)(PORT=1522))
  (CONNECT_DATA=
    (SERVICE_NAME=ORCL)
    (CID=(PROGRAM=sqlplus)(HOST=client-host)(USER=jdoe))))
```

We need to extract:
- `SERVICE_NAME` → maps to a DBBat `Database.Name`
- `USER` from CID → informational (the real username comes in the TTC AUTH phase)

This is a parenthesized key-value format, not SQL. Write a simple recursive descent parser or use regex:

```go
// Extract SERVICE_NAME from connect descriptor
func parseServiceName(descriptor string) string {
    // Match SERVICE_NAME=value within parentheses
    re := regexp.MustCompile(`(?i)SERVICE_NAME\s*=\s*([^)]+)`)
    m := re.FindStringSubmatch(descriptor)
    if len(m) > 1 {
        return strings.TrimSpace(m[1])
    }
    return ""
}
```

### O5Logon Authentication (Server-Side)

This is the most complex part. Oracle uses a Diffie-Hellman-based challenge-response protocol. For DBBat to terminate auth on the proxy side (rather than passthrough), we must implement the server side of O5Logon.

#### O5Logon Flow (DBBat as server)

**Phase 1 — Client sends username:**
```
TTC AUTH request:
  AUTH_TERMINAL       = "pts/0"
  AUTH_PROGRAM_NM     = "sqlplus"
  AUTH_MACHINE        = "client-host"
  AUTH_PID            = "12345"
  AUTH_SID            = "jdoe"
  Flags: AUTH_SESSKEY  (requesting key exchange)
  Username: "SCOTT"
```

**Phase 2 — DBBat sends challenge:**
```
TTC AUTH response:
  AUTH_SESSKEY             = <96 bytes: server encrypted session key>
  AUTH_VFR_DATA            = <salt from password verifier>
  AUTH_PBKDF2_VGEN_COUNT   = 4096  (PBKDF2 iterations for verifier generation)
  AUTH_PBKDF2_SDER_COUNT   = 3     (second derivation count)
  AUTH_GLOBALLY_UNIQUE_DBID = <database GUID>
```

**Phase 3 — Client sends proof:**
```
TTC AUTH request:
  AUTH_SESSKEY        = <48 bytes: client encrypted session key>
  AUTH_PASSWORD        = <encrypted password proof>
  AUTH_PBKDF2_SPEEDY_KEY = <derived key material>
```

**Phase 4 — DBBat verifies:**
1. Derive the shared session key using DH
2. Decrypt the password proof
3. Verify against DBBat's stored Argon2id hash (or a dedicated Oracle-compatible verifier)

#### Authentication Strategy: Two Options

**Option A: Full O5Logon termination (recommended for production)**

DBBat implements O5Logon server-side. This requires:
- Generating DH key pairs
- Computing password verifiers (SHA-1 + PBKDF2)
- AES-192/256 for session key encryption
- Storing an Oracle-compatible password verifier alongside Argon2id hash

Pros: Full control, same security model as PG proxy
Cons: Complex crypto, version-dependent (11g vs 12c vs 19c have variations)

**Option B: Simplified auth with passthrough fallback (recommended for MVP)**

For the MVP, use a hybrid approach:
1. Parse the TNS Connect to extract `SERVICE_NAME` → look up DBBat database
2. **Forward the TTC session negotiation (Set Protocol, Set Data Types) transparently** to upstream
3. **Intercept the AUTH phase**: extract the username from Phase 1
4. Look up user + grant in DBBat, check quotas
5. If no grant → send TNS Refuse, close connection
6. If grant exists → **let auth continue to upstream Oracle** (passthrough)
7. Upstream Oracle validates the actual password

This means:
- DBBat controls **who can connect to which database** (grants)
- Oracle controls **password validation**
- DBBat doesn't need to implement O5Logon crypto
- DBBat users must have matching credentials in Oracle

```go
// Phase 1 MVP: Auth-aware passthrough
type OracleSession struct {
    clientConn   net.Conn
    upstreamConn net.Conn
    store        *store.Store
    logger       *slog.Logger
    ctx          context.Context

    // Extracted during connection setup
    serviceName  string
    username     string
    database     *store.Database
    user         *store.User
    grant        *store.Grant
    connectionUID uuid.UUID

    // TNS state
    tnsVersion   uint16
    maxPacketSize uint32

    // TTC state (Phase 2+)
    cursorMap    map[uint16]*trackedCursor // cursor ID → SQL + metadata
}
```

### Database Model Changes

The `Database` model needs an Oracle variant:

```go
// Database.Protocol field (new)
const (
    ProtocolPostgreSQL = "postgresql"
    ProtocolOracle     = "oracle"
)
```

```sql
-- Migration: add protocol support
ALTER TABLE databases ADD COLUMN protocol TEXT NOT NULL DEFAULT 'postgresql';
ALTER TABLE databases ADD COLUMN oracle_service_name TEXT;
-- Port default changes: 5432 for PG, 1521 for Oracle
```

The `Database` model gains:
```go
type Database struct {
    // ... existing fields ...
    Protocol          string  `bun:"protocol,notnull,default:'postgresql'" json:"protocol"`
    OracleServiceName *string `bun:"oracle_service_name" json:"oracle_service_name,omitempty"`
}
```

### Server Changes

```go
// New listener alongside the PG one
// Config: DBB_LISTEN_ORA (default: :1522)

type OracleServer struct {
    listener net.Listener
    store    *store.Store
    // ... same deps as PG Server
}

func (s *OracleServer) Start() error {
    ln, err := net.Listen("tcp", s.listenAddr)
    // Accept loop → spawn OracleSession per connection
}
```

### What to Build (Phase 1 Deliverables)

1. **TNS packet reader/writer** — 8-byte header parsing, packet reassembly
2. **Connect descriptor parser** — extract `SERVICE_NAME`
3. **TTC frame reader** — extract function code from TNS Data packets
4. **Auth username extractor** — parse TTC AUTH Phase 1 to get username
5. **OracleSession** — lifecycle: accept → parse connect → check grant → relay auth → proxy
6. **OracleServer** — TCP listener, session spawning
7. **Database model update** — `protocol` + `oracle_service_name` fields
8. **Migration** — schema changes

### Key Reference: go-ora Source Files

| What we need | go-ora file | What to extract |
|-------------|-------------|-----------------|
| TNS packet read/write | `network/session.go` | `readPacket()`, `writePacket()`, packet header struct |
| Connect descriptor | `network/connect_option.go` | Connect string building/parsing |
| TTC session negotiation | `network/session_ctx.go` | Protocol version, data type negotiation |
| O5Logon auth (client side) | `auth_object.go` | DH, session key derivation, password encryption |
| Data type encoding | `converters.go`, `parameter.go` | NUMBER, VARCHAR2, DATE, TIMESTAMP decoding |

We should **vendor the relevant protocol-level code** from go-ora (MIT licensed) rather than importing the full driver. This gives us control and avoids pulling in connection-pool/driver machinery.

---

## Phase 2: Transparent Proxying with Query Interception

### Goal
Once authenticated, relay all TTC messages bidirectionally while intercepting query-related operations for logging and access control.

### TTC Message Structure

Inside every TNS Data packet:

```
Offset  Size  Field
0       2     Data flags (usually 0x0000)
2       1     TTC function code
3       ...   Function-specific payload
```

### TTC Function Codes (Key Operations)

| Code | Name | Direction | Purpose |
|------|------|-----------|---------|
| 0x01 | OSETPRO | Bidirectional | Set Protocol (session init) |
| 0x02 | ODTYPES | Bidirectional | Set Data Types (session init) |
| 0x03 | OOPEN | C→S | Open cursor |
| 0x05 | OCLOSE | C→S | Close cursor |
| 0x0E | OALL8 | C→S | Parse + Execute (combined, most common) |
| 0x11 | OFETCH | C→S | Fetch rows |
| 0x14 | OCANCEL | C→S | Cancel operation |
| 0x47 | OSQL7 | C→S | Describe statement |
| 0x5E | OAUTH | Bidirectional | Authentication messages |
| 0x08 | Response | S→C | Function response (rows, errors, etc.) |
| 0x09 | OMARKER | Bidirectional | Break/reset marker |
| 0x0B | OVERSION | C→S | Version info |
| 0x0C | OSTATUS | C→S | Server status |
| 0x44 | OLOBOPS | Bidirectional | LOB operations |
| 0x73 | OSESSKEY | Bidirectional | Session key exchange |

### Message Relay Architecture

```
Client ←──TNS──→ DBBat Proxy ←──TNS──→ Oracle Upstream

         ┌─────────────────────────────────────────┐
         │           OracleSession                  │
         │                                          │
         │  goroutine 1: clientToUpstream()         │
         │  ├─ readTNSPacket(clientConn)            │
         │  ├─ if Data packet:                      │
         │  │   ├─ parseTTCHeader()                 │
         │  │   ├─ switch functionCode:             │
         │  │   │   case OALL8: interceptParse()    │
         │  │   │   case OFETCH: interceptFetch()   │
         │  │   │   case OCLOSE: cleanupCursor()    │
         │  │   │   default: pass through           │
         │  │   └─ forward packet                   │
         │  └─ else: forward packet                 │
         │                                          │
         │  goroutine 2: upstreamToClient()         │
         │  ├─ readTNSPacket(upstreamConn)          │
         │  ├─ if Data packet:                      │
         │  │   ├─ parseTTCHeader()                 │
         │  │   ├─ if response to tracked query:    │
         │  │   │   ├─ captureResultMetadata()      │
         │  │   │   ├─ captureRowData()             │
         │  │   │   └─ if complete: logQuery()      │
         │  │   └─ forward packet                   │
         │  └─ else: forward packet                 │
         └─────────────────────────────────────────┘
```

### Cursor-Based State Tracking

This is the fundamental difference from PostgreSQL. In PG, every `Query` message contains the SQL text. In Oracle, SQL is only present in the `OALL8` (parse) step. Subsequent `OFETCH` operations reference a **cursor ID** — an opaque integer.

```go
type trackedCursor struct {
    cursorID    uint16
    sql         string
    bindValues  []string       // Decoded bind parameters
    parsedAt    time.Time
    columnNames []string       // Populated from describe/first-fetch response
    columnTypes []oracleType   // Oracle type codes (NUMBER=2, VARCHAR2=1, DATE=12, etc.)
}

type oracleQueryTracker struct {
    cursors       map[uint16]*trackedCursor   // Active cursors
    pendingQuery  *pendingOracleQuery          // Currently executing
}

type pendingOracleQuery struct {
    cursor        *trackedCursor
    startTime     time.Time
    capturedRows  []store.QueryRow
    capturedBytes int64
    rowNumber     int
    truncated     bool
}
```

### Intercepting OALL8 (Parse + Execute)

OALL8 is the primary query message. Its binary layout (simplified):

```
[TTC header: function code 0x0E]
[options: uint32]
[cursor ID: uint16]
[SQL length: varies]
[SQL text: UTF-8 bytes]         ← THIS IS WHAT WE WANT
[bind count: uint16]
[bind definitions...]
[bind values...]                ← AND THESE
[define count: uint16]
[column definitions...]
```

The exact byte offsets depend on the TTC version negotiated during session setup. go-ora's `command.go` → `execute()` method shows the encoding.

```go
func (s *OracleSession) interceptOALL8(payload []byte) error {
    // 1. Parse OALL8 to extract SQL text and bind values
    sql, binds, cursorID, err := s.decodeTTCExecute(payload)
    if err != nil {
        // Can't decode → log warning, pass through (don't block)
        s.logger.Warn("failed to decode OALL8", "error", err)
        return nil
    }

    // 2. Track cursor → SQL mapping
    s.tracker.cursors[cursorID] = &trackedCursor{
        cursorID:   cursorID,
        sql:        sql,
        bindValues: binds,
        parsedAt:   time.Now(),
    }

    // 3. Access control checks (same logic as PG proxy)
    if err := s.validateQuery(sql); err != nil {
        s.sendOracleError(err) // TTC error response
        return err
    }

    // 4. Start timing
    s.tracker.pendingQuery = &pendingOracleQuery{
        cursor:    s.tracker.cursors[cursorID],
        startTime: time.Now(),
    }

    return nil
}
```

### Access Control (Reusable from PG)

The SQL validation logic is largely protocol-agnostic. Factor out from `intercept.go`:

```go
// internal/proxy/shared/validation.go (extracted from PG proxy)
package shared

func ValidateQuery(sql string, grant *store.Grant) error {
    normalized := strings.ToUpper(strings.TrimSpace(sql))

    // Block password changes
    if containsPasswordChange(normalized) {
        return ErrPasswordChangeBlocked
    }

    // Read-only enforcement
    if grant.IsReadOnly() && isWriteQuery(normalized) {
        return ErrReadOnlyViolation
    }

    // DDL blocking
    if grant.ShouldBlockDDL() && isDDLQuery(normalized) {
        return ErrDDLBlocked
    }

    return nil
}
```

Oracle-specific additions:
- Block `ALTER SYSTEM` / `ALTER SESSION SET "_allow_level_security_..."` etc.
- Block `CREATE DATABASE LINK` (network escape)
- Block PL/SQL `EXECUTE IMMEDIATE` with dynamic SQL containing blocked operations (best-effort)
- Block `DBMS_SCHEDULER` / `DBMS_JOB` (async code execution)

```go
// Oracle-specific blocked patterns
var oracleBlockedPatterns = []string{
    `ALTER\s+SYSTEM`,
    `CREATE\s+DATABASE\s+LINK`,
    `DBMS_SCHEDULER`,
    `DBMS_JOB`,
    `UTL_HTTP`,      // Network access from DB
    `UTL_TCP`,
    `UTL_FILE`,      // File system access
    `DBMS_PIPE`,     // IPC escape
}
```

### Sending Oracle Error Responses

When we block a query, we need to send back a TTC error that the Oracle client understands:

```go
func (s *OracleSession) sendOracleError(queryErr error) error {
    // Oracle error format in TTC:
    // - Function code: 0x08 (response)
    // - Error flag set
    // - ORA error number (use ORA-01031: insufficient privileges)
    // - Error message text
    return s.writeTTCError(1031, queryErr.Error())
}
```

### What to Build (Phase 2 Deliverables)

1. **TTC function code parser** — read function code from Data packet payload
2. **OALL8 decoder** — extract SQL text, bind parameters, cursor ID
3. **Cursor state tracker** — map cursor IDs to SQL across the session
4. **Query validator** — factor out from PG, add Oracle-specific patterns
5. **TTC error encoder** — send ORA-style errors to client
6. **Bidirectional relay** — two goroutines with interception hooks
7. **OFETCH interception** — link fetch operations to their cursor's SQL

---

## Phase 3: Result Capture & Storage

### Goal
Capture query results from Oracle responses, decode Oracle data types, and store them using the existing `Query`/`QueryRow` model.

### Oracle Response Structure

When Oracle returns data, it uses TTC response packets (function code 0x08). The response to an OALL8 or OFETCH contains:

```
[TTC header: function code 0x08]
[return code: uint16]           ← 0 = success
[row count: uint32]
[column definitions (first response only):]
  [column count: uint16]
  [for each column:]
    [name length + name]
    [data type: uint8]          ← Oracle type code
    [max size: uint32]
    [precision: uint8]
    [scale: uint8]
    [nullable: uint8]
[row data:]
  [for each row:]
    [for each column:]
      [value length prefix]
      [value bytes]             ← Encoded per type
[more-data flag: bool]          ← If true, client must OFETCH for more
```

### Oracle Data Type Decoding

| Oracle Type Code | Type Name | Go Decode Strategy |
|------------------|-----------|--------------------|
| 1 | VARCHAR2 | Direct UTF-8 string |
| 2 | NUMBER | Oracle NUMBER format → `big.Float` → string |
| 12 | DATE | 7-byte Oracle date → `time.Time` |
| 23 | RAW | Hex-encode bytes |
| 96 | CHAR | UTF-8 string, trim trailing spaces |
| 100 | BINARY_FLOAT | IEEE 754 float32 |
| 101 | BINARY_DOUBLE | IEEE 754 float64 |
| 112 | CLOB | LOB locator → read via LOB operations (or inline if small) |
| 113 | BLOB | LOB locator → hex-encode or skip |
| 180 | TIMESTAMP | Extended Oracle timestamp format |
| 181 | TIMESTAMP WITH TIME ZONE | Timestamp + TZ offset |
| 231 | TIMESTAMP WITH LOCAL TIME ZONE | Timestamp in session TZ |

Oracle's NUMBER format is particularly tricky — it's a variable-length format with a sign byte, exponent byte, and mantissa digits in base-100. go-ora's `converters.go` has a working decoder.

```go
func decodeOracleValue(typeCode uint8, data []byte) (interface{}, error) {
    switch typeCode {
    case 1, 96: // VARCHAR2, CHAR
        return string(data), nil
    case 2: // NUMBER
        return decodeOracleNumber(data)  // Port from go-ora
    case 12: // DATE
        return decodeOracleDate(data)    // 7-byte format
    case 23: // RAW
        return hex.EncodeToString(data), nil
    case 100: // BINARY_FLOAT
        return math.Float32frombits(binary.BigEndian.Uint32(data)), nil
    case 101: // BINARY_DOUBLE
        return math.Float64frombits(binary.BigEndian.Uint64(data)), nil
    case 180, 181, 231: // TIMESTAMP variants
        return decodeOracleTimestamp(data)
    case 112, 113: // CLOB, BLOB
        return "[LOB]", nil // LOBs need special handling via LOB ops
    default:
        return base64.StdEncoding.EncodeToString(data), nil
    }
}
```

### Row Capture Flow

```
   OALL8 (SQL + binds)                OFETCH (cursor ID)
        │                                    │
        ▼                                    ▼
   Track cursor                        Look up cursor
   Start timing                        Continue timing
        │                                    │
        ▼                                    ▼
   ┌─────────── Response from Oracle ─────────────┐
   │                                               │
   │  First response → column definitions          │
   │  ├─ Extract column names + types              │
   │  ├─ Store in trackedCursor                    │
   │  └─ Capture rows (if any inline)              │
   │                                               │
   │  Row data → decode per column type            │
   │  ├─ Build JSON row (same as PG: {col: val})   │
   │  ├─ Check limits (MaxResultRows, MaxBytes)    │
   │  └─ Append to capturedRows                    │
   │                                               │
   │  More-data flag = false → query complete      │
   │  ├─ Calculate duration                        │
   │  └─ logQuery() → store.CreateQuery()          │
   └───────────────────────────────────────────────┘
```

### Storing Oracle Queries

The existing `Query` and `QueryRow` models work as-is:

```go
// Same logQuery pattern as PG proxy
func (s *OracleSession) logQuery(cursor *trackedCursor, pending *pendingOracleQuery,
    rowsAffected *int64, queryError *string, bytesTransferred int64) {

    duration := time.Since(pending.startTime).Seconds() * 1000

    query := &store.Query{
        UID:          uuid.Must(uuid.NewV7()),
        ConnectionID: s.connectionUID,
        SQLText:      cursor.sql,
        Parameters:   formatOracleBinds(cursor.bindValues),
        ExecutedAt:   pending.startTime,
        DurationMs:   &duration,
        RowsAffected: rowsAffected,
        Error:        queryError,
    }

    go func() {
        if err := s.store.CreateQuery(s.ctx, query); err != nil {
            s.logger.Error("failed to log query", "error", err)
            return
        }
        if len(pending.capturedRows) > 0 {
            if err := s.store.StoreQueryRows(s.ctx, query.UID, pending.capturedRows); err != nil {
                s.logger.Error("failed to store query rows", "error", err)
            }
        }
    }()
}
```

### LOB Handling

Large Objects (CLOB/BLOB) in Oracle use a locator pattern: the row data contains a LOB locator (pointer), and the client issues separate `OLOBOPS` (0x44) to read the actual content. For the MVP:

- **Don't capture LOB content** — store `"[CLOB]"` or `"[BLOB]"` placeholder
- Track LOB reads as separate operations in the logs
- Future: optionally capture small LOBs (< configurable threshold)

### PL/SQL Blocks

PL/SQL anonymous blocks (`BEGIN ... END;`) and stored procedure calls (`CALL pkg.proc(...)`) are sent as SQL text in OALL8. They may:
- Return OUT/INOUT parameters (not result sets)
- Execute multiple SQL statements internally (not visible to the proxy)
- Use `DBMS_OUTPUT` for text output

For the MVP:
- Log the PL/SQL block text as the query SQL
- Capture OUT parameter values as "rows" (single row with parameter names as columns)
- Don't attempt to track internal SQL within PL/SQL (Oracle audit handles that)

### What to Build (Phase 3 Deliverables)

1. **TTC response parser** — extract column definitions, row data, return codes
2. **Oracle type decoders** — NUMBER, DATE, TIMESTAMP, VARCHAR2 (port from go-ora)
3. **Row capture with limits** — same MaxResultRows/MaxResultBytes as PG
4. **Query logging integration** — async write to `store.CreateQuery` / `store.StoreQueryRows`
5. **Connection stats tracking** — bytes transferred, query count (same as PG)
6. **LOB placeholder handling** — detect LOB locators, store placeholders

---

## Package Structure

```
internal/
├── proxy/
│   ├── pg/                       # Rename current proxy code
│   │   ├── server.go
│   │   ├── session.go
│   │   ├── auth.go
│   │   ├── intercept.go
│   │   └── upstream.go
│   ├── oracle/                   # New Oracle proxy
│   │   ├── server.go             # TCP listener on :1522
│   │   ├── session.go            # Session lifecycle
│   │   ├── auth.go               # Auth interception / passthrough
│   │   ├── intercept.go          # Query interception + access control
│   │   ├── tns.go                # TNS packet reader/writer
│   │   ├── ttc.go                # TTC frame parser
│   │   ├── ttc_decode.go         # OALL8, OFETCH, response decoders
│   │   ├── types.go              # Oracle data type decoders
│   │   └── connect_descriptor.go # Connect string parser
│   └── shared/                   # Extracted common logic
│       ├── validation.go         # SQL validation (from PG intercept.go)
│       └── query_capture.go      # Row capture limits, truncation logic
```

## Risks & Open Questions

### High Risk
1. **TTC version fragmentation** — Oracle 18c, 19c, 21c, and 23ai each have TTC variations. Bind formats, response layouts, and function codes can differ. We mitigate this by testing against multiple versions (see "Container Strategy" below). 18c and 19c share the same TTC protocol version, so `gvenzl/oracle-xe:18.4.0-slim` is a valid stand-in for 19c in CI where Oracle SSO auth isn't available.
2. **Encrypted TNS (Oracle Native Encryption / TLS)** — If the client or server uses Oracle Native Network Encryption (not TLS), the TTC payload is encrypted and we cannot intercept. We'd need to either terminate encryption or reject encrypted connections. TLS is more tractable (terminate TLS at proxy, re-establish to upstream).

### Medium Risk
3. **OALL8 decode accuracy** — The binary format is not officially documented. go-ora's implementation is the reference but may have edge cases. We should have extensive packet capture tests.
4. **Multi-packet TTC messages** — Large SQL or large bind values may span multiple TNS Data packets. The TTC frame reassembly logic must handle this correctly.
5. **Connection pooling clients** — JDBC thin drivers and connection pools (UCP, HikariCP) may send multiplexed or pipelined operations. Need to verify our cursor tracking handles this.

### Low Risk / Open Questions
6. **Should we support Oracle Thick Client (OCI)?** — OCI uses the same TNS/TTC protocol, so yes by default. But OCI has additional features (array binds, scrollable cursors) that may need handling.
7. **Should `ProtocolOracle` databases share the same `Database` table or get a separate table?** — Recommendation: same table with `protocol` column. This keeps grants/connections/queries unified.
8. **Do we need `COPY`-equivalent handling?** — Oracle's equivalent is SQL*Loader and external tables. These don't go through the TTC protocol, so no proxy-level handling needed. Data Pump (`expdp`/`impdp`) uses a proprietary protocol — out of scope.
9. **Oracle RAC / Service failover** — If the upstream is RAC, the proxy should handle TNS Redirect messages (type 4) to follow the connection to the right instance.

## Estimated Scope

| Phase | New Go Code | Ported from go-ora | Refactored from PG proxy | Effort |
|-------|-------------|--------------------|--------------------------|----|
| Phase 1: Connection & Auth | ~600 lines | ~400 lines (TNS, connect) | ~100 lines | Medium-High |
| Phase 2: Query Interception | ~800 lines | ~500 lines (TTC decode) | ~200 lines (validation) | High |
| Phase 3: Result Capture | ~500 lines | ~300 lines (type decoders) | ~150 lines (capture logic) | Medium |
| **Total** | **~1,900 lines** | **~1,200 lines** | **~450 lines** | |

For reference, the current PG proxy is ~1,800 lines. The Oracle proxy is expected to be ~3,500 lines total (new + ported + refactored), owing to the more complex protocol.

## Suggested Development Order

### Step 1: TNS Packet Reader/Writer

Build the low-level TNS packet framing layer: read 8-byte headers, extract packet type and length, read full payload, write packets back.

**Files:** `oracle/tns.go`, `oracle/tns_test.go`

**Tests:**

```go
// tns_test.go

// --- Unit tests (pure bytes, no network) ---

func TestTNSPacket_ParseHeader(t *testing.T) {
    // Valid 8-byte header with known type and length
    raw := []byte{0x00, 0x2A, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00} // len=42, type=Data(6)
    pkt, err := parseTNSHeader(raw)
    require.NoError(t, err)
    assert.Equal(t, uint16(42), pkt.Length)
    assert.Equal(t, TNSPacketTypeData, pkt.Type)
}

func TestTNSPacket_ParseHeader_TooShort(t *testing.T) {
    // Less than 8 bytes → error
    _, err := parseTNSHeader([]byte{0x00, 0x0A})
    assert.ErrorIs(t, err, ErrTNSHeaderTooShort)
}

func TestTNSPacket_ParseHeader_AllTypes(t *testing.T) {
    // Verify all known packet types parse correctly
    for _, tt := range []struct {
        code byte
        want TNSPacketType
    }{
        {0x01, TNSPacketTypeConnect},
        {0x02, TNSPacketTypeAccept},
        {0x03, TNSPacketTypeRefuse},
        {0x04, TNSPacketTypeRedirect},
        {0x05, TNSPacketTypeMarker},
        {0x06, TNSPacketTypeData},
        {0x0B, TNSPacketTypeResend},
        {0x0C, TNSPacketTypeControl},
    } {
        raw := []byte{0x00, 0x08, 0x00, 0x00, tt.code, 0x00, 0x00, 0x00}
        pkt, err := parseTNSHeader(raw)
        require.NoError(t, err)
        assert.Equal(t, tt.want, pkt.Type)
    }
}

func TestTNSPacket_ParseHeader_UnknownType(t *testing.T) {
    // Unknown type code → parsed but flagged (don't reject, we may need to relay it)
    raw := []byte{0x00, 0x08, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00}
    pkt, err := parseTNSHeader(raw)
    require.NoError(t, err)
    assert.Equal(t, TNSPacketType(0xFF), pkt.Type)
}

func TestTNSPacket_Encode(t *testing.T) {
    // Encode a Data packet, verify header bytes
    payload := []byte("hello")
    encoded := encodeTNSPacket(TNSPacketTypeData, payload)
    assert.Equal(t, 8+len(payload), len(encoded))
    // Length field (big-endian) = 13
    assert.Equal(t, byte(0x00), encoded[0])
    assert.Equal(t, byte(0x0D), encoded[1])
    // Type = Data (6)
    assert.Equal(t, byte(0x06), encoded[4])
    // Payload intact
    assert.Equal(t, payload, encoded[8:])
}

func TestTNSPacket_RoundTrip(t *testing.T) {
    // Encode then parse → same values
    original := TNSPacket{Type: TNSPacketTypeConnect, Payload: []byte("test-connect-data")}
    encoded := encodeTNSPacket(original.Type, original.Payload)
    parsed, err := parseTNSHeader(encoded[:8])
    require.NoError(t, err)
    assert.Equal(t, original.Type, parsed.Type)
    assert.Equal(t, uint16(len(encoded)), parsed.Length)
}

func TestTNSPacket_ZeroLengthPayload(t *testing.T) {
    // Header-only packet (e.g., Resend has no payload)
    encoded := encodeTNSPacket(TNSPacketTypeResend, nil)
    assert.Equal(t, 8, len(encoded))
}

func TestTNSPacket_MaxLength(t *testing.T) {
    // TNS max packet size is typically 32767 bytes (SDU)
    // Verify we handle the max correctly
    payload := make([]byte, 32767-8)
    encoded := encodeTNSPacket(TNSPacketTypeData, payload)
    parsed, err := parseTNSHeader(encoded[:8])
    require.NoError(t, err)
    assert.Equal(t, uint16(32767), parsed.Length)
}

// --- Integration tests (net.Pipe, simulates real I/O) ---

func TestTNSPacket_ReadFromConn(t *testing.T) {
    // Write a packet to one end of net.Pipe, read from the other
    client, server := net.Pipe()
    defer client.Close()
    defer server.Close()

    go func() {
        payload := []byte("connect-data")
        raw := encodeTNSPacket(TNSPacketTypeConnect, payload)
        client.Write(raw)
    }()

    pkt, err := readTNSPacket(server)
    require.NoError(t, err)
    assert.Equal(t, TNSPacketTypeConnect, pkt.Type)
    assert.Equal(t, []byte("connect-data"), pkt.Payload)
}

func TestTNSPacket_ReadFromConn_PartialHeader(t *testing.T) {
    // Send header bytes one at a time → should still reassemble
    client, server := net.Pipe()
    defer client.Close()
    defer server.Close()

    go func() {
        raw := encodeTNSPacket(TNSPacketTypeData, []byte("payload"))
        for i := range raw {
            client.Write(raw[i : i+1])
            time.Sleep(time.Millisecond)
        }
    }()

    pkt, err := readTNSPacket(server)
    require.NoError(t, err)
    assert.Equal(t, []byte("payload"), pkt.Payload)
}

func TestTNSPacket_ReadFromConn_EOF(t *testing.T) {
    // Connection closed mid-read → io.EOF or io.ErrUnexpectedEOF
    client, server := net.Pipe()
    go func() {
        client.Write([]byte{0x00, 0x20}) // partial header, then close
        client.Close()
    }()

    _, err := readTNSPacket(server)
    assert.Error(t, err)
}

func TestTNSPacket_MultiplePackets(t *testing.T) {
    // Write 3 packets back to back, read them sequentially
    client, server := net.Pipe()
    defer client.Close()
    defer server.Close()

    types := []TNSPacketType{TNSPacketTypeConnect, TNSPacketTypeData, TNSPacketTypeMarker}
    go func() {
        for _, typ := range types {
            raw := encodeTNSPacket(typ, []byte(fmt.Sprintf("pkt-%d", typ)))
            client.Write(raw)
        }
    }()

    for _, expected := range types {
        pkt, err := readTNSPacket(server)
        require.NoError(t, err)
        assert.Equal(t, expected, pkt.Type)
    }
}

// --- Capture-based tests (real Oracle traffic) ---

func TestTNSPacket_ParseRealCapture_SQLPlusConnect(t *testing.T) {
    // Load a tcpdump capture of a real sqlplus connection
    // tcpdump -w captures/sqlplus_connect.bin -s0 port 1521
    raw := loadCapture(t, "testdata/captures/sqlplus_connect.bin")
    pkt, err := parseTNSHeader(raw[:8])
    require.NoError(t, err)
    assert.Equal(t, TNSPacketTypeConnect, pkt.Type)
    assert.True(t, pkt.Length > 8, "connect packet should have payload")
}
```

### Step 2: Connect Descriptor Parser

Parse Oracle's parenthesized connect descriptor format to extract `SERVICE_NAME`, `SID`, and other connection metadata.

**Files:** `oracle/connect_descriptor.go`, `oracle/connect_descriptor_test.go`

**Tests:**

```go
// connect_descriptor_test.go

func TestParseServiceName_Standard(t *testing.T) {
    desc := `(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db.example.com)(PORT=1521))(CONNECT_DATA=(SERVICE_NAME=ORCL)))`
    assert.Equal(t, "ORCL", parseServiceName(desc))
}

func TestParseServiceName_CaseInsensitive(t *testing.T) {
    desc := `(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db)(PORT=1521))(CONNECT_DATA=(service_name=mydb)))`
    assert.Equal(t, "mydb", parseServiceName(desc))
}

func TestParseServiceName_WithSpaces(t *testing.T) {
    desc := `(DESCRIPTION = (CONNECT_DATA = (SERVICE_NAME = PROD_DB )))`
    assert.Equal(t, "PROD_DB", parseServiceName(desc))
}

func TestParseServiceName_MissingServiceName(t *testing.T) {
    desc := `(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db)(PORT=1521))(CONNECT_DATA=(SID=ORCL)))`
    assert.Equal(t, "", parseServiceName(desc))
}

func TestParseSID_Fallback(t *testing.T) {
    desc := `(DESCRIPTION=(CONNECT_DATA=(SID=MYDB)))`
    assert.Equal(t, "", parseServiceName(desc))
    assert.Equal(t, "MYDB", parseSID(desc))
}

func TestParseServiceName_MultipleAddresses(t *testing.T) {
    // RAC connect descriptor with multiple addresses
    desc := `(DESCRIPTION=
        (ADDRESS_LIST=
            (ADDRESS=(PROTOCOL=TCP)(HOST=rac1)(PORT=1521))
            (ADDRESS=(PROTOCOL=TCP)(HOST=rac2)(PORT=1521)))
        (CONNECT_DATA=(SERVICE_NAME=RAC_SVC)(FAILOVER_MODE=(TYPE=SELECT)(METHOD=BASIC))))`
    assert.Equal(t, "RAC_SVC", parseServiceName(desc))
}

func TestParseServiceName_EZConnect(t *testing.T) {
    // EZ Connect format: host:port/service_name
    // Some clients send this instead of full descriptor
    desc := "db.example.com:1521/ORCL"
    assert.Equal(t, "ORCL", parseServiceNameEZConnect(desc))
}

func TestParseServiceName_EZConnect_NoPort(t *testing.T) {
    desc := "db.example.com/ORCL"
    assert.Equal(t, "ORCL", parseServiceNameEZConnect(desc))
}

func TestParseConnectDescriptor_Full(t *testing.T) {
    desc := `(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db.prod)(PORT=1521))(CONNECT_DATA=(SERVICE_NAME=FINDB)(CID=(PROGRAM=sqlplus)(HOST=workstation)(USER=jdoe))))`
    cd := parseConnectDescriptor(desc)
    assert.Equal(t, "FINDB", cd.ServiceName)
    assert.Equal(t, "db.prod", cd.Host)
    assert.Equal(t, 1521, cd.Port)
    assert.Equal(t, "sqlplus", cd.Program)
    assert.Equal(t, "jdoe", cd.OSUser)
}

func TestParseConnectDescriptor_Empty(t *testing.T) {
    cd := parseConnectDescriptor("")
    assert.Equal(t, "", cd.ServiceName)
}

func TestParseConnectDescriptor_MalformedParens(t *testing.T) {
    // Unbalanced parens → extract what we can without crashing
    desc := `(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=OK)`
    cd := parseConnectDescriptor(desc)
    assert.Equal(t, "OK", cd.ServiceName)
}

func TestParseConnectDescriptor_RealCapture(t *testing.T) {
    // Extract connect descriptor from a real TNS Connect packet payload
    raw := loadCapture(t, "testdata/captures/sqlplus_connect.bin")
    pkt, _ := readTNSPacketFromBytes(raw)
    desc := extractConnectDescriptor(pkt.Payload)
    assert.NotEmpty(t, desc.ServiceName)
}
```

### Step 3: OracleServer + OracleSession Skeleton

TCP listener that accepts connections, parses the TNS Connect, looks up the database, connects to upstream, and relays raw bytes. No interception yet — pure TCP relay.

**Files:** `oracle/server.go`, `oracle/session.go`, `oracle/server_test.go`, `oracle/session_test.go`

**Tests:**

```go
// server_test.go

func TestOracleServer_StartsAndAcceptsConnections(t *testing.T) {
    srv := NewOracleServer(":0", nil, nil, slog.Default()) // :0 = random port
    go srv.Start()
    defer srv.Stop()

    // Connect a raw TCP client
    conn, err := net.Dial("tcp", srv.Addr().String())
    require.NoError(t, err)
    defer conn.Close()
}

func TestOracleServer_GracefulShutdown(t *testing.T) {
    srv := NewOracleServer(":0", nil, nil, slog.Default())
    go srv.Start()

    // Connect a client
    conn, err := net.Dial("tcp", srv.Addr().String())
    require.NoError(t, err)

    // Shutdown should not panic
    srv.Stop()

    // Existing connection should eventually get closed
    conn.SetReadDeadline(time.Now().Add(time.Second))
    _, err = conn.Read(make([]byte, 1))
    assert.Error(t, err) // EOF or deadline exceeded
}

func TestOracleServer_ConcurrentConnections(t *testing.T) {
    srv := NewOracleServer(":0", nil, nil, slog.Default())
    go srv.Start()
    defer srv.Stop()

    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            conn, err := net.Dial("tcp", srv.Addr().String())
            if err == nil {
                conn.Close()
            }
        }()
    }
    wg.Wait()
}

// session_test.go

func TestOracleSession_RawRelay(t *testing.T) {
    // Set up: fake upstream Oracle (echo server), proxy session, fake client
    upstream := newEchoTNSServer(t)
    defer upstream.Close()

    client, proxyEnd := net.Pipe()
    defer client.Close()
    defer proxyEnd.Close()

    session := &OracleSession{
        clientConn: proxyEnd,
        // Point to fake upstream
    }
    session.upstreamConn, _ = net.Dial("tcp", upstream.Addr().String())

    go session.relayRaw()

    // Send a TNS Data packet from client side
    pkt := encodeTNSPacket(TNSPacketTypeData, []byte("hello oracle"))
    client.Write(pkt)

    // Should get it echoed back through upstream
    resp, err := readTNSPacket(client)
    require.NoError(t, err)
    assert.Equal(t, []byte("hello oracle"), resp.Payload)
}

func TestOracleSession_ConnectParsesServiceName(t *testing.T) {
    // Simulate a client sending TNS Connect with SERVICE_NAME
    client, proxyEnd := net.Pipe()
    defer client.Close()

    session := &OracleSession{clientConn: proxyEnd}

    connectPayload := buildTNSConnect("TESTDB")
    go func() {
        client.Write(encodeTNSPacket(TNSPacketTypeConnect, connectPayload))
    }()

    serviceName, err := session.receiveConnect()
    require.NoError(t, err)
    assert.Equal(t, "TESTDB", serviceName)
}

func TestOracleSession_ConnectUnknownDB_SendsRefuse(t *testing.T) {
    // Client sends SERVICE_NAME that doesn't exist in store → TNS Refuse
    client, proxyEnd := net.Pipe()
    defer client.Close()

    mockStore := newMockStore() // No databases configured
    session := &OracleSession{clientConn: proxyEnd, store: mockStore}

    go func() {
        client.Write(encodeTNSPacket(TNSPacketTypeConnect, buildTNSConnect("UNKNOWN_DB")))
    }()

    go session.Run()

    // Client should receive TNS Refuse
    pkt, err := readTNSPacket(client)
    require.NoError(t, err)
    assert.Equal(t, TNSPacketTypeRefuse, pkt.Type)
}
```

### Step 4: Auth Passthrough

Intercept the TTC AUTH messages to extract the username, check DBBat grants, then relay the auth exchange to the upstream Oracle for actual password validation.

**Files:** `oracle/auth.go`, `oracle/auth_test.go`

**Tests:**

```go
// auth_test.go

func TestExtractUsername_FromTTCAuth(t *testing.T) {
    // Build a TTC AUTH Phase 1 payload with known username
    payload := buildTTCAuthPhase1("SCOTT")
    username, err := extractUsernameFromAuth(payload)
    require.NoError(t, err)
    assert.Equal(t, "SCOTT", username)
}

func TestExtractUsername_EmptyUsername(t *testing.T) {
    payload := buildTTCAuthPhase1("")
    _, err := extractUsernameFromAuth(payload)
    assert.ErrorIs(t, err, ErrEmptyUsername)
}

func TestExtractUsername_UnicodeUsername(t *testing.T) {
    payload := buildTTCAuthPhase1("用户")
    username, err := extractUsernameFromAuth(payload)
    require.NoError(t, err)
    assert.Equal(t, "用户", username)
}

func TestExtractUsername_RealCapture(t *testing.T) {
    // Parse a captured TTC AUTH Phase 1 from a real sqlplus session
    raw := loadCapture(t, "testdata/captures/auth_phase1.bin")
    username, err := extractUsernameFromAuth(raw)
    require.NoError(t, err)
    assert.NotEmpty(t, username)
}

func TestAuthPassthrough_GrantExists_RelaysToUpstream(t *testing.T) {
    // Full auth flow with mock store (user + database + grant exist)
    upstream := newFakeOracleAuth(t, "SCOTT", "tiger") // Accepts SCOTT/tiger
    defer upstream.Close()

    client, proxyEnd := net.Pipe()
    defer client.Close()

    mockStore := newMockStore()
    mockStore.AddUser("SCOTT", "connector")
    mockStore.AddDatabase("ORCL", "oracle", upstream.Addr())
    mockStore.AddGrant("SCOTT", "ORCL")

    session := &OracleSession{
        clientConn: proxyEnd,
        store:      mockStore,
        database:   mockStore.databases["ORCL"],
    }
    session.upstreamConn, _ = net.Dial("tcp", upstream.Addr().String())

    // Simulate client sending AUTH Phase 1
    go func() {
        client.Write(wrapInTNSData(buildTTCAuthPhase1("SCOTT")))
        // Read challenge from proxy (relayed from upstream)
        readTNSPacket(client)
        // Send Phase 2 (password proof)
        client.Write(wrapInTNSData(buildTTCAuthPhase2("tiger")))
    }()

    err := session.handleAuth()
    require.NoError(t, err)
    assert.Equal(t, "SCOTT", session.user.Username)
    assert.NotNil(t, session.grant)
}

func TestAuthPassthrough_NoGrant_Refused(t *testing.T) {
    client, proxyEnd := net.Pipe()
    defer client.Close()

    mockStore := newMockStore()
    mockStore.AddUser("SCOTT", "connector")
    mockStore.AddDatabase("ORCL", "oracle", "localhost:1521")
    // No grant for SCOTT on ORCL

    session := &OracleSession{
        clientConn:  proxyEnd,
        store:       mockStore,
        serviceName: "ORCL",
    }

    go func() {
        client.Write(wrapInTNSData(buildTTCAuthPhase1("SCOTT")))
    }()

    err := session.handleAuth()
    assert.ErrorIs(t, err, ErrNoActiveGrant)

    // Client should receive TNS Refuse or TTC error
    pkt, _ := readTNSPacket(client)
    assert.Equal(t, TNSPacketTypeRefuse, pkt.Type)
}

func TestAuthPassthrough_UnknownUser_Refused(t *testing.T) {
    client, proxyEnd := net.Pipe()
    defer client.Close()

    mockStore := newMockStore()
    mockStore.AddDatabase("ORCL", "oracle", "localhost:1521")
    // User "HACKER" doesn't exist

    session := &OracleSession{
        clientConn:  proxyEnd,
        store:       mockStore,
        serviceName: "ORCL",
    }

    go func() {
        client.Write(wrapInTNSData(buildTTCAuthPhase1("HACKER")))
    }()

    err := session.handleAuth()
    assert.Error(t, err)
}

func TestAuthPassthrough_QuotaExceeded_Refused(t *testing.T) {
    client, proxyEnd := net.Pipe()
    defer client.Close()

    mockStore := newMockStore()
    mockStore.AddUser("SCOTT", "connector")
    mockStore.AddDatabase("ORCL", "oracle", "localhost:1521")
    mockStore.AddGrantWithQuota("SCOTT", "ORCL", 100, 100) // Already at quota

    session := &OracleSession{
        clientConn:  proxyEnd,
        store:       mockStore,
        serviceName: "ORCL",
    }

    go func() {
        client.Write(wrapInTNSData(buildTTCAuthPhase1("SCOTT")))
    }()

    err := session.handleAuth()
    assert.ErrorIs(t, err, ErrQueryLimitExceeded)
}

func TestAuthPassthrough_UpstreamRejectsPassword(t *testing.T) {
    // Upstream Oracle rejects the password → client gets the rejection
    upstream := newFakeOracleAuth(t, "SCOTT", "correct_password")
    defer upstream.Close()

    client, proxyEnd := net.Pipe()
    defer client.Close()

    mockStore := newMockStore()
    mockStore.AddUser("SCOTT", "connector")
    mockStore.AddDatabase("ORCL", "oracle", upstream.Addr())
    mockStore.AddGrant("SCOTT", "ORCL")

    session := &OracleSession{
        clientConn:  proxyEnd,
        store:       mockStore,
        serviceName: "ORCL",
    }
    session.upstreamConn, _ = net.Dial("tcp", upstream.Addr().String())

    go func() {
        client.Write(wrapInTNSData(buildTTCAuthPhase1("SCOTT")))
        readTNSPacket(client) // challenge
        client.Write(wrapInTNSData(buildTTCAuthPhase2("wrong_password")))
        // Should receive auth failure
        pkt, _ := readTNSPacket(client)
        assert.Contains(t, string(pkt.Payload), "ORA-01017") // invalid username/password
    }()

    err := session.handleAuth()
    assert.Error(t, err)
}
```

### Step 5: TTC Function Code Identification

Parse TTC function codes from TNS Data packets. At this stage, just identify and log — no deep decoding yet.

**Files:** `oracle/ttc.go`, `oracle/ttc_test.go`

**Tests:**

```go
// ttc_test.go

func TestParseTTCFunctionCode(t *testing.T) {
    tests := []struct {
        name    string
        payload []byte // TNS Data packet payload (after TNS header)
        want    TTCFunctionCode
    }{
        {"OALL8", buildTTCDataPayload(0x0E, []byte("...")), TTCFuncOALL8},
        {"OFETCH", buildTTCDataPayload(0x11, []byte("...")), TTCFuncOFETCH},
        {"OCLOSE", buildTTCDataPayload(0x05, []byte("...")), TTCFuncOCLOSE},
        {"OAUTH", buildTTCDataPayload(0x5E, []byte("...")), TTCFuncOAUTH},
        {"OOPEN", buildTTCDataPayload(0x03, []byte("...")), TTCFuncOOPEN},
        {"Response", buildTTCDataPayload(0x08, []byte("...")), TTCFuncResponse},
        {"OCANCEL", buildTTCDataPayload(0x14, []byte("...")), TTCFuncOCANCEL},
        {"OLOBOPS", buildTTCDataPayload(0x44, []byte("...")), TTCFuncOLOBOPS},
        {"SetProtocol", buildTTCDataPayload(0x01, []byte("...")), TTCFuncSetProtocol},
        {"SetDataTypes", buildTTCDataPayload(0x02, []byte("...")), TTCFuncSetDataTypes},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            fc, err := parseTTCFunctionCode(tt.payload)
            require.NoError(t, err)
            assert.Equal(t, tt.want, fc)
        })
    }
}

func TestParseTTCFunctionCode_EmptyPayload(t *testing.T) {
    _, err := parseTTCFunctionCode([]byte{})
    assert.ErrorIs(t, err, ErrTTCPayloadTooShort)
}

func TestParseTTCFunctionCode_DataFlagsPrefixed(t *testing.T) {
    // TNS Data payloads start with 2 bytes of data flags, then TTC
    payload := []byte{0x00, 0x00, 0x0E} // flags=0, func=OALL8
    fc, err := parseTTCFunctionCode(payload)
    require.NoError(t, err)
    assert.Equal(t, TTCFuncOALL8, fc)
}

func TestParseTTCFunctionCode_UnknownCode(t *testing.T) {
    payload := buildTTCDataPayload(0xFE, nil)
    fc, err := parseTTCFunctionCode(payload)
    require.NoError(t, err)
    assert.Equal(t, TTCFunctionCode(0xFE), fc)
    assert.False(t, fc.IsKnown())
}

func TestParseTTCFunctionCode_RealCaptures(t *testing.T) {
    // Load a real capture of "SELECT * FROM dual"
    // Expect: OALL8 from client, Response from server
    captures := []struct {
        file string
        want TTCFunctionCode
    }{
        {"testdata/captures/select_dual_request.bin", TTCFuncOALL8},
        {"testdata/captures/select_dual_response.bin", TTCFuncResponse},
        {"testdata/captures/fetch_request.bin", TTCFuncOFETCH},
        {"testdata/captures/close_cursor.bin", TTCFuncOCLOSE},
    }

    for _, c := range captures {
        t.Run(c.file, func(t *testing.T) {
            raw := loadCapture(t, c.file)
            fc, err := parseTTCFunctionCode(extractTNSDataPayload(raw))
            require.NoError(t, err)
            assert.Equal(t, c.want, fc)
        })
    }
}

func TestTTCFunctionCode_Stringer(t *testing.T) {
    assert.Equal(t, "OALL8", TTCFuncOALL8.String())
    assert.Equal(t, "OFETCH", TTCFuncOFETCH.String())
    assert.Equal(t, "UNKNOWN(0xfe)", TTCFunctionCode(0xFE).String())
}

func TestSessionLogsFunctionCodes(t *testing.T) {
    // Integration: verify that relayed packets get their function codes logged
    var logBuf bytes.Buffer
    logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

    upstream := newEchoTNSServer(t)
    client, proxyEnd := net.Pipe()

    session := &OracleSession{
        clientConn: proxyEnd,
        logger:     logger,
    }
    session.upstreamConn, _ = net.Dial("tcp", upstream.Addr().String())

    go session.proxyMessages()

    // Send an OALL8 packet
    client.Write(encodeTNSPacket(TNSPacketTypeData, buildTTCDataPayload(0x0E, []byte("SELECT 1"))))
    time.Sleep(50 * time.Millisecond)

    // Verify log contains function code
    assert.Contains(t, logBuf.String(), "OALL8")

    client.Close()
}
```

### Step 6: OALL8 SQL Extraction

Decode the OALL8 (parse+execute) message to extract SQL text, bind parameter values, and cursor ID. This is the hardest decoding step.

**Files:** `oracle/ttc_decode.go`, `oracle/ttc_decode_test.go`

**Tests:**

```go
// ttc_decode_test.go

func TestDecodeOALL8_SimpleSELECT(t *testing.T) {
    payload := buildOALL8("SELECT * FROM employees WHERE id = :1", []string{"42"}, 7)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, "SELECT * FROM employees WHERE id = :1", result.SQL)
    assert.Equal(t, uint16(7), result.CursorID)
    assert.Equal(t, []string{"42"}, result.BindValues)
}

func TestDecodeOALL8_NoBinds(t *testing.T) {
    payload := buildOALL8("SELECT SYSDATE FROM DUAL", nil, 1)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, "SELECT SYSDATE FROM DUAL", result.SQL)
    assert.Empty(t, result.BindValues)
}

func TestDecodeOALL8_MultipleBinds(t *testing.T) {
    sql := "INSERT INTO t (a, b, c) VALUES (:1, :2, :3)"
    binds := []string{"hello", "42", "2024-01-15"}
    payload := buildOALL8(sql, binds, 3)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, sql, result.SQL)
    assert.Equal(t, binds, result.BindValues)
}

func TestDecodeOALL8_PLSQLBlock(t *testing.T) {
    sql := "BEGIN my_package.do_something(:1, :2); END;"
    payload := buildOALL8(sql, []string{"arg1", "arg2"}, 5)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, sql, result.SQL)
    assert.True(t, result.IsPLSQL())
}

func TestDecodeOALL8_LargeSQL(t *testing.T) {
    // SQL larger than single TNS packet would normally be
    sql := "SELECT " + strings.Repeat("col, ", 1000) + "col FROM t"
    payload := buildOALL8(sql, nil, 10)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, sql, result.SQL)
}

func TestDecodeOALL8_EmptySQL(t *testing.T) {
    payload := buildOALL8("", nil, 1)
    _, err := decodeOALL8(payload)
    assert.ErrorIs(t, err, ErrEmptySQL)
}

func TestDecodeOALL8_UnicodeSQL(t *testing.T) {
    sql := "SELECT * FROM données WHERE nom = :1"
    payload := buildOALL8(sql, []string{"Éric"}, 2)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, sql, result.SQL)
    assert.Equal(t, []string{"Éric"}, result.BindValues)
}

func TestDecodeOALL8_NullBindValue(t *testing.T) {
    sql := "UPDATE t SET col = :1 WHERE id = :2"
    payload := buildOALL8WithNulls(sql, []interface{}{nil, 42}, 3)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, "NULL", result.BindValues[0])
    assert.Equal(t, "42", result.BindValues[1])
}

func TestDecodeOALL8_BinaryBindValue(t *testing.T) {
    sql := "INSERT INTO t (raw_col) VALUES (:1)"
    rawData := []byte{0xDE, 0xAD, 0xBE, 0xEF}
    payload := buildOALL8WithBinaryBind(sql, rawData, 4)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, "DEADBEEF", result.BindValues[0]) // hex-encoded
}

func TestDecodeOALL8_RealCapture_SelectDual(t *testing.T) {
    raw := loadCapture(t, "testdata/captures/select_dual_oall8.bin")
    result, err := decodeOALL8(extractTTCPayload(raw))
    require.NoError(t, err)
    assert.Contains(t, strings.ToUpper(result.SQL), "DUAL")
}

func TestDecodeOALL8_RealCapture_InsertWithBinds(t *testing.T) {
    raw := loadCapture(t, "testdata/captures/insert_oall8.bin")
    result, err := decodeOALL8(extractTTCPayload(raw))
    require.NoError(t, err)
    assert.True(t, strings.HasPrefix(strings.ToUpper(result.SQL), "INSERT"))
    assert.True(t, len(result.BindValues) > 0)
}

func TestDecodeOALL8_RealCapture_PLSQL(t *testing.T) {
    raw := loadCapture(t, "testdata/captures/plsql_oall8.bin")
    result, err := decodeOALL8(extractTTCPayload(raw))
    require.NoError(t, err)
    assert.True(t, result.IsPLSQL())
}

func TestDecodeOALL8_CorruptPayload(t *testing.T) {
    // Truncated payload → graceful error, not panic
    _, err := decodeOALL8([]byte{0x0E, 0x00, 0x01})
    assert.Error(t, err)
}

func TestDecodeOALL8_FuzzInputs(t *testing.T) {
    // Fuzz with random bytes → must not panic
    f := fuzz.New()
    for i := 0; i < 1000; i++ {
        var data []byte
        f.Fuzz(&data)
        decodeOALL8(data) // Just verify no panic; errors are fine
    }
}

func TestDecodeOFETCH(t *testing.T) {
    payload := buildOFETCH(7, 100) // cursor 7, fetch 100 rows
    result, err := decodeOFETCH(payload)
    require.NoError(t, err)
    assert.Equal(t, uint16(7), result.CursorID)
    assert.Equal(t, uint32(100), result.FetchSize)
}
```

### Step 7: Query Logging

Wire up OALL8 interception to the existing `store.CreateQuery`. Log SQL text, bind values, timing, connection ID.

**Files:** `oracle/intercept.go` (logging part), `oracle/intercept_test.go`

**Tests:**

```go
// intercept_test.go (query logging)

func TestQueryLogging_SimpleSELECT(t *testing.T) {
    // Send OALL8 through proxy → verify query appears in store
    ctx := context.Background()
    testStore := setupTestStore(t) // testcontainers PG for DBBat storage

    upstream := newFakeOracle(t) // Responds with 1 row to any SELECT
    defer upstream.Close()

    session := setupTestSession(t, testStore, upstream)
    session.connectionUID = uuid.Must(uuid.NewV7())

    // Simulate OALL8 + Response + ReadyForQuery equivalent
    oall8 := buildOALL8("SELECT 1 FROM DUAL", nil, 1)
    session.handleOALL8(oall8)

    // Simulate response completion
    session.completeQuery(nil, nil, 100)

    // Verify stored query
    queries, err := testStore.ListQueries(ctx, &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    require.NoError(t, err)
    require.Len(t, queries, 1)
    assert.Equal(t, "SELECT 1 FROM DUAL", queries[0].SQLText)
    assert.NotNil(t, queries[0].DurationMs)
    assert.True(t, *queries[0].DurationMs >= 0)
}

func TestQueryLogging_WithBindParameters(t *testing.T) {
    testStore := setupTestStore(t)
    upstream := newFakeOracle(t)
    session := setupTestSession(t, testStore, upstream)
    session.connectionUID = uuid.Must(uuid.NewV7())

    sql := "SELECT * FROM emp WHERE dept_id = :1 AND status = :2"
    oall8 := buildOALL8(sql, []string{"10", "ACTIVE"}, 2)
    session.handleOALL8(oall8)
    session.completeQuery(nil, nil, 0)

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    require.Len(t, queries, 1)
    assert.NotNil(t, queries[0].Parameters)
    assert.Equal(t, []string{"10", "ACTIVE"}, queries[0].Parameters.Values)
}

func TestQueryLogging_Error(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    session.handleOALL8(buildOALL8("SELECT * FROM nonexistent", nil, 1))
    errMsg := "ORA-00942: table or view does not exist"
    session.completeQuery(nil, &errMsg, 0)

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    require.Len(t, queries, 1)
    assert.NotNil(t, queries[0].Error)
    assert.Contains(t, *queries[0].Error, "ORA-00942")
}

func TestQueryLogging_DurationTracked(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    session.handleOALL8(buildOALL8("SELECT pg_sleep(0.1)", nil, 1))
    time.Sleep(100 * time.Millisecond)
    session.completeQuery(nil, nil, 0)

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    require.Len(t, queries, 1)
    assert.True(t, *queries[0].DurationMs >= 90, "duration should be ~100ms, got %f", *queries[0].DurationMs)
}

func TestQueryLogging_MultipleQueriesSameSession(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    for i := 0; i < 5; i++ {
        sql := fmt.Sprintf("SELECT %d FROM DUAL", i)
        session.handleOALL8(buildOALL8(sql, nil, uint16(i+1)))
        session.completeQuery(nil, nil, 0)
    }

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    assert.Len(t, queries, 5)
}

func TestQueryLogging_ConnectionStatsUpdated(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))

    // Create connection record
    conn, _ := testStore.CreateConnection(context.Background(), session.user.UID, session.database.UID, "127.0.0.1")
    session.connectionUID = conn.UID

    session.handleOALL8(buildOALL8("SELECT * FROM big_table", nil, 1))
    session.completeQuery(ptr(int64(500)), nil, 1048576) // 500 rows, 1MB

    // Verify connection stats updated
    updatedConn, _ := testStore.GetConnection(context.Background(), conn.UID)
    assert.Equal(t, int64(1), updatedConn.Queries)
    assert.True(t, updatedConn.BytesTransferred > 0)
}

func TestQueryLogging_CursorReuse(t *testing.T) {
    // Same cursor ID reused for different SQL (cursor close + reopen)
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    session.handleOALL8(buildOALL8("SELECT 1 FROM DUAL", nil, 5))
    session.completeQuery(nil, nil, 0)
    session.handleClose(5)

    session.handleOALL8(buildOALL8("SELECT 2 FROM DUAL", nil, 5))
    session.completeQuery(nil, nil, 0)

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    assert.Len(t, queries, 2)
    assert.Equal(t, "SELECT 1 FROM DUAL", queries[0].SQLText)
    assert.Equal(t, "SELECT 2 FROM DUAL", queries[1].SQLText)
}
```

### Step 8: Access Control Enforcement

Block writes, DDL, and Oracle-specific dangerous patterns based on grant controls.

**Files:** `shared/validation.go`, `oracle/intercept.go` (enforcement), `shared/validation_test.go`, `oracle/intercept_test.go`

**Tests:**

```go
// shared/validation_test.go

func TestValidateQuery_ReadOnly_BlocksWrites(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}

    blocked := []string{
        "INSERT INTO t VALUES (1)",
        "UPDATE t SET x = 1",
        "DELETE FROM t WHERE id = 1",
        "MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET t.x = s.x",
        "DROP TABLE t",
        "TRUNCATE TABLE t",
        "CREATE TABLE t (id NUMBER)",
        "ALTER TABLE t ADD (col VARCHAR2(100))",
        "GRANT SELECT ON t TO u",
        "REVOKE SELECT ON t FROM u",
    }

    for _, sql := range blocked {
        t.Run(sql, func(t *testing.T) {
            err := ValidateQuery(sql, grant)
            assert.ErrorIs(t, err, ErrReadOnlyViolation, "should block: %s", sql)
        })
    }
}

func TestValidateQuery_ReadOnly_AllowsReads(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}

    allowed := []string{
        "SELECT * FROM t",
        "SELECT 1 FROM DUAL",
        "WITH cte AS (SELECT 1 FROM DUAL) SELECT * FROM cte",
        "EXPLAIN PLAN FOR SELECT * FROM t",
        "  select * from t  ", // leading/trailing whitespace
    }

    for _, sql := range allowed {
        t.Run(sql, func(t *testing.T) {
            err := ValidateQuery(sql, grant)
            assert.NoError(t, err, "should allow: %s", sql)
        })
    }
}

func TestValidateQuery_BlockDDL(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlBlockDDL}}

    blocked := []string{
        "CREATE TABLE t (id NUMBER)",
        "ALTER TABLE t ADD (col NUMBER)",
        "DROP TABLE t",
        "CREATE INDEX idx ON t(col)",
        "CREATE OR REPLACE VIEW v AS SELECT 1 FROM DUAL",
        "CREATE SEQUENCE s",
        "ALTER INDEX idx REBUILD",
    }

    allowed := []string{
        "INSERT INTO t VALUES (1)", // DML is fine
        "SELECT * FROM t",
        "UPDATE t SET x = 1",
    }

    for _, sql := range blocked {
        assert.ErrorIs(t, ValidateQuery(sql, grant), ErrDDLBlocked, "should block: %s", sql)
    }
    for _, sql := range allowed {
        assert.NoError(t, ValidateQuery(sql, grant), "should allow: %s", sql)
    }
}

// oracle/intercept_test.go (Oracle-specific patterns)

func TestValidateOracleQuery_BlocksDangerousPatterns(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}

    blocked := []struct {
        sql    string
        reason string
    }{
        {"ALTER SYSTEM SET open_cursors=1000", "system config change"},
        {"ALTER SYSTEM KILL SESSION '123,456'", "kill session"},
        {"CREATE DATABASE LINK remote CONNECT TO u IDENTIFIED BY p USING 'tns'", "network escape"},
        {"BEGIN DBMS_SCHEDULER.CREATE_JOB(...); END;", "async execution"},
        {"SELECT UTL_HTTP.REQUEST('http://evil.com') FROM DUAL", "network access"},
        {"SELECT UTL_FILE.FOPEN('/etc/passwd','r') FROM DUAL", "file system access"},
        {"DECLARE PRAGMA AUTONOMOUS_TRANSACTION; BEGIN DELETE FROM t; COMMIT; END;", "autonomous transaction bypass"},
        {"BEGIN DBMS_PIPE.SEND_MESSAGE('pipe'); END;", "IPC escape"},
        {"BEGIN UTL_TCP.OPEN_CONNECTION('evil.com', 80); END;", "network escape"},
        {"BEGIN DBMS_JOB.SUBMIT(...); END;", "async execution"},
    }

    for _, tt := range blocked {
        t.Run(tt.reason, func(t *testing.T) {
            err := ValidateOracleQuery(tt.sql, grant)
            assert.Error(t, err, "should block %s: %s", tt.reason, tt.sql)
        })
    }
}

func TestValidateOracleQuery_AllowsSafePLSQL(t *testing.T) {
    grant := &store.Grant{} // No read_only restriction

    allowed := []string{
        "BEGIN my_pkg.read_data(:1, :2); END;",
        "DECLARE v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM t; END;",
        "BEGIN NULL; END;",
    }

    for _, sql := range allowed {
        assert.NoError(t, ValidateOracleQuery(sql, grant), "should allow: %s", sql)
    }
}

func TestValidateQuery_CaseInsensitiveMatching(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
    assert.Error(t, ValidateQuery("insert INTO t VALUES (1)", grant))
    assert.Error(t, ValidateQuery("INSERT into t values (1)", grant))
    assert.Error(t, ValidateQuery("  INSERT INTO t VALUES (1)  ", grant))
}

func TestValidateQuery_CommentBypass_Blocked(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}

    // Attempts to hide writes in comments should still be caught
    // (the actual statement after the comment is what matters)
    attacks := []string{
        "/* harmless */ INSERT INTO t VALUES (1)",
        "-- just a select\nINSERT INTO t VALUES (1)",
    }
    for _, sql := range attacks {
        assert.Error(t, ValidateQuery(sql, grant), "should block: %s", sql)
    }
}

func TestInterceptOALL8_BlockedQuery_SendsError(t *testing.T) {
    // Full session-level test: blocked query → TTC error sent to client
    client, proxyEnd := net.Pipe()
    defer client.Close()

    session := &OracleSession{
        clientConn: proxyEnd,
        grant:      &store.Grant{Controls: []string{store.ControlReadOnly}},
    }

    // Client sends a write query
    go func() {
        client.Write(encodeTNSPacket(TNSPacketTypeData,
            buildTTCDataPayload(0x0E, buildOALL8("DELETE FROM t", nil, 1))))
    }()

    err := session.handleClientMessage()
    assert.ErrorIs(t, err, ErrReadOnlyViolation)

    // Client should receive ORA error
    pkt, _ := readTNSPacket(client)
    assert.Equal(t, TNSPacketTypeData, pkt.Type)
    fc, _ := parseTTCFunctionCode(pkt.Payload)
    assert.Equal(t, TTCFuncResponse, fc)
    assert.Contains(t, string(pkt.Payload), "1031") // ORA-01031
}

func TestInterceptOALL8_AllowedQuery_Forwarded(t *testing.T) {
    upstream := newEchoTNSServer(t)
    client, proxyEnd := net.Pipe()
    defer client.Close()

    session := &OracleSession{
        clientConn: proxyEnd,
        grant:      &store.Grant{Controls: []string{store.ControlReadOnly}},
    }
    session.upstreamConn, _ = net.Dial("tcp", upstream.Addr().String())

    // Send a read query → should be forwarded to upstream
    go func() {
        client.Write(encodeTNSPacket(TNSPacketTypeData,
            buildTTCDataPayload(0x0E, buildOALL8("SELECT 1 FROM DUAL", nil, 1))))
    }()

    err := session.handleClientMessage()
    assert.NoError(t, err)
}
```

### Step 9: Response Parsing + Row Capture

Decode TTC responses to capture column definitions, row data, and Oracle data types. Store captured rows via existing QueryRow model.

**Files:** `oracle/ttc_decode.go` (response parsing), `oracle/types.go`, `oracle/types_test.go`, `oracle/ttc_decode_test.go`

**Tests:**

```go
// types_test.go — Oracle data type decoders

func TestDecodeOracleNumber(t *testing.T) {
    tests := []struct {
        name     string
        raw      []byte
        expected string
    }{
        {"zero", []byte{0x80}, "0"},
        {"one", []byte{0xC1, 0x02}, "1"},
        {"negative_one", []byte{0x3E, 0x64, 0x66}, "-1"},
        {"large", []byte{0xC3, 0x0D, 0x2A, 0x04}, "123456"}, // Verify against go-ora
        {"decimal", []byte{0xC1, 0x04, 0x1F}, "3.14"},        // Approximate
        {"max_precision", generateOracleNumber(t, "99999999999999999999999999999999999999"), "99999999999999999999999999999999999999"},
        {"very_small", generateOracleNumber(t, "0.000001"), "0.000001"},
        {"negative_decimal", generateOracleNumber(t, "-42.5"), "-42.5"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := decodeOracleNumber(tt.raw)
            require.NoError(t, err)
            assert.Equal(t, tt.expected, result)
        })
    }
}

func TestDecodeOracleNumber_EmptyBytes(t *testing.T) {
    _, err := decodeOracleNumber([]byte{})
    assert.Error(t, err)
}

func TestDecodeOracleDate(t *testing.T) {
    tests := []struct {
        name     string
        raw      []byte // 7-byte Oracle DATE format
        expected time.Time
    }{
        {
            "2024-03-15 14:30:00",
            []byte{120, 124, 3, 15, 15, 31, 1}, // century=120(20th), year=124(24), month=3, day=15, hour=15(14+1), min=31(30+1), sec=1(0+1)
            time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC),
        },
        {
            "2000-01-01 00:00:00",
            []byte{120, 100, 1, 1, 1, 1, 1},
            time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
        },
        {
            "1999-12-31 23:59:59",
            []byte{119, 199, 12, 31, 24, 60, 60},
            time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC),
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := decodeOracleDate(tt.raw)
            require.NoError(t, err)
            assert.Equal(t, tt.expected, result)
        })
    }
}

func TestDecodeOracleDate_WrongLength(t *testing.T) {
    _, err := decodeOracleDate([]byte{1, 2, 3}) // Need 7 bytes
    assert.ErrorIs(t, err, ErrInvalidDateLength)
}

func TestDecodeOracleTimestamp(t *testing.T) {
    // TIMESTAMP has 7 DATE bytes + 4 bytes for fractional seconds (nanoseconds)
    raw := append(
        []byte{120, 124, 6, 15, 11, 31, 1}, // 2024-06-15 10:30:00
        []byte{0x05, 0xF5, 0xE1, 0x00}...,  // 100000000 ns = 0.1 seconds
    )
    result, err := decodeOracleTimestamp(raw)
    require.NoError(t, err)
    assert.Equal(t, 2024, result.Year())
    assert.Equal(t, time.June, result.Month())
    assert.Equal(t, 100000000, result.Nanosecond())
}

func TestDecodeOracleVARCHAR2(t *testing.T) {
    result, err := decodeOracleValue(1, []byte("hello world"))
    require.NoError(t, err)
    assert.Equal(t, "hello world", result)
}

func TestDecodeOracleVARCHAR2_UTF8(t *testing.T) {
    result, err := decodeOracleValue(1, []byte("héllo wörld 日本語"))
    require.NoError(t, err)
    assert.Equal(t, "héllo wörld 日本語", result)
}

func TestDecodeOracleCHAR_TrimsPadding(t *testing.T) {
    result, err := decodeOracleValue(96, []byte("hello     "))
    require.NoError(t, err)
    assert.Equal(t, "hello", result)
}

func TestDecodeOracleRAW(t *testing.T) {
    result, err := decodeOracleValue(23, []byte{0xDE, 0xAD, 0xBE, 0xEF})
    require.NoError(t, err)
    assert.Equal(t, "deadbeef", result)
}

func TestDecodeOracleBINARY_FLOAT(t *testing.T) {
    buf := make([]byte, 4)
    binary.BigEndian.PutUint32(buf, math.Float32bits(3.14))
    result, err := decodeOracleValue(100, buf)
    require.NoError(t, err)
    assert.InDelta(t, 3.14, result, 0.01)
}

func TestDecodeOracleBINARY_DOUBLE(t *testing.T) {
    buf := make([]byte, 8)
    binary.BigEndian.PutUint64(buf, math.Float64bits(2.718281828))
    result, err := decodeOracleValue(101, buf)
    require.NoError(t, err)
    assert.InDelta(t, 2.718281828, result, 0.0001)
}

func TestDecodeOracleLOB_ReturnsPlaceholder(t *testing.T) {
    // CLOB
    result, err := decodeOracleValue(112, []byte{0x01, 0x02, 0x03})
    require.NoError(t, err)
    assert.Equal(t, "[LOB]", result)

    // BLOB
    result, err = decodeOracleValue(113, []byte{0x01, 0x02, 0x03})
    require.NoError(t, err)
    assert.Equal(t, "[LOB]", result)
}

func TestDecodeOracleValue_UnknownType_Base64(t *testing.T) {
    result, err := decodeOracleValue(255, []byte{0x01, 0x02})
    require.NoError(t, err)
    assert.Equal(t, base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}), result)
}

func TestDecodeOracleValue_NilData(t *testing.T) {
    result, err := decodeOracleValue(1, nil)
    require.NoError(t, err)
    assert.Nil(t, result)
}

// ttc_decode_test.go — Response parsing

func TestDecodeTTCResponse_ColumnDefinitions(t *testing.T) {
    resp := buildTTCResponse(
        []columnDef{
            {Name: "ID", TypeCode: 2, Size: 22},       // NUMBER
            {Name: "NAME", TypeCode: 1, Size: 100},     // VARCHAR2
            {Name: "CREATED", TypeCode: 12, Size: 7},   // DATE
        },
        nil, // no rows yet
    )
    result, err := decodeTTCResponse(resp)
    require.NoError(t, err)
    assert.Len(t, result.Columns, 3)
    assert.Equal(t, "ID", result.Columns[0].Name)
    assert.Equal(t, OracleTypeNUMBER, result.Columns[0].TypeCode)
    assert.Equal(t, "NAME", result.Columns[1].Name)
    assert.Equal(t, "CREATED", result.Columns[2].Name)
}

func TestDecodeTTCResponse_WithRows(t *testing.T) {
    resp := buildTTCResponse(
        []columnDef{
            {Name: "ID", TypeCode: 2},
            {Name: "NAME", TypeCode: 1},
        },
        [][]interface{}{
            {1, "Alice"},
            {2, "Bob"},
        },
    )
    result, err := decodeTTCResponse(resp)
    require.NoError(t, err)
    assert.Len(t, result.Rows, 2)
}

func TestDecodeTTCResponse_ErrorResponse(t *testing.T) {
    resp := buildTTCErrorResponse(942, "ORA-00942: table or view does not exist")
    result, err := decodeTTCResponse(resp)
    require.NoError(t, err)
    assert.True(t, result.IsError)
    assert.Equal(t, 942, result.ErrorCode)
    assert.Contains(t, result.ErrorMessage, "ORA-00942")
}

func TestDecodeTTCResponse_MoreDataFlag(t *testing.T) {
    resp := buildTTCResponseWithMoreData(true)
    result, err := decodeTTCResponse(resp)
    require.NoError(t, err)
    assert.True(t, result.MoreData)
}

func TestRowCapture_Limits(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())
    session.queryStorage = config.QueryStorageConfig{
        StoreResults:   true,
        MaxResultRows:  3,
        MaxResultBytes: 1024,
    }

    session.handleOALL8(buildOALL8("SELECT * FROM big_table", nil, 1))

    // Simulate 10 rows arriving
    for i := 0; i < 10; i++ {
        session.captureRow(
            []columnDef{{Name: "id", TypeCode: 2}, {Name: "data", TypeCode: 1}},
            []interface{}{i, fmt.Sprintf("row-%d", i)},
        )
    }

    session.completeQuery(ptr(int64(10)), nil, 5000)

    // Verify: no rows stored (truncated — same behavior as PG proxy)
    queries, _ := testStore.ListQueriesWithRows(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    require.Len(t, queries, 1)
    assert.Len(t, queries[0].Rows, 0) // Truncated → all discarded
}

func TestRowCapture_WithinLimits(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())
    session.queryStorage = config.QueryStorageConfig{
        StoreResults:   true,
        MaxResultRows:  100,
        MaxResultBytes: 1048576,
    }

    session.handleOALL8(buildOALL8("SELECT id, name FROM emp", nil, 1))

    session.captureRow(
        []columnDef{{Name: "ID", TypeCode: 2}, {Name: "NAME", TypeCode: 1}},
        []interface{}{1, "Alice"},
    )
    session.captureRow(
        []columnDef{{Name: "ID", TypeCode: 2}, {Name: "NAME", TypeCode: 1}},
        []interface{}{2, "Bob"},
    )

    session.completeQuery(ptr(int64(2)), nil, 200)

    queries, _ := testStore.ListQueriesWithRows(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    require.Len(t, queries, 1)
    require.Len(t, queries[0].Rows, 2)

    // Verify JSON structure matches PG format
    var row0 map[string]interface{}
    json.Unmarshal(queries[0].Rows[0].RowData, &row0)
    assert.Equal(t, "1", row0["ID"])
    assert.Equal(t, "Alice", row0["NAME"])
}

func TestRowCapture_MultiFetch(t *testing.T) {
    // OALL8 returns partial rows, then OFETCH gets more
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())
    session.queryStorage = config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100, MaxResultBytes: 1048576}

    // OALL8: parse+execute, get first batch
    session.handleOALL8(buildOALL8("SELECT id FROM t", nil, 5))
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}}, []interface{}{1})
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}}, []interface{}{2})
    // More data flag → don't complete yet

    // OFETCH: get second batch
    session.handleOFETCH(5) // cursor 5
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}}, []interface{}{3})
    // No more data → complete
    session.completeQuery(ptr(int64(3)), nil, 300)

    queries, _ := testStore.ListQueriesWithRows(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    require.Len(t, queries, 1)
    assert.Len(t, queries[0].Rows, 3)
    assert.Equal(t, "SELECT id FROM t", queries[0].SQLText)
}

func TestRowCapture_StoreResultsDisabled(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())
    session.queryStorage = config.QueryStorageConfig{StoreResults: false}

    session.handleOALL8(buildOALL8("SELECT * FROM t", nil, 1))
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}}, []interface{}{1})
    session.completeQuery(ptr(int64(1)), nil, 100)

    queries, _ := testStore.ListQueriesWithRows(context.Background(), &store.QueryFilter{
        ConnectionID: &session.connectionUID,
    })
    require.Len(t, queries, 1)
    assert.Len(t, queries[0].Rows, 0) // No rows captured
}

func TestDecodeTTCResponse_RealCapture(t *testing.T) {
    raw := loadCapture(t, "testdata/captures/select_emp_response.bin")
    result, err := decodeTTCResponse(extractTTCPayload(raw))
    require.NoError(t, err)
    assert.True(t, len(result.Columns) > 0)
    assert.True(t, len(result.Rows) > 0)
}
```

### Step 10: Full Integration Test

End-to-end test with a real Oracle database (via testcontainers), a real client, and the proxy in between.

**Files:** `oracle/integration_test.go`

#### Container Strategy

Oracle 19c is the primary target (most deployed LTS version), but 19c was never released as XE/Free — so we can't get it without Oracle SSO credentials. Our approach:

| Environment | Image | Why |
|-------------|-------|-----|
| **CI (GitHub Actions)** | `gvenzl/oracle-xe:18.4.0-slim` | No auth needed, 18c shares TTC protocol version with 19c, ~1.5GB, ~2min startup |
| **CI (extended matrix)** | `gvenzl/oracle-free:23-slim` | Tests against latest TTC version, catches forward-compat issues |
| **Local dev (optional)** | `container-registry.oracle.com/database/enterprise:19.3.0.0` | Real 19c, requires one-time `docker login` with Oracle SSO |

The integration tests accept the Oracle image as an env var so developers can run against any version:

```bash
# Default (CI): Oracle XE 18c (19c-compatible TTC)
make test-e2e-oracle

# Against 23ai
ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim make test-e2e-oracle

# Against real 19c (requires Oracle SSO docker login)
ORACLE_TEST_IMAGE=container-registry.oracle.com/database/enterprise:19.3.0.0 make test-e2e-oracle
```

**Tests:**

```go
// integration_test.go
//
// These tests require Docker and use testcontainers to spin up
// an Oracle database and PostgreSQL (for DBBat storage).
// The Oracle image is configurable via ORACLE_TEST_IMAGE env var.
// Default: gvenzl/oracle-xe:18.4.0-slim (TTC-compatible with 19c)
// Tagged with //go:build integration

//go:build integration

const defaultOracleImage = "gvenzl/oracle-xe:18.4.0-slim"

func oracleTestImage() string {
    if img := os.Getenv("ORACLE_TEST_IMAGE"); img != "" {
        return img
    }
    return defaultOracleImage
}

func TestIntegration_Setup(t *testing.T) {
    // Verify we can start Oracle container
    oracleC := startOracleContainer(t)
    defer oracleC.Terminate(context.Background())

    dsn := oracleC.MustConnectionString(t)
    assert.NotEmpty(t, dsn)
    t.Logf("Oracle container started: image=%s dsn=%s", oracleTestImage(), dsn)
}

func TestIntegration_ConnectThroughProxy(t *testing.T) {
    ctx := context.Background()
    oracleC := startOracleContainer(t)
    defer oracleC.Terminate(ctx)
    pgC := startPostgresContainer(t) // For DBBat storage
    defer pgC.Terminate(ctx)

    // Set up DBBat store with Oracle database + user + grant
    testStore := setupStoreFromContainer(t, pgC)
    testStore.CreateUser(ctx, "TESTUSER", "testpass", []string{"connector"})
    testStore.CreateOracleDatabase(ctx, "TESTDB", oracleC.Host(), oracleC.Port(), "FREEPDB1", "system", "oracle")
    testStore.CreateGrant(ctx, "TESTUSER", "TESTDB")

    // Start Oracle proxy
    proxy := NewOracleServer(":0", testStore, nil, slog.Default())
    go proxy.Start()
    defer proxy.Stop()

    // Connect through proxy using go-ora
    proxyDSN := fmt.Sprintf("oracle://TESTUSER:testpass@%s/TESTDB", proxy.Addr())
    db, err := sql.Open("oracle", proxyDSN)
    require.NoError(t, err)
    defer db.Close()

    err = db.PingContext(ctx)
    require.NoError(t, err)
}

func TestIntegration_SimpleQuery(t *testing.T) {
    ctx := context.Background()
    env := setupIntegrationEnv(t) // Helper: Oracle + PG + store + proxy + grant
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    var result string
    err := db.QueryRowContext(ctx, "SELECT 'hello' FROM DUAL").Scan(&result)
    require.NoError(t, err)
    assert.Equal(t, "hello", result)

    // Verify query was logged
    time.Sleep(100 * time.Millisecond) // Async logging
    queries := env.Store.ListQueries(ctx, &store.QueryFilter{})
    require.True(t, len(queries) >= 1)
    assert.Contains(t, queries[0].SQLText, "DUAL")
}

func TestIntegration_QueryWithBinds(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()
    ctx := context.Background()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    // Create test table via upstream (bypass proxy for setup)
    env.ExecUpstream("CREATE TABLE test_binds (id NUMBER, name VARCHAR2(100))")
    env.ExecUpstream("INSERT INTO test_binds VALUES (1, 'Alice')")
    env.ExecUpstream("INSERT INTO test_binds VALUES (2, 'Bob')")
    env.ExecUpstream("COMMIT")

    var name string
    err := db.QueryRowContext(ctx, "SELECT name FROM test_binds WHERE id = :1", 1).Scan(&name)
    require.NoError(t, err)
    assert.Equal(t, "Alice", name)

    // Verify bind parameters were captured
    time.Sleep(100 * time.Millisecond)
    queries := env.ListQueriesForUser("TESTUSER")
    found := false
    for _, q := range queries {
        if strings.Contains(q.SQLText, "test_binds") {
            found = true
            assert.NotNil(t, q.Parameters)
            assert.Contains(t, q.Parameters.Values, "1")
        }
    }
    assert.True(t, found, "bind query not found in logs")
}

func TestIntegration_ResultCapture(t *testing.T) {
    env := setupIntegrationEnv(t)
    env.QueryStorage = config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100, MaxResultBytes: 1048576}
    defer env.Cleanup()
    ctx := context.Background()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    env.ExecUpstream("CREATE TABLE test_capture (id NUMBER, val VARCHAR2(50))")
    env.ExecUpstream("INSERT INTO test_capture VALUES (1, 'alpha')")
    env.ExecUpstream("INSERT INTO test_capture VALUES (2, 'beta')")
    env.ExecUpstream("COMMIT")

    rows, err := db.QueryContext(ctx, "SELECT id, val FROM test_capture ORDER BY id")
    require.NoError(t, err)
    defer rows.Close()
    for rows.Next() {
        var id int
        var val string
        rows.Scan(&id, &val)
    }

    time.Sleep(200 * time.Millisecond)
    queriesWithRows := env.ListQueriesWithRowsForUser("TESTUSER")
    found := false
    for _, q := range queriesWithRows {
        if strings.Contains(q.SQLText, "test_capture") {
            found = true
            assert.Len(t, q.Rows, 2)
            var row0 map[string]interface{}
            json.Unmarshal(q.Rows[0].RowData, &row0)
            assert.Equal(t, "1", row0["ID"])
            assert.Equal(t, "alpha", row0["VAL"])
        }
    }
    assert.True(t, found)
}

func TestIntegration_ReadOnlyEnforcement(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    env.SetGrantControls("TESTUSER", "TESTDB", []string{store.ControlReadOnly})

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    // SELECT should work
    var n int
    err := db.QueryRowContext(context.Background(), "SELECT 1 FROM DUAL").Scan(&n)
    assert.NoError(t, err)

    // INSERT should be blocked by proxy (before reaching Oracle)
    _, err = db.ExecContext(context.Background(), "CREATE TABLE t (id NUMBER)")
    assert.Error(t, err)
    assert.Contains(t, err.Error(), "ORA-01031") // Our custom error
}

func TestIntegration_DDLBlocking(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    env.SetGrantControls("TESTUSER", "TESTDB", []string{store.ControlBlockDDL})

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    // DML should work
    env.ExecUpstream("CREATE TABLE test_ddl (id NUMBER)")
    _, err := db.ExecContext(context.Background(), "INSERT INTO test_ddl VALUES (1)")
    assert.NoError(t, err)

    // DDL should be blocked
    _, err = db.ExecContext(context.Background(), "DROP TABLE test_ddl")
    assert.Error(t, err)
}

func TestIntegration_OracleSpecificBlocking(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    env.SetGrantControls("TESTUSER", "TESTDB", []string{store.ControlReadOnly})

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    dangerousQueries := []string{
        "ALTER SYSTEM SET open_cursors=5000",
        "CREATE DATABASE LINK remote CONNECT TO sys IDENTIFIED BY pwd USING 'remote'",
    }

    for _, sql := range dangerousQueries {
        _, err := db.ExecContext(context.Background(), sql)
        assert.Error(t, err, "should block: %s", sql)
    }
}

func TestIntegration_ConnectionTracking(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()
    ctx := context.Background()

    db := env.ConnectAsUser("TESTUSER", "testpass")

    // Run a query
    db.QueryRowContext(ctx, "SELECT 1 FROM DUAL")

    // Check connection record exists
    conns := env.Store.ListConnections(ctx, &store.ConnectionFilter{})
    require.True(t, len(conns) >= 1)

    latestConn := conns[0]
    assert.NotNil(t, latestConn.ConnectedAt)
    assert.Nil(t, latestConn.DisconnectedAt) // Still open

    // Close connection
    db.Close()
    time.Sleep(100 * time.Millisecond)

    // Verify disconnection recorded
    updatedConn, _ := env.Store.GetConnection(ctx, latestConn.UID)
    assert.NotNil(t, updatedConn.DisconnectedAt)
    assert.True(t, updatedConn.Queries >= 1)
}

func TestIntegration_QuotaEnforcement(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    maxQueries := int64(3)
    env.SetGrantQuota("TESTUSER", "TESTDB", &maxQueries, nil)

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    // First 3 queries should succeed
    for i := 0; i < 3; i++ {
        _, err := db.QueryContext(context.Background(), "SELECT 1 FROM DUAL")
        assert.NoError(t, err, "query %d should succeed", i+1)
    }

    // Reconnect (quota checked at connection time)
    db.Close()
    db2, err := env.TryConnect("TESTUSER", "testpass")
    if err == nil {
        // If connection succeeds, queries should fail
        _, err = db2.QueryContext(context.Background(), "SELECT 1 FROM DUAL")
        // Either connection refused or query fails
        db2.Close()
    }
    // One of these should have failed due to quota
}

func TestIntegration_MultipleDataTypes(t *testing.T) {
    env := setupIntegrationEnv(t)
    env.QueryStorage = config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100, MaxResultBytes: 1048576}
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    env.ExecUpstream(`CREATE TABLE test_types (
        num_col NUMBER(10,2),
        str_col VARCHAR2(100),
        date_col DATE,
        ts_col TIMESTAMP,
        raw_col RAW(16),
        float_col BINARY_FLOAT,
        double_col BINARY_DOUBLE,
        char_col CHAR(10)
    )`)
    env.ExecUpstream(`INSERT INTO test_types VALUES (
        42.50, 'hello', DATE '2024-03-15', TIMESTAMP '2024-03-15 14:30:00',
        HEXTORAW('DEADBEEF'), 3.14, 2.718281828, 'fixed'
    )`)
    env.ExecUpstream("COMMIT")

    rows, _ := db.QueryContext(context.Background(), "SELECT * FROM test_types")
    defer rows.Close()
    for rows.Next() {
        // Just drain rows
        cols := make([]interface{}, 8)
        ptrs := make([]interface{}, 8)
        for i := range cols {
            ptrs[i] = &cols[i]
        }
        rows.Scan(ptrs...)
    }

    time.Sleep(200 * time.Millisecond)
    queriesWithRows := env.ListQueriesWithRowsForUser("TESTUSER")
    for _, q := range queriesWithRows {
        if strings.Contains(q.SQLText, "test_types") {
            require.Len(t, q.Rows, 1)
            var row map[string]interface{}
            json.Unmarshal(q.Rows[0].RowData, &row)

            assert.Equal(t, "42.50", row["NUM_COL"])
            assert.Equal(t, "hello", row["STR_COL"])
            assert.Contains(t, row["DATE_COL"], "2024-03-15")
            assert.Equal(t, "deadbeef", row["RAW_COL"])
            assert.Equal(t, "fixed", row["CHAR_COL"]) // Trimmed
        }
    }
}

func TestIntegration_ConcurrentSessions(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    var wg sync.WaitGroup
    errors := make([]error, 10)

    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            db := env.ConnectAsUser("TESTUSER", "testpass")
            defer db.Close()

            var n int
            errors[idx] = db.QueryRowContext(context.Background(),
                fmt.Sprintf("SELECT %d FROM DUAL", idx)).Scan(&n)
        }(i)
    }

    wg.Wait()
    for i, err := range errors {
        assert.NoError(t, err, "concurrent session %d failed", i)
    }
}

func TestIntegration_LargeResultSet(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    // Use Oracle's CONNECT BY to generate many rows
    rows, err := db.QueryContext(context.Background(),
        "SELECT LEVEL AS n FROM DUAL CONNECT BY LEVEL <= 10000")
    require.NoError(t, err)
    defer rows.Close()

    count := 0
    for rows.Next() {
        var n int
        rows.Scan(&n)
        count++
    }
    assert.Equal(t, 10000, count)

    // Verify query was logged (even if rows weren't captured due to limits)
    time.Sleep(200 * time.Millisecond)
    queries := env.ListQueriesForUser("TESTUSER")
    found := false
    for _, q := range queries {
        if strings.Contains(q.SQLText, "CONNECT BY") {
            found = true
            assert.Equal(t, int64(10000), *q.RowsAffected)
        }
    }
    assert.True(t, found)
}

func TestIntegration_PLSQL(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    env.ExecUpstream("CREATE TABLE test_plsql (val NUMBER)")

    _, err := db.ExecContext(context.Background(),
        "BEGIN INSERT INTO test_plsql VALUES (42); COMMIT; END;")
    require.NoError(t, err)

    // Verify PL/SQL block was logged
    time.Sleep(100 * time.Millisecond)
    queries := env.ListQueriesForUser("TESTUSER")
    found := false
    for _, q := range queries {
        if strings.Contains(q.SQLText, "BEGIN") && strings.Contains(q.SQLText, "test_plsql") {
            found = true
        }
    }
    assert.True(t, found)
}

// --- Test helpers ---

func startOracleContainer(t *testing.T) testcontainers.Container {
    t.Helper()
    ctx := context.Background()
    image := oracleTestImage()

    // Detect image family for env var and wait strategy differences
    isXE := strings.Contains(image, "oracle-xe")
    isFree := strings.Contains(image, "oracle-free")
    isEnterprise := strings.Contains(image, "enterprise")

    env := map[string]string{}
    switch {
    case isXE:
        env["ORACLE_PASSWORD"] = "oracle"
    case isFree:
        env["ORACLE_PASSWORD"] = "oracle"
    case isEnterprise:
        env["ORACLE_SID"] = "ORCLCDB"
        env["ORACLE_PDB"] = "ORCLPDB1"
        env["ORACLE_PWD"] = "oracle"
    }

    // Enterprise 19c takes longer to start
    timeout := 5 * time.Minute
    if isEnterprise {
        timeout = 10 * time.Minute
    }

    req := testcontainers.ContainerRequest{
        Image:        image,
        ExposedPorts: []string{"1521/tcp"},
        Env:          env,
        WaitingFor:   wait.ForLog("DATABASE IS READY TO USE!").WithStartupTimeout(timeout),
    }
    container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
        ContainerRequest: req,
        Started:          true,
    })
    require.NoError(t, err)
    t.Logf("Oracle container ready: image=%s", image)
    return container
}

func TestIntegration_VersionDetection(t *testing.T) {
    // Verify which Oracle version we're testing against
    // Useful for CI logs and debugging TTC version-specific failures
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    var banner string
    err := db.QueryRowContext(context.Background(),
        "SELECT banner FROM v$version WHERE ROWNUM = 1").Scan(&banner)
    require.NoError(t, err)
    t.Logf("Oracle version: %s", banner)

    // Verify the version matches our expectation
    image := oracleTestImage()
    switch {
    case strings.Contains(image, "oracle-xe:18"):
        assert.Contains(t, banner, "18c")
    case strings.Contains(image, "oracle-free:23"):
        assert.Contains(t, banner, "23")
    case strings.Contains(image, "enterprise:19"):
        assert.Contains(t, banner, "19c")
    }
}
```
