# Validate SSH bastion connectivity at provisioning time

## Goal

Give an admin immediate feedback on whether a newly-created `protocol: ssh` server
actually works — reachable host, accepted private key, usable tunnel — instead of
discovering it only when a user first tries to connect through the proxy.

## Why

Creating an SSH bastion today is write-and-hope. `POST /api/v1/servers` accepts the
host, username and `ssh_private_key` and returns 200 without ever dialling the
bastion. `ssh_known_host_key` stays empty until the first real proxied connection
performs TOFU pinning, so there is no signal in the API response or the UI telling
the admin whether the credentials are valid.

Concretely: provisioning the `opaline` bastion (`195.154.39.225`, user `www-data`)
plus its tunnelled MySQL target succeeded, both objects were created, and
`GET /servers/{uid}/connection` happily returned a connection URI — but none of that
proves the tunnel works. Verifying it required a live client connection through
`db.stonal.io:3306`, which is VPN-gated, so the provisioning could not be validated
from outside the network at all. A typo in the host, a wrong `username`, or a key
the bastion does not accept would all look identical to success.

## Implementation

- Add `POST /api/v1/servers/{uid}/test` (and a "Test connection" button in the
  server management UI added in v0.17.0).
- For `protocol: ssh`: dial the bastion, complete the handshake with the stored key,
  and — on first success — persist the host key into `ssh_known_host_key`, so TOFU
  pinning happens at provisioning time under admin supervision rather than
  implicitly on some user's first query.
- For database servers with a non-null `via_uid`: dial through the tunnel and open a
  protocol-level connection to the target, reusing the existing per-protocol dial
  paths in `internal/proxy/<protocol>/` rather than reimplementing handshakes.
- Return a structured result distinguishing the failure stages (bastion unreachable /
  SSH auth rejected / tunnel established but DB dial failed / DB auth failed) — the
  stage is what tells the admin which field they got wrong.
- Consider calling the same check inline on create/update and returning the outcome
  as a non-fatal warning in the response body, so the common path needs no second call.

No GitHub issue exists for this yet — one should be filed.

## Implementation Plan

1. **`internal/proxy/shared`** — expose a bastion-only connect path.
   - Add `(*Dialer).ConnectBastion(ctx, resolver, key, uid) (*ssh.Client, error)` wrapping
     the existing unexported `sshClientFor`, so a connectivity check can dial and
     handshake a `protocol: ssh` row (and its multi-hop `via` chain) without a
     database target. TOFU pinning already happens inside `sshClientFor`.

2. **`internal/proxy/conncheck`** (new package) — the staged check itself.
   - `Result{OK, Stage, Code, Message, HostKeyPinned, KnownHostKey, DurationMs}`.
   - Stages: `bastion_dial`, `bastion_auth`, `target_dial`, `target_auth`, `ok`.
   - Codes distinguish the enumerated failures: `dns_failure`, `timeout`,
     `unreachable`, `host_key_mismatch`, `auth_rejected`, `bad_private_key`,
     `no_auth_method`, `handshake_failed`, plus target-side `db_auth_failed` /
     `db_handshake_failed`.
   - Uses a **fresh** `shared.NewDialer()` per check so a pooled, already-open
     bastion client cannot mask a broken configuration.
   - Per-protocol target probes reuse the same client libraries and the same
     injected dialer as the proxies (`pgconn` for postgresql, go-mysql
     `ConnectWithDialer` for mysql/mariadb, mongo-driver for mongodb). Oracle has
     no reusable standalone dial path, so it is TCP-reachability only, reported
     explicitly as `target_dial` OK with an "auth not verified" note.
   - New package (rather than inside `shared`) to avoid an import cycle:
     the protocol proxies import `shared`.

3. **API** — `POST /api/v1/servers/{uid}/test`, admin only.
   - Always 200 with the structured result (`ok:false` for failures) so the UI can
     render the stage; 404 for unknown uid, 400 for a bad uid.
   - Audit event `database.tested` recording uid/stage/code — never secrets.
   - Opt-in inline check: `test_connection: true` on create/update runs the same
     check and attaches a non-fatal `connection_test` object to the response.
     Opt-in rather than always-on so existing provisioning of unreachable
     targets (tests, demo mode) does not slow down or change shape.
   - Update `internal/api/openapi.yml` (path + `ConnectionTestResult` schema).

4. **Frontend** — regenerate `front/src/api/schema.ts`, add a "Test" action to the
   server rows in `front/src/routes/_authenticated/servers/index.tsx` showing the
   stage/code/message and the pinned host key on success.

5. **Tests**
   - `internal/proxy/conncheck/conncheck_test.go` against the existing in-process
     fake SSH server harness: success + host-key pinning, unreachable bastion,
     rejected key, DNS failure, timeout, host-key mismatch, tunnelled target dial
     failure.
   - Assert the result never contains private-key/passphrase material.
   - API-level test for the new route (admin-only, 404 on unknown uid).

6. **Security** — secrets are never echoed: the result carries only stage, code and
   a classified message; logs carry uid/stage/code only.
