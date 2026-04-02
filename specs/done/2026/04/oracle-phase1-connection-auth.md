# Oracle Proxy — Phase 1: Connection & Authentication

> Parent spec: `specs/2026-04-02-oracle-proxy.md`

## Goal

Accept Oracle client connections on a dedicated TCP port (`DBB_LISTEN_ORA`, default `:1522`), parse the TNS Connect descriptor to identify the target database, verify the user has an active DBBat grant, then relay authentication to the upstream Oracle database. At the end of this phase, an Oracle client (sqlplus, JDBC thin, go-ora) can connect through DBBat to a real Oracle database, with DBBat enforcing grant-based access control at connection time.

## Prerequisites

- None (this is the first phase)

## Outcome

- `oracle/tns.go` — TNS packet reader/writer
- `oracle/connect_descriptor.go` — connect descriptor parser
- `oracle/server.go` — TCP listener
- `oracle/session.go` — session lifecycle + raw relay
- `oracle/auth.go` — auth passthrough with grant checking
- Database model updated with `protocol` + `oracle_service_name`
- SQL migration for schema changes
- Config updated with `DBB_LISTEN_ORA`

## Non-Goals (deferred to Phase 2/3)

- Query interception or SQL inspection
- TTC function code parsing beyond AUTH identification
- Result capture or row storage
- Access control on individual queries (read-only, DDL blocking)

---

## Architecture

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
    │                               │  Connect to upstream Oracle    │
    │                               │──────────────────────────────>│
    │                               │  Upstream: TNS Accept          │
    │                               │<──────────────────────────────│
    │                               │                                 │
    │  2. TNS Accept                │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │  3-6. TTC negotiation         │  3-6. TTC negotiation          │
    │  (Set Protocol, Data Types)   │  (relayed transparently)       │
    │<─────────────────────────────>│<─────────────────────────────>│
    │                               │                                 │
    │  7. TTC AUTH Phase 1          │                                 │
    │  (username)                   │                                 │
    │──────────────────────────────>│                                 │
    │                               │                                 │
    │                               │  DBBat checks:                 │
    │                               │  - Look up user by username    │
    │                               │  - Check active grant          │
    │                               │  - Check quotas                │
    │                               │  If no grant → TNS Refuse      │
    │                               │                                 │
    │                               │  7. Relay AUTH Phase 1         │
    │                               │──────────────────────────────>│
    │  8. AUTH Challenge            │  8. AUTH Challenge              │
    │<──────────────────────────────│<──────────────────────────────│
    │  9. AUTH Phase 2              │  9. Relay AUTH Phase 2         │
    │──────────────────────────────>│──────────────────────────────>│
    │  10. AUTH OK / Fail           │  10. AUTH OK / Fail            │
    │<──────────────────────────────│<──────────────────────────────│
    │                               │                                 │
    │         === Raw bidirectional relay ===                         │
```

**Authentication strategy (MVP):** Auth passthrough. DBBat intercepts the TTC AUTH Phase 1 to extract the username, checks grants, then relays the full O5Logon exchange to the upstream Oracle. Oracle validates the password. DBBat controls *who can connect to which database*; Oracle controls *password validation*.

---

## TNS Packet Structure

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

Packet types:
| Type | Code | Direction | Purpose |
|------|------|-----------|---------|
| Connect | 1 | C→S | Initial connection with connect descriptor |
| Accept | 2 | S→C | Connection accepted |
| Refuse | 3 | S→C | Connection refused (with reason) |
| Redirect | 4 | S→C | Redirect to different address |
| Marker | 5 | Bidir | Break/reset signals |
| Data | 6 | Bidir | TTC payload carrier |
| Resend | 11 | S→C | Request packet retransmission |
| Control | 12 | Bidir | Control messages |

## TNS Connect Descriptor

The client sends a connect string in the TNS Connect packet payload:

```
(DESCRIPTION=
  (ADDRESS=(PROTOCOL=TCP)(HOST=proxy-host)(PORT=1522))
  (CONNECT_DATA=
    (SERVICE_NAME=ORCL)
    (CID=(PROGRAM=sqlplus)(HOST=client-host)(USER=jdoe))))
```

Extract:
- `SERVICE_NAME` → maps to `Database.Name` (or `Database.OracleServiceName`)
- `USER` from CID → informational only (real username comes in TTC AUTH)
- Also handle EZ Connect format: `host:port/service_name`

## Database Model Changes

```go
// New protocol constants
const (
    ProtocolPostgreSQL = "postgresql"
    ProtocolOracle     = "oracle"
)

// Database model gains:
type Database struct {
    // ... existing fields ...
    Protocol          string  `bun:"protocol,notnull,default:'postgresql'" json:"protocol"`
    OracleServiceName *string `bun:"oracle_service_name" json:"oracle_service_name,omitempty"`
}
```

```sql
-- Migration: YYYYMMDDHHMMSS_oracle_protocol.up.sql
ALTER TABLE databases ADD COLUMN protocol TEXT NOT NULL DEFAULT 'postgresql';
ALTER TABLE databases ADD COLUMN oracle_service_name TEXT;

--bun:split

-- Migration: YYYYMMDDHHMMSS_oracle_protocol.down.sql
ALTER TABLE databases DROP COLUMN oracle_service_name;
ALTER TABLE databases DROP COLUMN protocol;
```

## Config Changes

```go
// config/config.go
type Config struct {
    // ... existing ...
    ListenOracle string `koanf:"listen_ora"` // DBB_LISTEN_ORA, default ":1522"
}
```

## Session Struct

```go
type OracleSession struct {
    clientConn    net.Conn
    upstreamConn  net.Conn
    store         *store.Store
    encryptionKey []byte
    logger        *slog.Logger
    ctx           context.Context
    authCache     *cache.AuthCache

    // Connection metadata
    serviceName       string
    username          string
    database          *store.Database
    user              *store.User
    grant             *store.Grant
    connectionUID     uuid.UUID
    authenticated     bool

    // TNS state
    tnsVersion    uint16
    maxPacketSize uint32
}
```

## Key Reference: go-ora Source Files

| What we need | go-ora file | What to extract |
|-------------|-------------|-----------------|
| TNS packet read/write | `network/session.go` | `readPacket()`, `writePacket()`, packet header struct |
| Connect descriptor | `network/connect_option.go` | Connect string building/parsing |
| TTC AUTH username extraction | `auth_object.go` | AUTH key-value pair parsing |

Vendor the relevant protocol-level code from go-ora (MIT licensed) rather than importing the full driver.

---

## Implementation Steps & Tests

### Step 1: TNS Packet Reader/Writer

Build the low-level TNS packet framing layer: read 8-byte headers, extract packet type and length, read full payload, write packets back.

**Files:** `oracle/tns.go`, `oracle/tns_test.go`

**Key types:**

```go
type TNSPacketType byte

const (
    TNSPacketTypeConnect  TNSPacketType = 1
    TNSPacketTypeAccept   TNSPacketType = 2
    TNSPacketTypeRefuse   TNSPacketType = 3
    TNSPacketTypeRedirect TNSPacketType = 4
    TNSPacketTypeMarker   TNSPacketType = 5
    TNSPacketTypeData     TNSPacketType = 6
    TNSPacketTypeResend   TNSPacketType = 11
    TNSPacketTypeControl  TNSPacketType = 12
)

type TNSPacket struct {
    Type    TNSPacketType
    Length  uint16
    Payload []byte
}

func readTNSPacket(conn net.Conn) (*TNSPacket, error)
func writeTNSPacket(conn net.Conn, pkt *TNSPacket) error
func parseTNSHeader(raw []byte) (*TNSPacket, error)
func encodeTNSPacket(typ TNSPacketType, payload []byte) []byte
```

**Tests:**

```go
// --- Unit tests (pure bytes) ---

func TestTNSPacket_ParseHeader(t *testing.T) {
    raw := []byte{0x00, 0x2A, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00}
    pkt, err := parseTNSHeader(raw)
    require.NoError(t, err)
    assert.Equal(t, uint16(42), pkt.Length)
    assert.Equal(t, TNSPacketTypeData, pkt.Type)
}

func TestTNSPacket_ParseHeader_TooShort(t *testing.T) {
    _, err := parseTNSHeader([]byte{0x00, 0x0A})
    assert.ErrorIs(t, err, ErrTNSHeaderTooShort)
}

func TestTNSPacket_ParseHeader_AllTypes(t *testing.T) {
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
    raw := []byte{0x00, 0x08, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00}
    pkt, err := parseTNSHeader(raw)
    require.NoError(t, err)
    assert.Equal(t, TNSPacketType(0xFF), pkt.Type)
}

func TestTNSPacket_Encode(t *testing.T) {
    payload := []byte("hello")
    encoded := encodeTNSPacket(TNSPacketTypeData, payload)
    assert.Equal(t, 8+len(payload), len(encoded))
    assert.Equal(t, byte(0x00), encoded[0])
    assert.Equal(t, byte(0x0D), encoded[1])
    assert.Equal(t, byte(0x06), encoded[4])
    assert.Equal(t, payload, encoded[8:])
}

func TestTNSPacket_RoundTrip(t *testing.T) {
    original := TNSPacket{Type: TNSPacketTypeConnect, Payload: []byte("test-connect-data")}
    encoded := encodeTNSPacket(original.Type, original.Payload)
    parsed, err := parseTNSHeader(encoded[:8])
    require.NoError(t, err)
    assert.Equal(t, original.Type, parsed.Type)
    assert.Equal(t, uint16(len(encoded)), parsed.Length)
}

func TestTNSPacket_ZeroLengthPayload(t *testing.T) {
    encoded := encodeTNSPacket(TNSPacketTypeResend, nil)
    assert.Equal(t, 8, len(encoded))
}

func TestTNSPacket_MaxLength(t *testing.T) {
    payload := make([]byte, 32767-8) // SDU max
    encoded := encodeTNSPacket(TNSPacketTypeData, payload)
    parsed, err := parseTNSHeader(encoded[:8])
    require.NoError(t, err)
    assert.Equal(t, uint16(32767), parsed.Length)
}

// --- I/O tests (net.Pipe) ---

func TestTNSPacket_ReadFromConn(t *testing.T) {
    client, server := net.Pipe()
    defer client.Close()
    defer server.Close()

    go func() {
        raw := encodeTNSPacket(TNSPacketTypeConnect, []byte("connect-data"))
        client.Write(raw)
    }()

    pkt, err := readTNSPacket(server)
    require.NoError(t, err)
    assert.Equal(t, TNSPacketTypeConnect, pkt.Type)
    assert.Equal(t, []byte("connect-data"), pkt.Payload)
}

func TestTNSPacket_ReadFromConn_PartialHeader(t *testing.T) {
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
    client, server := net.Pipe()
    go func() {
        client.Write([]byte{0x00, 0x20})
        client.Close()
    }()

    _, err := readTNSPacket(server)
    assert.Error(t, err)
}

func TestTNSPacket_MultiplePackets(t *testing.T) {
    client, server := net.Pipe()
    defer client.Close()
    defer server.Close()

    types := []TNSPacketType{TNSPacketTypeConnect, TNSPacketTypeData, TNSPacketTypeMarker}
    go func() {
        for _, typ := range types {
            client.Write(encodeTNSPacket(typ, []byte(fmt.Sprintf("pkt-%d", typ))))
        }
    }()

    for _, expected := range types {
        pkt, err := readTNSPacket(server)
        require.NoError(t, err)
        assert.Equal(t, expected, pkt.Type)
    }
}

// --- Capture-based tests ---

func TestTNSPacket_ParseRealCapture_SQLPlusConnect(t *testing.T) {
    raw := loadCapture(t, "testdata/captures/sqlplus_connect.bin")
    pkt, err := parseTNSHeader(raw[:8])
    require.NoError(t, err)
    assert.Equal(t, TNSPacketTypeConnect, pkt.Type)
    assert.True(t, pkt.Length > 8)
}
```

---

### Step 2: Connect Descriptor Parser

Parse Oracle's parenthesized connect descriptor format to extract `SERVICE_NAME`, `SID`, and other metadata.

**Files:** `oracle/connect_descriptor.go`, `oracle/connect_descriptor_test.go`

**Key types:**

```go
type ConnectDescriptor struct {
    ServiceName string
    SID         string
    Host        string
    Port        int
    Program     string // From CID
    OSUser      string // From CID
}

func parseServiceName(descriptor string) string
func parseSID(descriptor string) string
func parseServiceNameEZConnect(descriptor string) string
func parseConnectDescriptor(descriptor string) ConnectDescriptor
func extractConnectDescriptor(tnsConnectPayload []byte) ConnectDescriptor
```

**Tests:**

```go
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
    desc := `(DESCRIPTION=
        (ADDRESS_LIST=
            (ADDRESS=(PROTOCOL=TCP)(HOST=rac1)(PORT=1521))
            (ADDRESS=(PROTOCOL=TCP)(HOST=rac2)(PORT=1521)))
        (CONNECT_DATA=(SERVICE_NAME=RAC_SVC)(FAILOVER_MODE=(TYPE=SELECT)(METHOD=BASIC))))`
    assert.Equal(t, "RAC_SVC", parseServiceName(desc))
}

func TestParseServiceName_EZConnect(t *testing.T) {
    assert.Equal(t, "ORCL", parseServiceNameEZConnect("db.example.com:1521/ORCL"))
}

func TestParseServiceName_EZConnect_NoPort(t *testing.T) {
    assert.Equal(t, "ORCL", parseServiceNameEZConnect("db.example.com/ORCL"))
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
    desc := `(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=OK)`
    cd := parseConnectDescriptor(desc)
    assert.Equal(t, "OK", cd.ServiceName)
}

func TestParseConnectDescriptor_RealCapture(t *testing.T) {
    raw := loadCapture(t, "testdata/captures/sqlplus_connect.bin")
    pkt, _ := readTNSPacketFromBytes(raw)
    desc := extractConnectDescriptor(pkt.Payload)
    assert.NotEmpty(t, desc.ServiceName)
}
```

---

### Step 3: OracleServer + OracleSession Skeleton

TCP listener that accepts connections, parses TNS Connect, looks up the database, connects to upstream, and relays raw bytes. No TTC inspection yet.

**Files:** `oracle/server.go`, `oracle/session.go`, `oracle/server_test.go`, `oracle/session_test.go`

**Key interfaces:**

```go
type OracleServer struct {
    listenAddr string
    listener   net.Listener
    store      *store.Store
    // ... same deps as PG Server
}

func NewOracleServer(addr string, s *store.Store, key []byte, logger *slog.Logger) *OracleServer
func (s *OracleServer) Start() error
func (s *OracleServer) Stop()
func (s *OracleServer) Addr() net.Addr
```

**Session lifecycle:**
1. `readTNSPacket` → expect Connect
2. `extractConnectDescriptor` → get `SERVICE_NAME`
3. `store.GetDatabaseByName` (or by `OracleServiceName`) → get upstream address
4. Connect to upstream Oracle via TCP
5. Forward TNS Connect to upstream, receive Accept/Refuse
6. Forward Accept/Refuse to client
7. Enter raw bidirectional relay loop

**Tests:**

```go
func TestOracleServer_StartsAndAcceptsConnections(t *testing.T) {
    srv := NewOracleServer(":0", nil, nil, slog.Default())
    go srv.Start()
    defer srv.Stop()

    conn, err := net.Dial("tcp", srv.Addr().String())
    require.NoError(t, err)
    defer conn.Close()
}

func TestOracleServer_GracefulShutdown(t *testing.T) {
    srv := NewOracleServer(":0", nil, nil, slog.Default())
    go srv.Start()

    conn, err := net.Dial("tcp", srv.Addr().String())
    require.NoError(t, err)
    srv.Stop()

    conn.SetReadDeadline(time.Now().Add(time.Second))
    _, err = conn.Read(make([]byte, 1))
    assert.Error(t, err)
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
            conn, _ := net.Dial("tcp", srv.Addr().String())
            if conn != nil { conn.Close() }
        }()
    }
    wg.Wait()
}

func TestOracleSession_RawRelay(t *testing.T) {
    upstream := newEchoTNSServer(t)
    defer upstream.Close()

    client, proxyEnd := net.Pipe()
    defer client.Close()
    defer proxyEnd.Close()

    session := &OracleSession{clientConn: proxyEnd}
    session.upstreamConn, _ = net.Dial("tcp", upstream.Addr().String())

    go session.relayRaw()

    pkt := encodeTNSPacket(TNSPacketTypeData, []byte("hello oracle"))
    client.Write(pkt)

    resp, err := readTNSPacket(client)
    require.NoError(t, err)
    assert.Equal(t, []byte("hello oracle"), resp.Payload)
}

func TestOracleSession_ConnectParsesServiceName(t *testing.T) {
    client, proxyEnd := net.Pipe()
    defer client.Close()

    session := &OracleSession{clientConn: proxyEnd}

    go func() {
        client.Write(encodeTNSPacket(TNSPacketTypeConnect, buildTNSConnect("TESTDB")))
    }()

    serviceName, err := session.receiveConnect()
    require.NoError(t, err)
    assert.Equal(t, "TESTDB", serviceName)
}

func TestOracleSession_ConnectUnknownDB_SendsRefuse(t *testing.T) {
    client, proxyEnd := net.Pipe()
    defer client.Close()

    mockStore := newMockStore()
    session := &OracleSession{clientConn: proxyEnd, store: mockStore}

    go func() {
        client.Write(encodeTNSPacket(TNSPacketTypeConnect, buildTNSConnect("UNKNOWN_DB")))
    }()

    go session.Run()

    pkt, err := readTNSPacket(client)
    require.NoError(t, err)
    assert.Equal(t, TNSPacketTypeRefuse, pkt.Type)
}
```

---

### Step 4: Auth Passthrough

Intercept TTC AUTH Phase 1 to extract username, check DBBat grants, then relay the full auth exchange to upstream.

**Files:** `oracle/auth.go`, `oracle/auth_test.go`

**Logic:**
1. After TNS Accept, messages switch to TTC inside TNS Data packets
2. First few exchanges are Set Protocol + Set Data Types → relay transparently
3. When we see TTC function code 0x5E (OAUTH): parse username from Phase 1
4. Look up user + grant in DBBat store, check quotas
5. If denied → send TNS Refuse, close
6. If allowed → relay the AUTH message to upstream, continue relaying until auth completes
7. Once upstream sends AUTH OK → create connection record, enter proxy mode

**Username extraction from TTC AUTH:**
The AUTH Phase 1 message contains key-value pairs. The username is sent as a null-terminated string early in the payload. go-ora's `auth_object.go` shows the exact encoding.

```go
func extractUsernameFromAuth(ttcPayload []byte) (string, error) {
    // Skip TTC header (data flags + function code)
    // Parse AUTH key-value pairs
    // Find the username field
    // Return decoded string
}
```

**Tests:**

```go
func TestExtractUsername_FromTTCAuth(t *testing.T) {
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
    raw := loadCapture(t, "testdata/captures/auth_phase1.bin")
    username, err := extractUsernameFromAuth(raw)
    require.NoError(t, err)
    assert.NotEmpty(t, username)
}

func TestAuthPassthrough_GrantExists_RelaysToUpstream(t *testing.T) {
    upstream := newFakeOracleAuth(t, "SCOTT", "tiger")
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

    go func() {
        client.Write(wrapInTNSData(buildTTCAuthPhase1("SCOTT")))
        readTNSPacket(client) // challenge
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

    pkt, _ := readTNSPacket(client)
    assert.Equal(t, TNSPacketTypeRefuse, pkt.Type)
}

func TestAuthPassthrough_UnknownUser_Refused(t *testing.T) {
    client, proxyEnd := net.Pipe()
    defer client.Close()

    mockStore := newMockStore()
    mockStore.AddDatabase("ORCL", "oracle", "localhost:1521")

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
    mockStore.AddGrantWithQuota("SCOTT", "ORCL", 100, 100)

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
        readTNSPacket(client)
        client.Write(wrapInTNSData(buildTTCAuthPhase2("wrong_password")))
        pkt, _ := readTNSPacket(client)
        assert.Contains(t, string(pkt.Payload), "ORA-01017")
    }()

    err := session.handleAuth()
    assert.Error(t, err)
}
```

---

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `internal/proxy/oracle/tns.go` | New | TNS packet reader/writer |
| `internal/proxy/oracle/tns_test.go` | New | TNS tests |
| `internal/proxy/oracle/connect_descriptor.go` | New | Connect descriptor parser |
| `internal/proxy/oracle/connect_descriptor_test.go` | New | Parser tests |
| `internal/proxy/oracle/server.go` | New | TCP listener |
| `internal/proxy/oracle/server_test.go` | New | Server tests |
| `internal/proxy/oracle/session.go` | New | Session lifecycle + raw relay |
| `internal/proxy/oracle/session_test.go` | New | Session tests |
| `internal/proxy/oracle/auth.go` | New | Auth passthrough |
| `internal/proxy/oracle/auth_test.go` | New | Auth tests |
| `internal/proxy/oracle/errors.go` | New | Error definitions |
| `internal/proxy/oracle/testdata/captures/` | New | Real packet captures for tests |
| `internal/store/models.go` | Modified | Add Protocol + OracleServiceName to Database |
| `internal/config/config.go` | Modified | Add ListenOracle |
| `internal/migrations/sql/YYYYMMDDHHMMSS_oracle_protocol.up.sql` | New | Migration |
| `internal/migrations/sql/YYYYMMDDHHMMSS_oracle_protocol.down.sql` | New | Rollback |
| `cmd/dbbat/main.go` | Modified | Start OracleServer alongside PG |

## Acceptance Criteria

1. `sqlplus SCOTT/tiger@//localhost:1522/TESTDB` connects through DBBat to an upstream Oracle 18c XE
2. Connection is logged in `connections` table with correct user/database UIDs
3. If no grant exists for SCOTT on TESTDB → connection refused with clear error
4. If grant quota is exceeded → connection refused
5. If upstream Oracle rejects password → client receives Oracle error
6. All TNS Data packets relay transparently after auth (raw pass-through)
7. `DBB_LISTEN_ORA=:1522` config works; disabled by default if empty
8. `Database` model supports `protocol=oracle` with `oracle_service_name`
9. Existing PG proxy is unaffected (no regressions)

## Estimated Size

~600 lines new Go code + ~400 lines ported from go-ora + ~100 lines refactored = **~1,100 lines total** (+ ~500 lines tests)
