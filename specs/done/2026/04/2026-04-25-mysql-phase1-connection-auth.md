# MySQL Proxy — Phase 1: Connection & Authentication

> Parent spec: `2026-04-25-mysql-proxy.md`

## Goal

Accept MySQL client connections on `DBB_LISTEN_MYSQL` (default `:3307`), perform DBBat-side authentication using `mysql_native_password`, then establish an authenticated upstream connection to the target MySQL database.

After this phase, `mysql -h dbbat-host -P 3307 -u dev -p mydb` succeeds end-to-end. No query logging or interception yet — Phase 2.

## Prerequisites

None. This is the first phase.

## Outcome

### Files added
```
internal/proxy/mysql/server.go        # TCP listener, lifecycle, dump cleanup
internal/proxy/mysql/session.go       # Per-connection session state
internal/proxy/mysql/auth.go          # DBBat-side auth (user store, API keys, grant check)
internal/proxy/mysql/upstream.go      # Outbound connection to upstream MySQL
internal/proxy/mysql/errors.go        # Sentinel errors
internal/proxy/mysql/server_test.go
internal/proxy/mysql/auth_test.go
internal/migrations/sql/20260425000000_drop_databases_port_default.up.sql
internal/migrations/sql/20260425000000_drop_databases_port_default.down.sql
```

### Files modified
```
go.mod / go.sum                       # add github.com/go-mysql-org/go-mysql
internal/store/models.go              # add ProtocolMySQL constant
internal/config/config.go             # add ListenMySQL field + default :3307
main.go                               # wire startMySQLProxy()
internal/api/databases.go             # validate port presence (no SQL default)
internal/api/openapi.yml              # protocol enum gains mysql (this file gets the API spec piece)
```

## Architecture

```
Client (mysql)              DBBat (proxy)                Upstream MySQL
     │                           │                              │
     │  TCP connect              │                              │
     │──────────────────────────>│                              │
     │                           │                              │
     │  Handshake v10            │                              │
     │  (server greeting,        │                              │
     │   advertise native_pw)    │                              │
     │<──────────────────────────│                              │
     │                           │                              │
     │  HandshakeResponse41      │                              │
     │  (user, db, scrambled_pw) │                              │
     │──────────────────────────>│                              │
     │                           │                              │
     │                           │ 1. Look up user             │
     │                           │ 2. Look up database         │
     │                           │    by database_name where   │
     │                           │    protocol='mysql'         │
     │                           │ 3. Check active grant       │
     │                           │ 4. Check quotas             │
     │                           │ 5. Verify scrambled pw      │
     │                           │    against user.pw_hash     │
     │                           │    OR verify as API key     │
     │                           │                              │
     │                           │  Decrypt db creds            │
     │                           │  Dial upstream               │
     │                           │─────────────────────────────>│
     │                           │  Handshake + auth (using     │
     │                           │  upstream's preferred plugin)│
     │                           │<────────────────────────────>│
     │                           │                              │
     │  OK packet                │                              │
     │<──────────────────────────│                              │
     │                           │                              │
     │  ===  raw byte relay (Phase 2 will intercept) ===       │
```

## Library

```
github.com/go-mysql-org/go-mysql v1.x
```

Subpackages:
- `github.com/go-mysql-org/go-mysql/server` — server-side framing, handshake helpers, `Conn` type
- `github.com/go-mysql-org/go-mysql/client` — client-side connection to upstream
- `github.com/go-mysql-org/go-mysql/mysql` — wire-protocol types (capabilities, command codes, OK/ERR/EOF packets)

The `server` package exposes a `Handler` interface for handling commands. For Phase 1 we only need the handshake; we'll set up a minimal handler that passes everything through and refine it in Phase 2.

## Database Schema

### Migration: `20260425000000_drop_databases_port_default.up.sql`
```sql
ALTER TABLE databases ALTER COLUMN port DROP DEFAULT;
```

### Down
```sql
ALTER TABLE databases ALTER COLUMN port SET DEFAULT 5432;
```

Existing rows are unaffected (their `port` values stay). New rows must specify a port. The API layer is updated to require `port` in create requests, with protocol-aware error messages suggesting 5432/1521/3306.

## Store changes

```go
// internal/store/models.go
const (
    ProtocolPostgreSQL = "postgresql"
    ProtocolOracle     = "oracle"
    ProtocolMySQL      = "mysql"  // ← new
)
```

No new table, no new column. `Database.Protocol` already accepts arbitrary strings; the API layer enforces the enum.

## Config changes

```go
// internal/config/config.go
type Config struct {
    // ...existing fields...
    ListenMySQL string `koanf:"listen_mysql"`  // empty = disabled, default ":3307"
}

// Default
ListenMySQL: ":3307",
```

CLI flag passthrough not added in Phase 1 (matches Oracle — no `--listen-mysql` CLI flag, only the env var). Can be added later if needed.

## Server skeleton

```go
// internal/proxy/mysql/server.go
package mysql

type Server struct {
    store         *store.Store
    encryptionKey []byte
    queryStorage  config.QueryStorageConfig
    dumpConfig    config.DumpConfig
    authCache     *cache.AuthCache
    logger        *slog.Logger
    listener      net.Listener
    wg            sync.WaitGroup
    shutdown      chan struct{}
    ctx           context.Context
    cancel        context.CancelFunc
}

func NewServer(...) *Server { ... }
func (s *Server) Start(addr string) error { ... }    // mirror PG/Oracle Start()
func (s *Server) Shutdown(ctx context.Context) error { ... }
func (s *Server) handleConnection(c net.Conn) { ... } // dispatches to Session.Run()
```

## Session lifecycle

```go
// internal/proxy/mysql/session.go
package mysql

type Session struct {
    clientConn   net.Conn
    upstreamConn *client.Conn  // go-mysql client connection to upstream

    store         *store.Store
    encryptionKey []byte
    logger        *slog.Logger
    ctx           context.Context

    user       *store.User
    database   *store.Database
    grant      *store.Grant
    connection *store.Connection  // dbbat connections row

    // wrapped server-side conn (from go-mysql/server) once handshake succeeds
    serverConn *server.Conn
}

func NewSession(...) *Session { ... }

func (s *Session) Run() error {
    if err := s.authenticate(); err != nil {
        // close client connection with ErrPacket
        return err
    }
    if err := s.connectUpstream(); err != nil {
        return err
    }
    if err := s.recordConnection(); err != nil {
        return err
    }
    defer s.recordDisconnect()

    return s.proxyLoop()  // Phase 1: raw relay; Phase 2 replaces with intercept
}
```

## Authentication

```go
// internal/proxy/mysql/auth.go
func (s *Session) authenticate() error {
    // 1. Generate random scramble (20 bytes for native_password)
    // 2. Send Handshake v10:
    //    - protocol_version: 10
    //    - server_version: "8.0.0-dbbat-" + version
    //    - connection_id: random uint32
    //    - auth_plugin_data: scramble
    //    - capability_flags: CLIENT_PROTOCOL_41 | CLIENT_SECURE_CONNECTION |
    //                       CLIENT_PLUGIN_AUTH | CLIENT_CONNECT_WITH_DB
    //    - auth_plugin_name: "mysql_native_password"
    // 3. Receive HandshakeResponse41:
    //    - username, database, auth_response (20 bytes scrambled)
    //    - if SSL_REQUEST flag set → reject with ER_NOT_SUPPORTED_AUTH_MODE
    //
    // 4. Look up DBBat user; refuse if not found
    // 5. Look up DBBat database by database_name where protocol='mysql'
    // 6. Check active grant; check quotas
    // 7. Verify auth_response:
    //    a. If client wants public-key challenge for caching_sha2 → send AuthSwitchRequest
    //       to mysql_native_password (forces fallback)
    //    b. Compute expected: SHA1(password) XOR SHA1(scramble + SHA1(SHA1(password)))
    //       — but we have the Argon2id hash, not the password. So we must support
    //       cleartext fallback OR require the client to use mysql_clear_password OR
    //       use API key auth.
    //
    //       DECISION: send AuthSwitchRequest to "mysql_clear_password" after the
    //       initial handshake. The client returns the password in cleartext, and
    //       we verify against Argon2id. This requires CLIENT_PLUGIN_AUTH support
    //       (universal in modern clients). Document the decision: clients see
    //       a cleartext-password prompt, but the connection remains plaintext-only
    //       per our TLS scope decision (deploy on private network).
    //
    //    c. Alternatively: if we want to verify a SHA1 challenge, we'd need to
    //       store SHA1(SHA1(password)) for each user — a second password derivation
    //       just for MySQL. NOT doing this in v1.
    //
    // 8. If password starts with API key prefix: verify via VerifyAPIKey instead
    // 9. Send OK packet
}
```

### Auth plugin: cleartext after initial native_password offer

This is a meaningful sub-decision. We have three options for how to verify the password:

| Option | How | Pros | Cons |
|--------|-----|------|------|
| **A.** Initial offer `mysql_clear_password`, accept cleartext, verify against Argon2id | Simplest. Standard MySQL pattern for PAM/LDAP backends. | Works with all clients. Compatible with API keys. | Cleartext on the wire (mitigated by private network deployment per TLS decision). |
| **B.** Offer `mysql_native_password`, verify SHA1 challenge | Matches MySQL default. | No cleartext on wire. | Requires storing `SHA1(SHA1(pw))` per user — a second hash. Doesn't work for API keys (we'd need to also pre-compute their SHA1 form). |
| **C.** Offer native, then `AuthSwitchRequest` to clear | Hybrid. | Same UX as B for clients that prefer it. | Same downsides as A; complexity. |

**Decision: Option A.** Advertise `mysql_clear_password` from the start. This is the same model PostgreSQL uses (`AuthenticationCleartextPassword`). The `docs/mysql.md` already documents the plaintext-on-wire constraint and the deploy-on-private-network requirement.

The Phase 1 handshake therefore looks like:
1. Server sends Handshake v10 with `auth_plugin_name = "mysql_clear_password"` AND scramble (scramble is unused but still sent for protocol conformance).
2. Client sends HandshakeResponse41 with the password in cleartext (20 byte scramble field is replaced by the cleartext password bytes).
3. Server verifies via Argon2id (or as API key).

If a client refuses `mysql_clear_password` (most clients require an opt-in flag like `--enable-cleartext-plugin`), we document the fix per client.

## Upstream connection

```go
// internal/proxy/mysql/upstream.go
func (s *Session) connectUpstream() error {
    if err := s.database.DecryptPassword(s.encryptionKey); err != nil {
        return err
    }

    addr := net.JoinHostPort(s.database.Host, strconv.Itoa(s.database.Port))

    // go-mysql client.Connect handles handshake + auth with the upstream's
    // preferred plugin (native, caching_sha2, sha256). We pass the stored creds.
    conn, err := client.Connect(
        addr,
        s.database.Username,
        s.database.Password,
        s.database.DatabaseName,
        func(c *client.Conn) error {
            c.SetCapability(mysql.CLIENT_FOUND_ROWS)
            // SSL mode: honor s.database.SSLMode
            switch s.database.SSLMode {
            case "require", "verify-ca", "verify-full":
                c.UseSSL(s.database.SSLMode == "require") // insecure-skip-verify if "require"
            }
            // Application identifier: connection attribute
            c.SetAttributes(map[string]string{
                "program_name": "dbbat-" + version.Version,
            })
            return nil
        },
    )
    if err != nil {
        return fmt.Errorf("upstream connect: %w", err)
    }

    s.upstreamConn = conn
    return nil
}
```

## Connection record

After successful upstream connect, insert a row into `connections` with `user_id`, `database_id`, `source_ip`. On disconnect, set `disconnected_at`. Mirror PG's `recordConnection` / `recordDisconnect`.

## main.go wiring

Mirror `startOracleProxy`:

```go
func startMySQLProxy(ctx context.Context, cfg *config.Config, dataStore *store.Store,
    authCache *cache.AuthCache, logger *slog.Logger) (*mysql.Server, error) {
    if cfg.ListenMySQL == "" {
        return nil, nil
    }
    srv := mysql.NewServer(dataStore, cfg.EncryptionKey, cfg.QueryStorage,
        cfg.DumpConfig, authCache, logger)
    go func() {
        if err := srv.Start(cfg.ListenMySQL); err != nil {
            logger.ErrorContext(ctx, "MySQL proxy server error", slog.Any("error", err))
        }
    }()
    logger.InfoContext(ctx, "MySQL proxy server started", slog.String("addr", cfg.ListenMySQL))
    return srv, nil
}
```

And in `runServer`:
```go
mysqlServer, err := startMySQLProxy(ctx, cfg, dataStore, proxyAuthCache, logger)
if err != nil { return err }
if mysqlServer != nil { servers = append(servers, mysqlServer) }
```

## Proxy loop (Phase 1: raw relay)

For Phase 1 only, after auth succeeds, we run a bidirectional `io.Copy` between client and upstream connections. This proves the connection works end-to-end with no interception. Phase 2 replaces this with a command-aware loop.

```go
func (s *Session) proxyLoop() error {
    errCh := make(chan error, 2)
    go func() { _, err := io.Copy(s.clientConn, s.upstreamConn.Conn); errCh <- err }()
    go func() { _, err := io.Copy(s.upstreamConn.Conn, s.clientConn); errCh <- err }()
    return <-errCh
}
```

> Note: `client.Conn` from go-mysql wraps the raw `net.Conn`; we expose it for relay. This works for Phase 1 because we don't yet need to inspect packets. Phase 2 replaces this with a proper `Handler` implementation that intercepts each `COM_*` command.

## Tests

### Unit tests (no upstream)
- `auth_test.go`:
  - `TestAuthenticate_UserNotFound` — refuses with ER_ACCESS_DENIED
  - `TestAuthenticate_NoActiveGrant` — refuses
  - `TestAuthenticate_QuotaExceeded` — refuses
  - `TestAuthenticate_BadPassword` — refuses
  - `TestAuthenticate_GoodPassword` — sends OK
  - `TestAuthenticate_APIKey` — accepts API key
  - `TestAuthenticate_RefuseSSLRequest` — sends ER_NOT_SUPPORTED_AUTH_MODE
- `server_test.go`:
  - `TestServer_StartShutdown` — basic lifecycle

### Integration tests (testcontainers MySQL 8.4)
- `integration_test.go`:
  - `TestEndToEnd_HandshakeOnly` — go-sql-driver client `db.Ping()` succeeds through proxy
  - `TestEndToEnd_NoGrant_Refused` — connection rejected with auth error

## Verification checklist

- [ ] `make lint` clean
- [ ] `make test` passes (unit tests)
- [ ] `make test ./internal/proxy/mysql/...` passes integration test
- [ ] `make build-binary` produces `dbbat` binary
- [ ] Manual: `mysql --enable-cleartext-plugin -h 127.0.0.1 -P 3307 -u dev -p testdb` connects, runs `SELECT 1`, disconnects cleanly
- [ ] Logs show successful handshake, grant check, upstream connect

## Out of scope for Phase 1

- Query logging (Phase 2)
- Read-only enforcement (Phase 2)
- Result row capture (Phase 3)
- TLS / SSL Request acceptance (v2)
- caching_sha2_password (v2)
- Frontend changes (separate UI spec)
- OpenAPI changes (separate API spec)
