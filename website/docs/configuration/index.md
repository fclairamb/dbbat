---
sidebar_position: 1
---

# Configuration Overview

DBBat is configured via environment variables, an optional configuration file (YAML/JSON/TOML), or CLI flags.

## Priority Order

Configuration is loaded in this priority order (highest wins):

1. CLI flags
2. Environment variables (`DBB_…`)
3. Configuration file (`--config`, or `DBB_CONFIG=` env var)
4. Built-in defaults

## Environment Variables

### Required

| Variable | Description |
|----------|-------------|
| `DBB_DSN` | PostgreSQL DSN for DBBat's own storage (users, grants, queries, audit, …) |

### Listeners

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_LISTEN_PG` | PostgreSQL proxy listen address | `:5433` |
| `DBB_LISTEN_ORA` | Oracle proxy listen address. Empty value disables the Oracle proxy. | `:1522` |
| `DBB_LISTEN_MYSQL` | MySQL/MariaDB proxy listen address. Empty value disables it. | `:3307` |
| `DBB_LISTEN_MONGO` | MongoDB proxy listen address. Empty value disables it. | `:27018` |
| `DBB_LISTEN_MSSQL` | Microsoft SQL Server (TDS) proxy listen address. Empty value disables it. | `:1434` |
| `DBB_LISTEN_API` | REST API + web UI listen address | `:4200` |

`:1434` looks like it should collide with SQL Server, but it does not: the SQL
Server Browser service that owns port 1434 is **UDP-only**, so 1434/tcp is free
even on a host already running SQL Server.

### Encryption Key

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_KEY` | Base64-encoded 32-byte AES-256 key | Auto-generated |
| `DBB_KEYFILE` | Path to a file containing the encryption key | - |

If neither is set, DBBat generates a key on first start and writes it to `~/.dbbat/key` (mode `0600`, parent dir `0700`). Losing this key means the encrypted database credentials cannot be recovered.

### Run Mode & Logging

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_RUN_MODE` | `` (production), `test`, or `demo` | `` |
| `DBB_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `DBB_INSTANCE_ID` | Identifies this process among the replicas sharing a store | Hostname (the pod name under Kubernetes) |
| `DBB_BASE_URL` | Base URL path the frontend is served under | `/app` |
| `DBB_REDIRECTS` | Dev-only redirect rules (`/path:host:port[/target]`, comma-separated) | - |
| `DBB_DEMO_TARGET_DB` | Demo-mode allowed target (`user:pass@host/dbname`) | `demo:demo@localhost/demo` |

`DBB_INSTANCE_ID` is stamped on every connection dbbat records, alongside a
**run id** — a UUID the process mints in memory at every start, which is not
configurable. Both identify the process in the `instances` registry, which is
keyed by the pair: each run registers itself at startup, refreshes its
`last_seen_at` every **30 seconds**, and deletes its row on a clean shutdown.
The run id is what makes the identity trustworthy — an instance id is unique per
process only by convention, a run id by construction.

At startup, and before any proxy accepts, dbbat marks as disconnected every
connection left open by a run that is no longer running — a crash or a
`SIGKILL` never runs the normal teardown, so those rows would otherwise stay
"open" forever and never become eligible for retention. Two kinds are closed:

- **Its own**: connections left by a *previous run* carrying this instance id.
  Logged at `info`; a large number means an earlier run did not shut down
  cleanly.
- **Reclaimed**: connections owned by any other run that is provably gone — it
  deleted its registry row on a clean shutdown, or has not heartbeated for
  **15 minutes** (30 missed heartbeats). Logged separately, also at `info`: a
  non-zero count means some process died without shutting down.

Both kinds are liveness-checked, so neither closes anything a heartbeating run
still owns. A run that crashed moments before this one started therefore looks
alive at startup and is *not* reclaimed then — its rows are picked up by a later
reclaim pass, once its registry row goes stale.

Sessions are closed at their last activity time, so retention still measures
from when the session actually stopped talking.

The **reclaim** half does not only run at startup: every running process
re-runs it roughly every **7.5 minutes** (half the grace period, jittered so
replicas do not all sweep at once). Without that, the commonest crash would go
unnoticed for as long as the deployment stayed up — a `SIGKILL`ed pod leaves a
registry row seconds old, so its replacement sees a live-looking predecessor and
reclaims nothing, and 15 minutes later, when the row finally goes stale, there
is no restart left to look. That pass excludes the *current run*, not the
current instance id, so it also reclaims a previous run of this same id — a
stable id (a StatefulSet, or a pinned `DBB_INSTANCE_ID`) does not keep its own
crashed run's rows open until the next restart. The own half stays at startup
because that is the only moment its count means "the run I replaced".

Liveness, not identity, is what makes this safe when several replicas share one
store: a starting replica must never close a *live* connection belonging to a
different replica, because such a row immediately becomes eligible for the
retention sweep. The grace period is deliberately generous — a running replica
would have to fail every heartbeat for a quarter of an hour while still serving
traffic before anything touched its sessions.

A plain Kubernetes Deployment, which mints a new pod name on every restart, is
therefore handled as well as a StatefulSet or an explicit `DBB_INSTANCE_ID`: the
replacement pod does not recognise its predecessor's id, but it can see that the
predecessor stopped heartbeating.

:::tip
`DBB_INSTANCE_ID` should be **unique per running process**, though nothing
breaks if it is not. Two live replicas sharing an id cannot close each other's
sessions — the reconcile keys on the run id, which no configuration can make
them share — but they do answer to one identity in the logs and in the UI, and
the "left open by a previous run" count then covers every run of that id rather
than the process reporting it. A process that detects a live peer under its own
id logs a warning once, a heartbeat after it starts. The default (the hostname)
is already unique; there is no reason to pin it.
:::

Connections recorded before instance tracking existed carry an empty instance
id; they have no owner and never will, so they are reclaimed the same way. Those
recorded before run tracking carry no run id: they are judged by their instance
id alone, which is the rule the build that wrote them was playing by, so a
replica that is still serving them through an upgrade keeps them.

:::caution Upgrading from v0.20.x
v0.20.x is the only released build that predates run tracking: its heartbeat
upserts the registry row with `ON CONFLICT (instance_id)`. Once a later build's
migration changes the registry's primary key to `(instance_id, run_id)`, that
conflict target no longer exists, so a v0.20.x replica's heartbeats start
failing and its row stops moving. After the 15-minute grace period above, a
new-build replica treats it as dead and reclaims the connections it is still
serving — and a reclaimed connection immediately becomes eligible for deletion
by the retention sweep, even while the v0.20.x replica is still writing queries
against it.

This only bites a multi-replica deployment (`replicaCount > 1`) where a
v0.20.x replica keeps serving for more than 15 minutes after the migration
runs — a single-replica deployment is never affected. **Complete the upgrade
from v0.20.x within 15 minutes, and do not roll back to v0.20.x once you have
migrated.** Nothing is broken by running the migration itself; this is a
rollout-window caveat, not a live bug.
:::

### Session Packet Dumps

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_DUMP_DIR` | Directory for `.pcapng` session captures. Empty = disabled. | _disabled_ |
| `DBB_DUMP_MAX_SIZE` | Max dump file size per session, in bytes | `10485760` (10 MB) |
| `DBB_DUMP_RETENTION` | Auto-delete dumps older than this (Go duration). Local captures only. | `24h` |
| `DBB_DUMP_UPLOAD_URL` | Blob bucket finished captures are uploaded to on session close (`s3://bucket/prefix`, `file://…`). Empty = local disk only. Requires `DBB_DUMP_DIR`. | _disabled_ |

See [Session Packet Dumps](/docs/features/session-dumps) for what gets captured.

### Proxy TLS termination

Four of the five proxies terminate client TLS at the listener, and each has the
same three knobs: `*_TLS_DISABLE`, `*_TLS_CERT_FILE`, `*_TLS_KEY_FILE`. (The
Oracle listener is the exception — it has no TLS termination.)

The rule is the same everywhere: **set both the cert and the key, or neither.**
Leaving both empty makes the proxy generate a self-signed RSA-2048 certificate
in memory at startup — convenient for development, not something to run in
production. Setting exactly one of the two is a configuration error and the
proxy refuses to start.

#### PostgreSQL Proxy TLS

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_PG_TLS_DISABLE` | Answer `SSLRequest` with `N` and stay plaintext-only | `false` |
| `DBB_PG_TLS_CERT_FILE` | PEM-encoded server certificate | _auto self-signed_ |
| `DBB_PG_TLS_KEY_FILE` | PEM-encoded private key | _auto-generated RSA-2048_ |

Disabling TLS here is rarely what you want: a client with `sslmode=prefer` (the
libpq default) silently falls back to plaintext rather than failing, so the
credentials travel in the clear and nothing tells anyone.

#### MySQL Proxy TLS

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_MYSQL_TLS_DISABLE` | Refuse `SSLRequest` packets and stay plaintext-only | `false` |
| `DBB_MYSQL_TLS_CERT_FILE` | PEM-encoded server certificate | _auto self-signed_ |
| `DBB_MYSQL_TLS_KEY_FILE` | PEM-encoded RSA private key (RSA required for the non-TLS `caching_sha2` public-key path) | _auto-generated RSA-2048_ |

#### MongoDB Proxy TLS

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_MONGO_TLS_DISABLE` | Keep the listener plaintext — no TLS termination | `false` |
| `DBB_MONGO_TLS_CERT_FILE` | PEM-encoded server certificate | _auto self-signed_ |
| `DBB_MONGO_TLS_KEY_FILE` | PEM-encoded private key | _auto-generated RSA-2048_ |

MongoDB TLS is implicit from the first byte — there is no `STARTTLS`-style
upgrade to negotiate — so the client decides by connecting with `tls=true` or
without it. The listener peeks that first byte and serves both on the same port.
SASL `PLAIN` authentication is only accepted over TLS, so
`DBB_MONGO_TLS_DISABLE=true` also rules that mechanism out.

#### SQL Server Proxy TLS

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_MSSQL_TLS_DISABLE` | Answer `ENCRYPT_NOT_SUP` and stay plaintext — this also **refuses** clients that require encryption | `false` |
| `DBB_MSSQL_TLS_CERT_FILE` | PEM-encoded server certificate | _auto self-signed_ |
| `DBB_MSSQL_TLS_KEY_FILE` | PEM-encoded private key | _auto-generated RSA-2048_ |
| `DBB_MSSQL_TLS_MAX_VERSION` | Ceiling for the client-leg handshake: `1.2` or `1.3`. The floor is 1.2 either way. | `1.2` |

:::caution `DBB_MSSQL_TLS_MAX_VERSION=1.3` is opt-in
TDS carries the TLS handshake *inside* PRELOGIN packets, and under TLS 1.3 the
client's handshake ends on a write — so a driver has to decide for itself
whether that last flight is still encapsulated. dbbat handles both, but **only
`go-mssqldb` has been verified end to end**; the Microsoft ODBC and JDBC drivers
are untested at 1.3. The classic symptom of a driver guessing wrong is a client
that **connects and then hangs**, not an error, so test your own driver before
enabling this. Any value other than `1.2` or `1.3` fails the process at startup
rather than falling back silently.

The full explanation is in the
[SQL Server protocol notes](https://github.com/fclairamb/dbbat/blob/main/docs/mssql.md#tls-version).
:::

Note that a SQL Server client connecting with `Encrypt=no` still performs a
complete TLS handshake — TLS then covers the LOGIN7 packet and nothing else,
and both ends revert to cleartext TDS once login is through.

### Query Result Storage

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_QUERY_STORAGE_STORE_RESULTS` | Globally enable result-row capture | `true` |
| `DBB_QUERY_STORAGE_MAX_RESULT_ROWS` | Max rows captured per query | `100000` |
| `DBB_QUERY_STORAGE_MAX_RESULT_BYTES` | Max bytes captured per query | `104857600` (100 MB) |
| `DBB_QUERY_STORAGE_RETENTION` | Auto-delete query history and its captured rows past this Go duration. `0` keeps everything forever. | `0` (recommended: `720h`) |

Retention is **opt-in**: upgrading dbbat never starts deleting audit history on
its own. The per-query caps above bound one query's capture; retention bounds
the accumulation of every query ever proxied. See
[Query Logging](/docs/features/query-logging#retention) for exactly what a sweep
removes.

### Rate Limiting

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_RATE_LIMIT_ENABLED` | Enable per-user/IP rate limiting | `true` |
| `DBB_RATE_LIMIT_REQUESTS_PER_MINUTE` | Requests per minute per authenticated user | `60` |
| `DBB_RATE_LIMIT_REQUESTS_PER_MINUTE_ANON` | Requests per minute per source IP (unauthenticated) | `10` |
| `DBB_RATE_LIMIT_BURST` | Short-burst tolerance | `10` |

### Password Hashing (Argon2id)

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_HASH_PRESET` | One of `default`, `low`, `minimal` | `default` |
| `DBB_HASH_MEMORY_MB` | Memory cost (1–1024 MB) | `64` |
| `DBB_HASH_TIME` | Time cost (1–10) | `1` |
| `DBB_HASH_THREADS` | Parallelism (1–16) | `4` |

### Auth Cache

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_AUTH_CACHE_ENABLED` | Cache auth results across REST + proxies | `true` |
| `DBB_AUTH_CACHE_TTL_SECONDS` | Cache entry TTL | `300` |
| `DBB_AUTH_CACHE_MAX_SIZE` | Maximum cache entries | `10000` |

### Slack OAuth (optional)

| Variable | Description |
|----------|-------------|
| `DBB_SLACK_AUTH_CLIENT_ID` | Slack app client ID |
| `DBB_SLACK_AUTH_CLIENT_SECRET` | Slack app client secret |
| `DBB_SLACK_AUTH_TEAM_ID` | Restrict sign-in to one workspace |
| `DBB_SLACK_AUTH_AUTO_CREATE_USERS` | Auto-provision new users (default `true`) |
| `DBB_SLACK_AUTH_DEFAULT_ROLE` | Role assigned to auto-provisioned users (default `connector`) |

### Slack notifications & interactivity (optional)

When configured, DBBat posts each grant request to a Slack channel and updates
that message as the request is decided.

:::note Auto-approved requests
Grant definitions can be flagged `auto_approve`. A request matching such a
definition is approved instantly at request time — there is no admin decision to
make, so its Slack notification carries **no Approve/Deny buttons**, whether or
not a signing secret or app token is configured. A justification is still
required, and the approval gets its own audit trail tagged `via: auto_approve`
(as opposed to `via: slack` or a web-UI decision).
:::

| Variable | Description |
|----------|-------------|
| `DBB_SLACK_NOTIFY_BOT_TOKEN` | Bot user OAuth token (`xoxb-…`). Empty disables notifications. |
| `DBB_SLACK_NOTIFY_CHANNEL` | Channel id or `#name` to post to (default `#dbbat`). Required when the bot token is set. |
| `DBB_PUBLIC_URL` | Externally reachable base URL, used for the "Review in dbbat" deep-link. Required when the bot token is set, unless the `public.web_ui_url` parameter is set (see [Global Parameters](#global-parameters)) — that parameter takes precedence when both are present. |
| `DBB_SLACK_SIGNING_SECRET` | App signing secret. When set, notification messages carry **✅ Approve / ❌ Deny** buttons and DBBat serves `POST /api/v1/slack/interactions` to receive clicks. Empty keeps the link-through-UI flow (no buttons, no inbound endpoint). Requires the bot token — setting it without one fails at startup. The legacy name `DBB_SLACK_NOTIFY_SIGNING_SECRET` is also accepted as an alias; if both are set, the canonical `DBB_SLACK_SIGNING_SECRET` wins. |
| `DBB_SLACK_NOTIFY_APP_TOKEN` | App-level token (`xapp-…`, scope `connections:write`). When set, DBBat opens an outbound **Socket Mode** connection and receives Approve/Deny clicks over it — no inbound reachability or signing secret needed. Renders the buttons on its own. Requires the bot token — setting it without one fails at startup. |

#### Choosing a deployment shape

| Your deployment | Configure | How Approve/Deny clicks arrive |
|-----------------|-----------|--------------------------------|
| **Publicly reachable** — Slack's servers can reach `DBB_PUBLIC_URL` | Bot token + `DBB_SLACK_SIGNING_SECRET` | Inbound `POST /api/v1/slack/interactions`, authenticated by Slack's request signature |
| **Gated** — VPN, intranet, or an ingress that allowlists source IPs | Bot token + `DBB_SLACK_NOTIFY_APP_TOKEN` | Outbound **Socket Mode** WebSocket — no inbound reachability needed |
| **Neither** — notifications only | Bot token only | No buttons: messages carry the "Review in dbbat" deep-link and admins decide in the web UI |

"Publicly reachable" means reachable by *Slack's servers*, not just by your
users' browsers. A `curl` from your laptop proving the endpoint answers is not
enough: if the load balancer in front of DBBat allowlists inbound source IPs (a
common webhook-hardening pattern), Slack's delivery is dropped at the network
boundary — clicks fail with *"Operation timed out"* after 3 seconds and DBBat
never sees the request. That is a **gated** deployment: use Socket Mode.
(Allowlisting Slack instead is impractical — Slack does not publish a small
stable set of interactivity source IPs.) At startup, DBBat logs a reminder when
interactivity is configured with the HTTP endpoint as its only transport.

#### Enabling the Approve / Deny buttons

1. In your Slack app, enable **Interactivity & Shortcuts** and set the request
   URL to `https://<YOUR_DBBAT_HOST>/api/v1/slack/interactions` (see
   [`slack_app_manifest.json`](https://github.com/fclairamb/dbbat/blob/main/slack_app_manifest.json)).
2. Copy the app's **Signing Secret** from the *Basic Information* page into
   `DBB_SLACK_SIGNING_SECRET`.

Clicks are authenticated by Slack's request signature. Only DBBat admins can
approve or deny; anyone else who clicks gets an ephemeral error. A decision made
from a button is identical to one made in the web UI (same audit event, tagged
`via: slack`), updates the original message in place (removing the buttons), and
posts a reply in the message thread.

Requests matching an `auto_approve` grant definition never reach this flow: they
are already approved when the message is posted, so it carries no buttons and is
never "decided". Their audit event is tagged `via: auto_approve`.

**Deployment note:** button clicks require *Slack's servers* to reach
`DBB_PUBLIC_URL` (inbound), whereas the deep-link only needs *users' browsers*
to reach it. Intranet-only deployments that can't accept inbound Slack traffic
should use **Socket Mode** (below) instead of the signing secret — or leave both
unset and keep the link-through-UI flow.

#### Socket Mode (no inbound endpoint)

For deployments Slack can't reach inbound (behind a VPN or an IP-allowlisted
ingress), Socket Mode delivers Approve/Deny clicks over an **outbound** WebSocket
that DBBat opens to Slack — so no public reachability and no signing secret are
required.

1. In your Slack app, open **Settings → Socket Mode** and enable it.
2. Under **Basic Information → App-Level Tokens**, generate a token with the
   `connections:write` scope and put it in `DBB_SLACK_NOTIFY_APP_TOKEN`.

Everything downstream (admin-only decisions, ephemeral errors, `via: slack` audit
tagging, in-place message update, thread reply) is identical to the HTTP path —
only the transport differs. Socket Mode and the HTTP endpoint can both be
configured; at the Slack app level, enabling Socket Mode makes Slack deliver over
the socket and ignore the request URL.

## Configuration File

DBBat supports YAML, JSON, and TOML configuration files.

### YAML Example

```yaml
listen_pg: ":5433"
listen_ora: ":1522"
listen_mysql: ":3307"
listen_mongo: ":27018"
listen_mssql: ":1434"
listen_api: ":4200"
dsn: "postgres://user:pass@localhost:5432/dbbat?sslmode=require"

query_storage:
  store_results: true
  max_result_rows: 100000
  max_result_bytes: 104857600
  retention: "0" # keep forever; e.g. "720h" for 30 days

rate_limit:
  enabled: true
  requests_per_minute: 60
  burst: 10

dump:
  dir: "/var/dbbat/dumps"
  max_size: 33554432
  retention: "72h"

pg:
  tls:
    disable: false
    cert_file: "/etc/dbbat/pg.crt"
    key_file: "/etc/dbbat/pg.key"

mysql:
  tls:
    disable: false
    cert_file: "/etc/dbbat/mysql.crt"
    key_file: "/etc/dbbat/mysql.key"

mongo:
  tls:
    disable: false
    cert_file: "/etc/dbbat/mongo.crt"
    key_file: "/etc/dbbat/mongo.key"

mssql:
  tls:
    disable: false
    cert_file: "/etc/dbbat/mssql.crt"
    key_file: "/etc/dbbat/mssql.key"
  tls_max_version: "1.2" # "1.3" is opt-in; see above

slack_auth:
  client_id: "..."
  client_secret: "..."
  auto_create_users: true
  default_role: "connector"

slack_notify:
  bot_token: "xoxb-..."
  channel: "#dbbat"
  signing_secret: "..."   # Approve/Deny buttons via the inbound HTTP endpoint
  # app_token: "xapp-..." # or via Socket Mode (outbound; for gated deployments)

public_url: "https://dbbat.example.com"
```

Load with the `--config` flag:

```bash
dbbat serve --config /etc/dbbat/config.yaml
```

## Global Parameters

Since v0.16.0, some settings live in the database rather than in the environment,
so an operator can change them at runtime without a restart. They are managed
through `GET`, `PUT`, and `DELETE` on `/api/v1/parameters`, and the effective
values are exposed by `GET /api/v1/instance`.

| Parameter | Description |
|-----------|-------------|
| `public.web_ui_url` | Externally reachable base URL of the web UI, used for Slack deep-links. **Takes precedence over `DBB_PUBLIC_URL`** when set. |

```bash
# Read the current parameters
curl -H "Authorization: Bearer $DBBAT_API_KEY" http://localhost:4200/api/v1/parameters

# Set the public web UI URL
curl -X PUT http://localhost:4200/api/v1/parameters \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"public.web_ui_url": "https://dbbat.example.com"}'
```

Deleting the parameter falls back to `DBB_PUBLIC_URL`.

## Generating an Encryption Key

DBBat requires a 32-byte AES-256 key for encrypting database credentials. If neither `DBB_KEY` nor `DBB_KEYFILE` is set, DBBat generates one at `~/.dbbat/key` and reuses it on subsequent starts.

To generate one yourself:

```bash
openssl rand -base64 32
```

Use it as `DBB_KEY=…` or write it to a file referenced by `DBB_KEYFILE=`.

## Storage Database

DBBat stores its configuration and logs in a PostgreSQL database. Provide the DSN via `DBB_DSN`.

### DSN Format

```
postgres://user:password@host:port/database?sslmode=require
```

### SSL Modes

- `disable` — No SSL
- `require` — Require SSL but don't verify certificate
- `verify-ca` — Require SSL and verify CA
- `verify-full` — Require SSL and verify CA + hostname

:::warning Security
DBBat warns at startup if any configured target database matches the storage DSN — sharing a database for storage and proxying enables privilege escalation. Use a separate database (or a separate cluster) for DBBat's own storage.
:::

## Run Modes

### Test Mode (`DBB_RUN_MODE=test`)

Useful for E2E testing and development:

- Wipes all DBBat-owned tables on startup
- Recreates admin with password `admintest` (already password-changed)
- Creates `viewer` (role `viewer`) and `connector` (role `connector`) users
- Creates a sample target database, plus stable API keys (`dbb_admin_key`, `dbb_viewer_key`, `dbb_connector_key`)

### Demo Mode (`DBB_RUN_MODE=demo`)

For public demos with restricted database targets:

- Wipes all DBBat-owned tables on startup
- Creates admin/viewer/connector users with their username as the password
- Only allows database configurations matching `DBB_DEMO_TARGET_DB`
- Defaults to `demo:demo@localhost/demo`

## Default Admin

On first startup (in production mode), DBBat creates a default admin user:

- **Username**: `admin`
- **Password**: `admin`

The password is flagged as requiring change. Login attempts return `403 password_change_required` until the admin calls `PUT /api/v1/auth/password` to set a real password.
