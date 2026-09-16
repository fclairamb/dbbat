# PostgreSQL Proxy

The PostgreSQL proxy speaks the [PostgreSQL wire protocol v3](https://www.postgresql.org/docs/current/protocol.html) using `jackc/pgx/v5/pgproto3`. Clients connect to DBBat as if it were a real PostgreSQL server; DBBat terminates the client connection, authenticates against the DBBat user store, and proxies queries to the configured upstream database.

## TLS Handling: Termination at the Proxy

DBBat **terminates TLS** at the proxy. When a client sends an `SSLRequest` startup packet (the special 8-byte preamble with version `80877103`), the proxy responds:

- `'S'` if TLS is configured — followed by a TLS handshake on the same socket.
- `'N'` if TLS is disabled — the connection stays plaintext (existing behavior).

After the optional TLS upgrade, the client sends its real `StartupMessage` and the auth flow continues inside the (now-encrypted) tunnel.

Configuration (env vars, all optional):

| Var | Description |
|-----|-------------|
| `DBB_PG_TLS_DISABLE` | When `true`, the proxy refuses `SSLRequest` and stays plaintext-only. Default `false`. |
| `DBB_PG_TLS_CERT_FILE` | Path to PEM-encoded server cert. |
| `DBB_PG_TLS_KEY_FILE` | Path to PEM-encoded server key. |

If both cert/key paths are empty (and TLS isn't disabled), the proxy auto-generates a self-signed RSA-2048 certificate at startup (`CN=dbbat-pg-proxy`, `SAN=localhost`, 10-year validity) — fine for dev, but use a real cert in production.

If only one of cert/key is set, the proxy fails to start with `ErrTLSConfigInvalid`. This is intentional: half-configured TLS is almost always a mistake.

The TLS upgrade is **mid-connection**, not at the listener level — that's how PG works. The listener stays raw TCP; `pgproto3` is built on top of the (possibly upgraded) `net.Conn` after `negotiateSSL` runs. This is the same pattern the MySQL proxy uses.

### Upstream TLS

The proxy→upstream leg is encrypted independently of the client→proxy leg, driven by the `servers.ssl_mode` column. `upstream.ConnectPostgres` (`internal/proxy/upstream/postgres.go`) dials (directly, or through an SSH bastion when `via_uid` is set) and negotiates SSL **before** the `StartupMessage` — Postgres expects the 8-byte `SSLRequest` preamble on a fresh connection, not interleaved with protocol traffic.

That connector is also what the connectivity check runs, so a green check proves the proxy's own login path got in rather than that a second implementation could. `internal/proxy/postgresql/upstream.go` keeps only the half that needs a downstream client: replaying the server's startup state, the read-only `SET`, and the cancel-key registration.

Semantics mirror libpq's `sslmode`, encoded once in `upstream.PlanFor`:

| `ssl_mode` | Probe sent | Upstream answers `'S'` | Upstream answers `'N'` |
|-----------|-----------|------------------------|------------------------|
| `disable` | no | — (stays plaintext) | — |
| `allow`, `prefer`, empty | yes | TLS, **certificate not verified** | continue plaintext |
| `require` | yes | TLS, **certificate not verified** | fail (`ErrUpstreamTLSRequired`) |
| `verify-ca`, `verify-full` | yes | TLS with full chain **and** hostname verification | fail (`ErrUpstreamTLSRequired`) |

Details:

- **`verify-ca` is treated as `verify-full`.** Go's stdlib doesn't cleanly express "verify the CA but not the hostname", so both modes verify the hostname too — stricter than libpq, but safer.
- **`ServerName`** for the verifying modes is the server row's `host` value, and the system root pool is used (no per-server CA bundle yet).
- **TLS 1.2 is the floor** (`MinVersion: tls.VersionTLS12`); non-verifying modes set `InsecureSkipVerify` to get libpq-parity encryption-without-authentication.
- Any response byte other than `'S'` or `'N'` fails with `ErrUpstreamSSLResponse`.
- The default for new server rows is `prefer`, which means an upstream that declines TLS **silently downgrades to plaintext** — use `require` or better when the upstream link matters.
- **What actually happened is recorded.** Because `prefer` is a preference rather than a guarantee, each session stores the outcome in `connections.upstream_tls`.
- **No SCRAM channel binding upstream.** `internal/proxy/upstream/postgres_scram.go` sends the `n,,` gs2 header (client doesn't support channel binding), so `SCRAM-SHA-256-PLUS` is never selected even inside a TLS tunnel — binding to a certificate we deliberately don't verify would silently upgrade `require`'s "encrypt only" guarantee. An upstream offering *only* `SCRAM-SHA-256-PLUS` fails with `ErrSCRAMNoSupportedMechanism`.

### Operator notes

- `psql sslmode=require` works against an auto-generated cert.
- `psql sslmode=verify-full` requires `sslrootcert=` pointing to the proxy's cert. With auto-generation, fetch the cert via `openssl s_client -connect host:port -starttls postgres -showcerts`, or provide your own via `DBB_PG_TLS_CERT_FILE`.
- `sslmode=prefer` (the libpq default) silently downgrades to plaintext if the server replies `'N'` — set `DBB_PG_TLS_DISABLE=true` to keep that legacy behavior.
- Cert hot-reload is **not** supported. Restart the process to pick up new certs.

## Authentication

DBBat sends `AuthenticationCleartextPassword` (`R`) to the client. Inside a TLS tunnel this is safe; over plaintext the password travels in the clear, which is why TLS support exists.

Both DBBat user passwords (Argon2id) and DBBat API keys (prefix `dbb_`) are accepted as the password. API key verification is independent of the user password path.

## Finding a session in pg_stat_activity

Every proxied session's `application_name` is dbbat-branded and carries the
connection's own uid, not just the dbbat user's:

```sql
SELECT pid, application_name, state, query
FROM pg_stat_activity
WHERE application_name LIKE 'dbbat/%';
```

`application_name` reads `dbbat/0.28.1 @florent c=3f9a1c7b2e4d for psql` — the
`c=` tag is the last 12 hex characters of the connection uid. Paste it (or the
whole `c=...` token) into the connections page's search box, or call
`GET /api/v1/connections?uid_suffix=3f9a1c7b2e4d` directly, to land on the
exact dbbat connection: its queries, its grant, and the Terminate button —
rather than guessing from the username alone, which is ambiguous the moment a
user has more than one session open.

## Testing

### Integration tests

`internal/proxy/postgresql/integration_test.go` sits behind the `integration` build tag, so `make test` never runs it. CI runs `go vet -tags integration ./...`, which only proves it compiles — run it for real before trusting it:

```bash
# needs Docker; starts a PostgreSQL upstream + a PostgreSQL container for dbbat's own store
go test -tags integration -timeout 40m ./internal/proxy/postgresql/

# run the same matrix against another server version
PG_TEST_IMAGE=postgres:17 go test -tags integration -timeout 40m ./internal/proxy/postgresql/
```

| Variable | Purpose |
|----------|---------|
| `PG_TEST_IMAGE` | Upstream PostgreSQL image the proxy connects to (default `postgres:16-alpine`) |
| `DBBAT_STORE_TEST_IMAGE` | Image backing dbbat's own store (default `postgres:15-alpine`) |

The suite dials **through** the proxy with `jackc/pgx/v5` and covers password / `dbb_` API-key / wrong-password auth, `sslmode=require` (proxy-terminated TLS) and `sslmode=disable`, upstream TLS (`ssl_mode` `require` / `disable` / `verify-full` against a TLS-enabled upstream container, asserted via `pg_stat_ssl`), refusal of an unknown database name, simple-protocol query + result-row capture, extended-protocol (Parse/Bind/Execute) bind-parameter capture, the `read_only`, `block_ddl` and `block_copy` grant controls, per-session `.pcapng` captures, and mid-session grant revocation tearing the connection down. Both default images have arm64 builds, so it runs unmodified on Apple Silicon (verified on 2026-07-21).

## Per-statement time limits

PostgreSQL is the one protocol where both layers are comfortable.

**Layer 1**, the server-side setting: `SET SESSION statement_timeout = <ms>` is
issued in `replayUpstreamStartup`, in the same batch as the read-only pin
(`runUpstreamSetup`), before the client is told it is connected. A session that
cannot be pinned fails rather than running unbounded. The client then gets a
real `ERROR: canceling statement due to statement timeout` with SQLSTATE
`57014`, and **the session survives** — which is the whole reason this layer
exists.

A statement that would unset or change it is refused through `validateStatement`
(so the simple and extended paths cannot drift): `SET [SESSION|LOCAL]
statement_timeout …`, `RESET statement_timeout`, and `RESET ALL`. Every `SET` is
refused rather than only the widening ones — parsing PostgreSQL's unit grammar
(`500`, `500ms`, `5s`, `1min`, `DEFAULT`) to allow a narrowing one would be a
new place to be wrong about a security-relevant value, and a client wanting less
time can cancel its own query. `SHOW statement_timeout` stays allowed. The
matcher is comment-normalized, like every other check here:
`SET/**/statement_timeout=0` reaches the server as a plain `SET`.

**Layer 2**, the watchdog: `LimitGuard` trips `ErrStatementTimeout` once the
oldest in-flight statement passes `limit + 2s`. Several statements can be in
flight at once under the extended protocol, so the clock holds the *oldest*
start (`extendedState.pendingQueries`) and is re-armed from the next pending one
on completion. A `COPY` in progress is a statement.

On a trip dbbat sends a **`CancelRequest`** carrying the upstream's
`BackendKeyData` before closing anything. It goes on a *fresh* connection — the
backend running the query is not reading its socket — dialed through the same
`shared.DialUpstream` path the session used, so an SSH bastion or a Kubernetes
port-forward is honoured. This is not optional politeness: a backend in a long
sequential scan does not notice a dead client until it next tries to send, and
`client_connection_check_interval` defaults to `0`, so without the cancel the
scan runs to completion.
