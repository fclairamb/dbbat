# Oracle Auth Termination with O5LOGON

## Goal

Restructure the Oracle proxy so that dbbat **terminates authentication** on both sides independently — acting as an Oracle server toward the client and as an Oracle client toward the upstream database. Clients authenticate to dbbat using their API key as the Oracle password. dbbat authenticates to upstream Oracle using the stored (encrypted) database credentials. The client never sees upstream credentials.

This replaces the current broken auth passthrough (disabled for TNS v315+).

## Prerequisites

- `api-key-proxy-auth.md` (API keys accepted as proxy credentials — PG first)

## Outcome

- Oracle clients authenticate to dbbat with username + API key (as Oracle password)
- dbbat implements O5LOGON server-side auth to verify the API key
- After client auth, dbbat connects to upstream Oracle with stored DB credentials
- Grant checks, quota enforcement, and access control work like PostgreSQL
- The placeholder `s.username = "proxy"` workaround is removed

## Non-Goals

- O7LOGON (SHA-512/PBKDF2) support — O5LOGON (SHA-1, verifier type 6949) covers all Oracle 11g+ clients
- NNE (Native Network Encryption) support
- Oracle password changes through the proxy
- Supporting Oracle OCI (thick) clients — thin clients only (JDBC thin, python-oracledb thin, go-ora, sqlplus)

---

## Architecture

### New Connection Flow

```
Client                          dbbat (Oracle proxy)                 Upstream Oracle
  |                                |                                      |
  | 1. TNS Connect(SERVICE_NAME)   |                                      |
  |------------------------------->|                                      |
  |                                | Parse SERVICE_NAME                   |
  |                                | Look up database                     |
  |                                |                                      |
  | 2. TNS Accept                  |                                      |
  |<-------------------------------| (dbbat crafts Accept)                |
  |                                |                                      |
  | 3. Set Protocol                |                                      |
  |------------------------------->|                                      |
  | 4. Set Protocol Response       |                                      |
  |<-------------------------------| (dbbat responds)                     |
  |                                |                                      |
  | 5. Set Data Types              |                                      |
  |------------------------------->|                                      |
  | 6. Set Data Types Response     |                                      |
  |<-------------------------------| (dbbat responds)                     |
  |                                |                                      |
  | 7. AUTH Phase 1 (username)     |                                      |
  |------------------------------->|                                      |
  |                                | Look up dbbat user                   |
  |                                | Check grant + quotas                 |
  |                                | Load API key O5LOGON verifier        |
  |                                |                                      |
  | 8. AUTH Challenge              |                                      |
  |<-------------------------------| (O5LOGON: sesskey + salt)            |
  |                                |                                      |
  | 9. AUTH Phase 2 (enc password) |                                      |
  |------------------------------->|                                      |
  |                                | Decrypt → API key                    |
  |                                | Verify via store.VerifyAPIKey()      |
  |                                |                                      |
  | 10. AUTH OK                    |                                      |
  |<-------------------------------|                                      |
  |                                |                                      |
  |                                | 11. Connect to upstream Oracle       |
  |                                |------------------------------------>|
  |                                | 12. TNS Connect (stored creds)       |
  |                                |------------------------------------>|
  |                                | 13. TNS Accept                       |
  |                                |<------------------------------------|
  |                                | 14. Set Protocol + Data Types        |
  |                                |<----------------------------------->|
  |                                | 15. AUTH with stored DB password     |
  |                                |<----------------------------------->|
  |                                | 16. AUTH OK                          |
  |                                |<------------------------------------|
  |                                |                                      |
  |         === Bidirectional relay (post-auth) ===                       |
```

### Key Architectural Decisions

1. **Client auth happens BEFORE upstream connection** — no upstream resources consumed for unauthorized clients
2. **O5LOGON verifier type 6949** (SHA-1 based) — maximum compatibility with Oracle 11g+ clients
3. **TTC negotiation is handled by dbbat** — Set Protocol and Set Data Types responses are hardcoded templates captured from a real Oracle server
4. **Upstream auth uses adapted go-ora code** — the client-side O5LOGON is already implemented in go-ora (MIT)

---

## O5LOGON Protocol (Server-Side)

### Verifier Generation (at API key creation time)

```
salt         = random(10 bytes)
verifier_key = SHA1(api_key_plaintext || salt), zero-padded to 24 bytes
```

Store `salt` and `verifier_key` (encrypted with dbbat master key) in `api_keys` table.

### Auth Challenge (server → client)

```
1. Generate server_session_key = random(48 bytes)
2. Derive encryption key from verifier_key (first 24 bytes of SHA1(verifier_key))
3. Encrypt server_session_key with AES-192-CBC using derived key
4. Send TTC AUTH response containing:
   - AUTH_SESSKEY = hex(encrypted_server_session_key)
   - AUTH_VFR_DATA = hex(salt) + "6949" (verifier type suffix)
```

### Password Decryption (from client's AUTH Phase 2)

```
1. Receive client's AUTH_SESSKEY (encrypted client session key)
2. Decrypt client_session_key using verifier_key
3. Derive combined_key = MD5(server_session_key || client_session_key)
4. Decrypt AUTH_PASSWORD using AES-192-CBC with combined_key (padded to 24 bytes)
5. Strip 16-byte random prefix → plaintext password (= API key)
6. Verify via store.VerifyAPIKey(plaintext)
```

### Reference Implementation

The go-ora library (`github.com/sijms/go-ora/v2/auth_object.go`) implements the client side. The server side is the mirror: where the client decrypts the server session key, the server encrypts it; where the client encrypts the password, the server decrypts it.

---

## Database Schema Changes

### Migration: `YYYYMMDDHHMMSS_api_key_o5logon.up.sql`

```sql
ALTER TABLE api_keys ADD COLUMN o5logon_salt BYTEA;
ALTER TABLE api_keys ADD COLUMN o5logon_verifier BYTEA;

--bun:split

-- Backfill note: existing API keys won't have O5LOGON verifiers.
-- They'll work for REST API and PG proxy but not Oracle proxy.
-- Users must regenerate keys to get Oracle support.
-- Test mode stable keys (from api-key-proxy-auth spec) will be
-- updated to include O5LOGON verifiers via CreateAPIKeyWithValue.
```

### Migration: `YYYYMMDDHHMMSS_api_key_o5logon.down.sql`

```sql
ALTER TABLE api_keys DROP COLUMN o5logon_verifier;
ALTER TABLE api_keys DROP COLUMN o5logon_salt;
```

### Model Changes

```go
// internal/store/models.go — APIKey struct
type APIKey struct {
    // ... existing fields ...
    O5LogonSalt     []byte `bun:"o5logon_salt"     json:"-"`
    O5LogonVerifier []byte `bun:"o5logon_verifier" json:"-"` // encrypted with dbbat master key
}
```

---

## Implementation Steps

### Step 1: O5LOGON Crypto Core

**File:** `internal/proxy/oracle/o5logon.go` (~250 lines)

```go
// O5LogonServer implements server-side O5LOGON authentication.
type O5LogonServer struct {
    salt             []byte // 10-byte salt (from stored verifier)
    verifierKey      []byte // 24-byte key derived from password+salt
    serverSessionKey []byte // 48-byte random (generated per auth)
    clientSessionKey []byte // 48-byte (received from client)
    combinedKey      []byte // derived from both session keys
}

// NewO5LogonServer creates a server from stored verifier data.
func NewO5LogonServer(salt, verifierKey []byte) *O5LogonServer

// GenerateChallenge produces the AUTH_SESSKEY and AUTH_VFR_DATA for the client.
func (s *O5LogonServer) GenerateChallenge() (encServerSessKey string, authVfrData string, error)

// DecryptPassword extracts the plaintext password from the client's AUTH Phase 2.
func (s *O5LogonServer) DecryptPassword(encClientSessKey, encPassword string) (string, error)

// --- Verifier generation (called at API key creation time) ---

// GenerateO5LogonVerifier creates salt + verifier key from a plaintext password.
func GenerateO5LogonVerifier(password string) (salt, verifierKey []byte, error)
```

**File:** `internal/proxy/oracle/o5logon_test.go` (~200 lines)

```go
func TestO5Logon_RoundTrip(t *testing.T) {
    // Generate verifier from known password
    password := "dbb_test1234abcdefghijklmnopqrs"
    salt, verifierKey, err := GenerateO5LogonVerifier(password)
    require.NoError(t, err)

    // Server generates challenge
    server := NewO5LogonServer(salt, verifierKey)
    encSessKey, vfrData, err := server.GenerateChallenge()
    require.NoError(t, err)

    // Simulate client response (using go-ora crypto as reference)
    clientEncSessKey, clientEncPassword := simulateO5LogonClient(password, encSessKey, vfrData)

    // Server decrypts password
    decrypted, err := server.DecryptPassword(clientEncSessKey, clientEncPassword)
    require.NoError(t, err)
    assert.Equal(t, password, decrypted)
}

func TestO5Logon_WrongPassword(t *testing.T) {
    salt, verifierKey, _ := GenerateO5LogonVerifier("correct_password")
    server := NewO5LogonServer(salt, verifierKey)
    encSessKey, vfrData, _ := server.GenerateChallenge()

    // Client uses wrong password
    clientEncSessKey, clientEncPassword := simulateO5LogonClient("wrong_password", encSessKey, vfrData)

    decrypted, err := server.DecryptPassword(clientEncSessKey, clientEncPassword)
    // Either error or decrypted != "correct_password"
    assert.True(t, err != nil || decrypted != "correct_password")
}

func TestO5Logon_VerifierGeneration(t *testing.T) {
    salt, verifier, err := GenerateO5LogonVerifier("test_password")
    require.NoError(t, err)
    assert.Len(t, salt, 10)
    assert.Len(t, verifier, 24) // SHA-1 output zero-padded to 24 bytes
}
```

---

### Step 2: TTC Auth Message Construction

**File:** `internal/proxy/oracle/ttc_auth.go` (~200 lines)

Handles TTC-level message building and parsing for the auth exchange.

```go
// parseAuthPhase1 extracts username and key-value pairs from AUTH Phase 1.
func parseAuthPhase1(payload []byte) (username string, kvPairs map[string]string, err error)

// buildAuthChallenge constructs the AUTH challenge TTC message.
// Contains AUTH_SESSKEY (encrypted server session key) and AUTH_VFR_DATA (salt + verifier type).
func buildAuthChallenge(encServerSessKey, authVfrData string) []byte

// parseAuthPhase2 extracts client session key and encrypted password from AUTH Phase 2.
func parseAuthPhase2(payload []byte) (clientSessKey, encPassword string, err error)

// buildAuthOK constructs the AUTH success response TTC message.
func buildAuthOK() []byte

// buildAuthFailed constructs the AUTH failure response with ORA error code.
func buildAuthFailed(oraCode int, message string) []byte
```

**File:** `internal/proxy/oracle/ttc_negotiate.go` (~100 lines)

Hardcoded responses for TTC negotiation (captured from a real Oracle server).

```go
// buildTNSAccept crafts a TNS Accept packet for the client.
// Based on captured real Accept packets, with configurable SDU size.
func buildTNSAccept(clientVersion uint16) []byte

// buildSetProtocolResponse returns a hardcoded Set Protocol response.
func buildSetProtocolResponse() []byte

// buildSetDataTypesResponse returns a hardcoded Set Data Types response.
func buildSetDataTypesResponse() []byte
```

---

### Step 3: Upstream Client-Side Auth

**File:** `internal/proxy/oracle/upstream_auth.go` (~300 lines)

dbbat acting as an Oracle client to authenticate to upstream.

```go
// upstreamAuth performs full Oracle authentication to the upstream server.
// This handles: TNS Connect, Accept, Set Protocol, Set Data Types, O5LOGON auth.
func (s *session) upstreamAuth() error

// upstreamConnect sends TNS Connect with the database's service name and handles Accept.
func (s *session) upstreamConnect() error

// upstreamNegotiate handles Set Protocol and Set Data Types with upstream.
func (s *session) upstreamNegotiate() error

// upstreamO5Logon performs client-side O5LOGON auth with upstream using stored credentials.
// Adapted from go-ora auth_object.go (MIT licensed).
func (s *session) upstreamO5Logon() error
```

---

### Step 4: Session Restructure

**File:** `internal/proxy/oracle/session.go` (significant rewrite of `run()`)

```go
func (s *session) run() error {
    // Phase 1: Client connection (same as today)
    connectPkt := s.receiveConnect()        // TNS Connect from client
    s.serviceName = s.parseServiceName(connectPkt)
    s.database = s.lookupDatabase(s.serviceName)

    // Phase 2: Client TTC negotiation (NEW — dbbat acts as Oracle server)
    s.sendAccept()                          // TNS Accept to client
    s.handleSetProtocol()                   // Set Protocol exchange
    s.handleSetDataTypes()                  // Set Data Types exchange

    // Phase 3: Client authentication (NEW — O5LOGON server-side)
    if err := s.authenticateClient(); err != nil {
        s.sendRefuse(ORA01017, "authentication failed")
        return err
    }

    // Phase 4: Upstream connection (NEW — dbbat acts as Oracle client)
    if err := s.upstreamAuth(); err != nil {
        return fmt.Errorf("upstream auth failed: %w", err)
    }

    // Phase 5: Record connection, init dump, enter relay (same as today)
    s.createConnectionRecord()
    s.initDumpWriter()
    return s.proxyMessages()
}

func (s *session) authenticateClient() error {
    // Receive AUTH Phase 1 → extract username
    username, kvPairs := parseAuthPhase1(clientPayload)
    
    // Look up dbbat user
    user, err := s.store.GetUserByUsername(s.ctx, username)
    // Check grant
    grant, err := s.store.GetActiveGrant(s.ctx, user.UID, s.database.UID)
    // Check quotas
    
    // Load API key O5LOGON verifier for this user
    // (pick the first valid API key with an O5LOGON verifier)
    verifier := s.loadO5LogonVerifier(user.UID)
    
    // Generate challenge
    o5 := NewO5LogonServer(verifier.Salt, verifier.VerifierKey)
    encSessKey, vfrData := o5.GenerateChallenge()
    s.sendAuthChallenge(encSessKey, vfrData)
    
    // Receive AUTH Phase 2 → decrypt password (= API key)
    clientSessKey, encPassword := parseAuthPhase2(phase2Payload)
    plainPassword := o5.DecryptPassword(clientSessKey, encPassword)
    
    // Verify the decrypted password as an API key
    apiKey, err := s.store.VerifyAPIKey(s.ctx, plainPassword)
    if apiKey.UserID != user.UID {
        return ErrAPIKeyOwnerMismatch
    }
    
    // Send AUTH OK
    s.sendAuthOK()
    
    s.user = user
    s.grant = grant
    s.authenticated = true
    return nil
}
```

---

### Step 5: API Key Verifier Storage

**File:** `internal/store/api_keys.go`

Update `CreateAPIKey()` to compute and store O5LOGON verifier:

```go
func (s *Store) CreateAPIKey(ctx context.Context, userID uuid.UUID, name string, expiresAt *time.Time) (*APIKey, string, error) {
    plainKey, prefix, err := generateAPIKey()
    // ... existing hash generation ...

    // NEW: Generate O5LOGON verifier
    salt, verifierKey, err := oracle.GenerateO5LogonVerifier(plainKey)
    if err != nil {
        return nil, "", fmt.Errorf("failed to generate O5LOGON verifier: %w", err)
    }

    // Encrypt verifier key with dbbat master key
    encVerifier, err := crypto.Encrypt(verifierKey, s.encryptionKey, crypto.APIKeyAAD(prefix))
    if err != nil {
        return nil, "", fmt.Errorf("failed to encrypt O5LOGON verifier: %w", err)
    }

    apiKey := &APIKey{
        // ... existing fields ...
        O5LogonSalt:     salt,
        O5LogonVerifier: encVerifier,
    }
    // ... insert ...
}
```

---

## Tests

### Integration Test: Full Oracle Auth Flow

```go
func TestOracleSession_APIKeyAuth_Success(t *testing.T) {
    // Setup
    store := setupTestStore(t)
    user := createTestUser(t, store, "orauser")
    db := createTestOracleDatabase(t, store, "TESTDB", upstreamAddr)
    createTestGrant(t, store, user.UID, db.UID)
    _, plainKey, _ := store.CreateAPIKey(ctx, user.UID, "ora-key", nil)

    // Connect to dbbat Oracle proxy
    // Use go-ora client with username="orauser", password=plainKey, service="TESTDB"
    conn, err := goora.NewConnection(fmt.Sprintf(
        "oracle://orauser:%s@localhost:%d/TESTDB", plainKey, proxyPort))
    require.NoError(t, err)
    defer conn.Close()
}

func TestOracleSession_APIKeyAuth_NoGrant(t *testing.T) {
    store := setupTestStore(t)
    user := createTestUser(t, store, "nograntuser")
    createTestOracleDatabase(t, store, "TESTDB", upstreamAddr)
    _, plainKey, _ := store.CreateAPIKey(ctx, user.UID, "no-grant-key", nil)

    conn, err := goora.NewConnection(fmt.Sprintf(
        "oracle://nograntuser:%s@localhost:%d/TESTDB", plainKey, proxyPort))
    assert.Error(t, err) // Should get ORA-01017 or connection refused
}

func TestOracleSession_APIKeyAuth_WrongKey(t *testing.T) {
    store := setupTestStore(t)
    user := createTestUser(t, store, "wrongkeyuser")
    db := createTestOracleDatabase(t, store, "TESTDB", upstreamAddr)
    createTestGrant(t, store, user.UID, db.UID)
    store.CreateAPIKey(ctx, user.UID, "real-key", nil)

    conn, err := goora.NewConnection(fmt.Sprintf(
        "oracle://wrongkeyuser:dbb_not_the_right_key_at_all@localhost:%d/TESTDB", proxyPort))
    assert.Error(t, err) // Auth should fail
}
```

---

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `internal/proxy/oracle/o5logon.go` | New | Server-side O5LOGON crypto (~250 lines) |
| `internal/proxy/oracle/o5logon_test.go` | New | O5LOGON round-trip tests (~200 lines) |
| `internal/proxy/oracle/ttc_auth.go` | New | TTC AUTH message build/parse (~200 lines) |
| `internal/proxy/oracle/ttc_auth_test.go` | New | TTC AUTH message tests (~100 lines) |
| `internal/proxy/oracle/ttc_negotiate.go` | New | Hardcoded Set Protocol/Data Types responses (~100 lines) |
| `internal/proxy/oracle/upstream_auth.go` | New | Client-side upstream Oracle auth (~300 lines) |
| `internal/proxy/oracle/upstream_auth_test.go` | New | Upstream auth tests (~100 lines) |
| `internal/proxy/oracle/session.go` | Modified | Restructure `run()` for terminated auth |
| `internal/proxy/oracle/auth.go` | Modified | Replace passthrough with O5LOGON server auth |
| `internal/store/models.go` | Modified | Add O5LOGON fields to APIKey |
| `internal/store/api_keys.go` | Modified | Compute O5LOGON verifier at key creation |
| `internal/migrations/sql/YYYYMMDDHHMMSS_api_key_o5logon.up.sql` | New | Add O5LOGON columns |
| `internal/migrations/sql/YYYYMMDDHHMMSS_api_key_o5logon.down.sql` | New | Rollback |

## Acceptance Criteria

1. `python-oracledb` (thin) connecting as `orauser/dbb_xxx@//localhost:1522/TESTDB` authenticates through dbbat to upstream Oracle
2. The upstream Oracle session uses the stored database credentials (not the API key)
3. If no grant exists → ORA-01017 authentication error
4. If API key is revoked/expired → authentication error
5. If API key belongs to a different user → authentication error
6. Quota enforcement works on Oracle connections
7. Connection records are created with correct user/database UIDs
8. Query interception and access controls (read_only, block_ddl) work after auth
9. JDBC thin, python-oracledb thin, and go-ora clients all work
10. Existing PG proxy is unaffected

## Estimated Size

~1,250 lines new Go code + ~600 lines tests = **~1,850 lines total**

## Implementation Plan

### Step 1: Database Migration
- Add `o5logon_salt` and `o5logon_verifier` columns to `api_keys` table
- Create up/down migration files
- Add fields to `APIKey` model in `internal/store/models.go`

### Step 2: O5LOGON Crypto Core
- Create `internal/proxy/oracle/o5logon.go` with server-side O5LOGON implementation
- `GenerateO5LogonVerifier(password)` — creates salt + verifier key
- `NewO5LogonServer(salt, verifierKey)` — creates server from stored data
- `GenerateChallenge()` — produces AUTH_SESSKEY + AUTH_VFR_DATA
- `DecryptPassword(encClientSessKey, encPassword)` — extracts plaintext password
- Create `o5logon_test.go` with round-trip tests

### Step 3: TTC Auth Message Construction
- Create `internal/proxy/oracle/ttc_auth.go` for AUTH message build/parse
- `parseAuthPhase1()` — extract username and key-value pairs
- `buildAuthChallenge()` — construct AUTH challenge message
- `parseAuthPhase2()` — extract client session key and encrypted password
- `buildAuthOK()` / `buildAuthFailed()` — auth result messages

### Step 4: TTC Negotiation Responses
- Create `internal/proxy/oracle/ttc_negotiate.go`
- `buildTNSAccept()` — craft Accept packet for client
- `buildSetProtocolResponse()` — hardcoded Set Protocol response
- `buildSetDataTypesResponse()` — hardcoded Set Data Types response

### Step 5: Upstream Client-Side Auth
- Create `internal/proxy/oracle/upstream_auth.go`
- `upstreamAuth()` — full upstream Oracle auth sequence
- `upstreamConnect()` — TNS Connect with stored credentials
- `upstreamNegotiate()` — Set Protocol + Data Types exchange
- `upstreamO5Logon()` — client-side O5LOGON auth

### Step 6: Session Restructure
- Rewrite `session.run()` to terminate auth on both sides
- Phase 1: Receive client Connect, parse service name, lookup database
- Phase 2: Send Accept, handle Set Protocol/Data Types (dbbat as server)
- Phase 3: Client O5LOGON auth (server-side)
- Phase 4: Upstream connection + auth (client-side)
- Phase 5: Bidirectional relay (existing)

### Step 7: API Key Verifier Storage
- Update `CreateAPIKey()` and `CreateAPIKeyWithValue()` to compute O5LOGON verifiers
- Encrypt verifier with dbbat master key
- Add `GetAPIKeyO5LogonVerifier()` to retrieve verifier for a user

### Step 8: QA
- Build, lint, test — fix any issues

## Risks

1. **TTC message format**: Different Oracle client drivers may send slightly different TTC structures. Mitigation: capture real packets from sqlplus, JDBC, and python-oracledb; make parsing tolerant.
2. **TNS Accept crafting**: The Accept packet must be valid enough for all clients. Mitigation: use a real captured Accept as template.
3. **Set Data Types**: Large static response (~200 bytes). Mitigation: capture from real Oracle and replay verbatim.
4. **O5LOGON crypto correctness**: Must match what Oracle clients expect. Mitigation: extensive round-trip tests with go-ora as the reference client implementation.
5. **Multi-key ambiguity**: `loadO5LogonVerifier()` picks the first valid API key with an O5LOGON verifier. O5LOGON is a challenge-response protocol — the challenge is derived from one specific key's verifier, so only that key's plaintext will decrypt correctly. If a user has multiple API keys, only the first one (by list order) works for Oracle auth. The others will fail with a garbled decryption. Mitigation: document this behavior; in practice users will have one active API key. A future enhancement could include a key-hint mechanism (e.g., key prefix in the connect descriptor).

---

## Implementation Status (updated 2026-04-15)

### Code: COMPLETE — Activation: PENDING

All code described in this spec has been implemented and merged. Every function is present but annotated with `//nolint:unused` because `session.run()` still uses **passthrough mode** (forwards auth directly to upstream Oracle).

**What works today (passthrough mode):**
- Client connects using real Oracle credentials (upstream user/password)
- SERVICE_NAME rewrite works (e.g., `abyla_glh` → `MUTU01`)
- Query interception and logging work
- No per-user access control — the proxy picks the first active grant for the database

**What's missing to activate terminated auth:**
- Switch `session.run()` from passthrough to the terminated auth flow (the code exists, it just needs to replace the current passthrough block)
- Validate hardcoded TTC negotiation responses with real Oracle thin clients (python-oracledb, JDBC thin, go-ora)
- Verify that API keys created after the O5LOGON migration have valid verifiers

**Files with unused code ready for activation:**
- `session.go`: `handleClientNegotiation()`, `authenticateClient()`, `loadO5LogonVerifier()`
- `upstream_auth.go`: `upstreamAuth()`, `upstreamConnect()`, `upstreamNegotiate()`, `upstreamO5Logon()`
- `ttc_negotiate.go`: `buildTNSAccept()`, `buildSetProtocolResponse()`, `buildSetDataTypesResponse()`
- `ttc_auth.go`: all parse/build functions
- `crypto_util.go`: encrypt/decrypt verifier helpers
