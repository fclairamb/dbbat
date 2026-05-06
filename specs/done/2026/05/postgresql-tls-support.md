# PostgreSQL Proxy SSL/TLS Termination

## Goal

Make the PostgreSQL proxy accept TLS-upgraded client connections, terminating TLS at the proxy. Mirror the existing MySQL TLS pattern (`internal/proxy/mysql/tls.go`) so behavior, env vars, and operator UX are symmetric across the two protocols.

Out of scope: upstream TLS (proxy → real PostgreSQL). The `databases.ssl_mode` column already exists but is unused by `internal/proxy/postgresql/upstream.go`; that's a separate plan.

## Why this matters

The PostgreSQL proxy currently **refuses** SSL: when a client sends `SSLRequest` (the special 8-byte startup with code 80877103), the handler replies with the single byte `'N'` and the connection stays plaintext (`internal/proxy/postgresql/session.go:518-524`).

Consequences:

- Most production PG clients (psql, pgAdmin, JDBC, psycopg2, pgx) default to `sslmode=prefer` and silently downgrade to plaintext — a security footgun, not a feature.
- Credentials still travel cleartext between client and proxy: the proxy issues `AuthenticationCleartextPassword` (`internal/proxy/postgresql/auth.go`), so anyone on the wire between client and proxy sees the password.
- The MySQL proxy already supports termination via `DBB_MYSQL_TLS_*` (`internal/proxy/mysql/tls.go`, env mapping at `internal/config/config.go:363-366`). The asymmetry is surprising and undermines the "same pipeline across protocols" story in the project README.

Note on port: the proxy listens on `:5434` by default (`DBB_LISTEN_PG`), but operators often rebind it to `:5432` to look like real Postgres. SSL support is independent of port choice.

## Root cause / Current state

`internal/proxy/postgresql/session.go:507-525`:

```go
// Check for SSLRequest
if length == 8 {
    versionBuf := make([]byte, 4)
    if _, err := io.ReadFull(s.clientConn, versionBuf); err != nil {
        return nil, err
    }
    version := int(versionBuf[0])<<24 | ...

    const sslRequestCode = 80877103
    if version == sslRequestCode {
        // Deny SSL
        if _, err := s.clientConn.Write([]byte{'N'}); err != nil {
            return nil, fmt.Errorf("failed to deny SSL: %w", err)
        }
        return s.receiveStartupMessage()
    }
}
```

The negotiation byte itself is correctly detected — the code just hardcodes the rejection path. There is no TLS config plumbed through `Server` or `Session`, no `tls.Config`, and no listener-level wrapping.

The `Server` struct (`internal/proxy/postgresql/server.go:18-31`) and `NewServer` constructor (line 33) take no TLS-related parameters. `Session` (`session.go:67-93`) and `NewSession` (line 95) likewise.

## Fix approach

Mirror the MySQL TLS pattern. PG's TLS upgrade is simpler than MySQL's: no RSA fallback (PG has no `caching_sha2_password` analogue), so we only need a `*tls.Config`, no `*rsa.PrivateKey`.

### Files to change

#### 1. `internal/config/config.go` — new `PGConfig` block

The `TLSConfig` struct at line 153 is already generic and reusable.

Add:

```go
// PGConfig holds configuration specific to the PostgreSQL proxy.
type PGConfig struct {
    // TLS holds TLS server-termination settings for the proxy. When enabled,
    // the proxy responds 'S' to SSLRequest and terminates TLS at the proxy.
    TLS TLSConfig `koanf:"tls"`
}
```

Add to the main `Config` struct (next to `MySQL MySQLConfig` at line 232):

```go
// PG holds PostgreSQL proxy specific configuration.
PG PGConfig `koanf:"pg"`
```

Add an env-var transform rule next to the `mysql_tls_*` rule (`config.go:363-366`):

```go
// pg_tls_* -> pg.tls.*
if strings.HasPrefix(key, "pg_tls_") {
    return "pg.tls." + strings.TrimPrefix(key, "pg_tls_"), v
}
```

Yields `DBB_PG_TLS_DISABLE`, `DBB_PG_TLS_CERT_FILE`, `DBB_PG_TLS_KEY_FILE` — symmetric with MySQL.

#### 2. `internal/proxy/postgresql/tls.go` — new file

Adapt `internal/proxy/mysql/tls.go` (lines 1-138) without the RSA-key return:

```go
func loadTLS(cfg config.PGConfig) (*tls.Config, error)
func loadTLSFromFiles(certFile, keyFile string) (*tls.Config, error)
func generateSelfSignedTLS() (*tls.Config, error)
```

Behavior matches MySQL:

- `cfg.TLS.Disable == true` → return `(nil, nil)`. Caller treats `nil` as "TLS off".
- Both `CertFile` and `KeyFile` set → load from disk via `tls.LoadX509KeyPair`.
- Both empty → auto-generate self-signed RSA-2048 (10-year validity, CN `dbbat-pg-proxy`, SAN `localhost`).
- Only one set → return `ErrTLSConfigInvalid`.

Reuse `MinVersion: tls.VersionTLS12` and the same x509 template shape from `mysql/tls.go:99-138`. Skip `parseRSAFromPEM` — not needed for PG.

#### 3. `internal/proxy/postgresql/server.go` — store the TLS config

- Add `tlsConfig *tls.Config` to the `Server` struct (after line 26).
- Add `tlsConfig *tls.Config` parameter to `NewServer(...)` (line 33). Match MySQL's call-site convention; loading the cert can happen either in `main.go` before the `NewServer` call or inside `NewServer` itself — pick whichever matches MySQL's choice (it loads inside `NewServer`).
- Pass `s.tlsConfig` into each `NewSession` call (line 140).

The listener stays plaintext — TLS is negotiated mid-connection per the PG protocol, so no `tls.NewListener` wrapping. This is the same pattern MySQL uses (raw listener, library upgrades the connection after `SSLRequest`).

Update the startup log line to include `slog.Bool("tls", s.tlsConfig != nil)`, mirroring MySQL.

#### 4. `internal/proxy/postgresql/session.go` — accept `'S'` and upgrade

Two related changes:

**(a) Thread the TLS config through.** Add `tlsConfig *tls.Config` to the `Session` struct (after line 77) and to `NewSession` (line 95).

**(b) Restructure `Run()` so SSL negotiation happens before the backend is built.** `Run()` currently does `s.clientBackend = pgproto3.NewBackend(s.clientConn, s.clientConn)` at line 127 *before* `authenticate()`. Wrapping `s.clientConn` with `tls.Server()` would invalidate that backend's reader/writer. Cleanest split: extract a small `negotiateSSL()` step called from `Run()` *before* `pgproto3.NewBackend` is constructed:

```go
func (s *Session) Run() error {
    defer s.cleanup()

    if err := s.negotiateSSL(); err != nil {
        return err
    }
    s.clientBackend = pgproto3.NewBackend(s.clientConn, s.clientConn)

    if err := s.authenticate(); err != nil { ... }
    ...
}
```

`negotiateSSL` reads the first 4 bytes of length, peeks for the `SSLRequest` code, and either:

- (TLS enabled, request seen) writes `'S'`, wraps `s.clientConn = tls.Server(s.clientConn, s.tlsConfig)`, runs `HandshakeContext(s.ctx)`. The next StartupMessage is read via the backend after this returns.
- (TLS disabled, request seen) writes `'N'`, leaves `s.clientConn` as plaintext. Existing behavior preserved.
- (no SSLRequest) puts the 4 bytes back via a small bufio peek-or-prepend helper, or reorganize so `receiveStartupMessage` consumes from a `bufio.Reader` set on the session. The simplest correct approach is to wrap `s.clientConn` in a `bufio.Reader` once, do the peek/decision against the buffered reader, and then build the backend on `(bufReader, s.clientConn)`.

After `negotiateSSL` returns, the existing code path in `receiveStartupMessage` no longer needs the SSL branch — drop lines 507-526 from `receiveStartupMessage`, since negotiation has already happened.

#### 5. `main.go` — plumb the config through

At the call site that constructs the PG proxy (`postgresql.NewServer(...)` near line 329), pass `cfg.PG` (or the loaded `*tls.Config`, depending on where `loadTLS` is called — match MySQL's convention at line 409). Add a `slog.Bool("tls", !cfg.PG.TLS.Disable)` field to the startup log line, mirroring MySQL at lines 422-424.

#### 6. `internal/proxy/postgresql/tls_test.go` — new test file

Mirror `internal/proxy/mysql/tls_test.go`:

- `TestLoadTLS_Disabled` — `Disable:true` returns `nil, nil`.
- `TestLoadTLS_AutoGenerated` — generated cert is parseable, `tls.Config.Certificates` non-empty, key is RSA.
- `TestLoadTLS_PartialPathsRejected` — only one of cert/key set returns `ErrTLSConfigInvalid`.

#### 7. `CLAUDE.md` — document the new env vars

Add three rows to the env-var table next to the `DBB_MYSQL_TLS_*` rows:

| Variable | Description | Required |
|----------|-------------|----------|
| `DBB_PG_TLS_DISABLE` | Refuse TLS upgrade on the PostgreSQL listener (default: `false`) | No |
| `DBB_PG_TLS_CERT_FILE` | PEM cert for PG TLS termination (auto self-signed if empty) | No |
| `DBB_PG_TLS_KEY_FILE` | PEM key for PG TLS termination (auto-generated if empty) | No |

#### 8. `docs/postgresql.md` (or section in existing PG doc)

Add a brief section mirroring `docs/mysql.md:49-65`: termination model, env vars, auto-generation behavior, production guidance to provide a real cert. Note that upstream is still plaintext until the separate upstream-TLS work lands.

## Acceptance criteria

- [ ] `psql "host=localhost port=5434 user=admin sslmode=require"` succeeds; `\conninfo` reports `SSL connection (...)`.
- [ ] `psql "host=localhost port=5434 user=admin sslmode=disable"` still succeeds (plaintext path unchanged).
- [ ] `psql "host=localhost port=5434 sslmode=verify-full sslrootcert=cert.pem ..."` succeeds when pointed at the proxy's own certificate.
- [ ] `DBB_PG_TLS_DISABLE=true` — proxy still responds `'N'` to `SSLRequest`; existing behavior preserved.
- [ ] `DBB_PG_TLS_CERT_FILE` set without `DBB_PG_TLS_KEY_FILE` (and vice versa) → server fails to start with `ErrTLSConfigInvalid`.
- [ ] Auto-generation path: server starts cleanly with no TLS env vars set, accepts `sslmode=require` clients.
- [ ] Startup log line includes `tls=true` (or `tls=false` when disabled).
- [ ] Unit tests added in `internal/proxy/postgresql/tls_test.go` covering disabled / auto-generated / partial-paths.
- [ ] `make test` and `make lint` pass.
- [ ] `CLAUDE.md` env-var table updated.
- [ ] `docs/postgresql.md` (or equivalent) describes the new behavior.

## Implementation notes

- **Why not wrap the listener with `tls.NewListener`?** The PG protocol does TLS negotiation *after* the connection is open: the client first sends `SSLRequest` over plaintext, the server replies `'S'` or `'N'`, and only then does the TLS handshake start. Wrapping the listener would break this — clients that don't send `SSLRequest` (or that ask for it and expect `'N'`) would see TLS handshake failures instead of clean plaintext. MySQL takes the same approach for the same reason.
- **Existing `TLSConfig` struct (`config.go:153`) is already generic** — no need to copy it under a different name. Just embed it under `PGConfig.TLS`.
- **No RSA-key plumbing needed for PG.** `internal/proxy/mysql/tls.go` returns `(*tls.Config, *rsa.PrivateKey, error)` because MySQL's `caching_sha2_password` flow needs the RSA key for non-TLS public-key retrieval. PG has no such flow.
- **`bufio.Reader` for peek-style SSL detection.** The simplest restructure is: in `Session`, hold a `clientReader io.Reader` (a `bufio.Reader` over `clientConn`). `negotiateSSL` reads from it. After SSL upgrade, replace `clientConn` with the TLS conn and reset the reader. Build the backend on `(clientReader, clientConn)`. This is cleaner than threading the 4-byte length read through `receiveStartupMessage`.
- **`HandshakeContext` over `Handshake`** so a misbehaving client can't hang the goroutine indefinitely. `s.ctx` is already on the session.
- **Self-signed cert validity: 10 years.** Match MySQL exactly. CN `dbbat-pg-proxy`, SAN `localhost`. Operators wanting verify-full should provide their own cert.

## Out of scope (separate plans)

- **Upstream TLS** (proxy → real PostgreSQL). The `databases.ssl_mode` column exists in the store but is unused by `internal/proxy/postgresql/upstream.go:20-47` — it dials plaintext via `net.Dial("tcp", addr)`. Wiring `ssl_mode` is a different change set; depends on upstream cert verification policy decisions.
- **Client certificate auth** (`tls.Config.ClientAuth = RequireAndVerifyClientCert`). MySQL doesn't do this either; can be added later symmetrically.
- **`GSSENCRequest` (code 80877104).** A second negotiation byte for GSSAPI encryption. Currently also unhandled (the proxy will read its 8 bytes as an unknown startup and fail). Can stay rejected; rare in practice.
- **Cert hot-reload.** MySQL doesn't reload either. Restart the process to pick up new certs.

## Verification

1. **Unit tests:** `make test` — new `internal/proxy/postgresql/tls_test.go` passes. Confirm no regressions in `internal/proxy/postgresql/*_test.go`.
2. **Lint:** `make lint`.
3. **Manual TLS handshake (auto-cert path):**
   - `make dev`.
   - `psql "host=localhost port=5434 user=admin sslmode=require"` → succeeds, `\conninfo` shows `SSL connection (protocol: TLSv1.3, cipher: ..., bits: 256)`.
   - `psql "host=localhost port=5434 user=admin sslmode=disable"` → still succeeds.
4. **Manual TLS with provided cert:**
   - `openssl req -x509 -newkey rsa:2048 -nodes -keyout key.pem -out cert.pem -days 365 -subj /CN=localhost -addext subjectAltName=DNS:localhost`
   - `DBB_PG_TLS_CERT_FILE=$(pwd)/cert.pem DBB_PG_TLS_KEY_FILE=$(pwd)/key.pem make dev`
   - `psql "host=localhost port=5434 sslmode=verify-full sslrootcert=$(pwd)/cert.pem user=admin"` → succeeds with full cert verification.
5. **Disabled path:** `DBB_PG_TLS_DISABLE=true make dev` → `psql sslmode=require` is refused with `server does not support SSL, but SSL was required`.
6. **Logs check:** `logs/` should show `"PostgreSQL proxy server started ... tls=true"`. (Per CLAUDE.md / project memory: always check `logs/` when debugging dbbat.)
7. **Cross-check with MySQL behavior:** confirm operator UX is symmetric — same env-var prefix style, same auto-generation default, same disable semantics.

## Implementation Plan

1. **Config** — add `PGConfig` block to `internal/config/config.go` with embedded `TLSConfig`. Wire into the main `Config` struct as `PG PGConfig`. Add the env-var transform rule `pg_tls_*` → `pg.tls.*` next to the MySQL one.
2. **TLS loader** — create `internal/proxy/postgresql/tls.go` adapted from `mysql/tls.go` minus the RSA fallback. Three functions: `loadTLS`, `loadTLSFromFiles`, `generateSelfSignedTLS`. Same self-signed shape (RSA-2048, 10y validity, CN `dbbat-pg-proxy`, SAN `localhost`). Define `ErrTLSConfigInvalid`.
3. **Server wiring** — add `tlsConfig *tls.Config` to `Server` struct, take `cfg config.PGConfig` in `NewServer` and call `loadTLS` inside. Pass `tlsConfig` into `NewSession`. Update startup log line to include `tls=true/false`.
4. **Session SSL negotiation** — extract `negotiateSSL()` from `receiveStartupMessage`. Use `bufio.Reader` over `clientConn` so the 4-byte length peek can be undone for non-SSLRequest startups. After `negotiateSSL`, build the backend on the (possibly upgraded) reader/conn pair. Drop the SSL branch from `receiveStartupMessage`.
5. **Plumb config through** — update `main.go` to pass `cfg.PG` into `postgresql.NewServer`, log `tls` field on startup.
6. **Tests** — add `internal/proxy/postgresql/tls_test.go` mirroring `mysql/tls_test.go`: disabled / auto-generated / partial-paths.
7. **Docs** — add `DBB_PG_TLS_*` rows to `CLAUDE.md` env-var table. Update `docs/postgresql.md` (or create) with the new TLS section.
8. **QA** — `make test`, `make lint`, `make build-binary`. Manual psql `sslmode=require` smoke check.
