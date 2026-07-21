---
sidebar_position: 6
---

# SSH Tunnels

Not every database is reachable from wherever DBBat runs. Production databases commonly sit in a private subnet with no route from outside the VPC, fronted by a bastion host.

DBBat can dial any upstream **through an SSH bastion**, so those databases become proxyable without opening them to the network. Tunnelling is available for all four proxied protocols — PostgreSQL, Oracle, MySQL/MariaDB, and MongoDB.

```
client ──► DBBat ──► SSH bastion ──► database in private subnet
```

## Registering a Bastion

Bastions are stored as servers with protocol `ssh`, managed through their own endpoint:

```bash
curl -X POST http://localhost:4200/api/v1/ssh-servers \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "prod-bastion",
    "description": "Production VPC bastion",
    "host": "bastion.example.com",
    "port": 22,
    "username": "ec2-user",
    "ssh_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n..."
  }'
```

| Field | Description |
|-------|-------------|
| `host` / `port` | Bastion address; port defaults to `22` |
| `username` | SSH user on the bastion |
| `ssh_private_key` | Private key in PEM form. **Write-only** — never returned by the API |
| `ssh_passphrase` | Passphrase for the key, if it is encrypted. **Write-only** |
| `ssh_known_host_key` | The pinned host key. **Read-only**, populated on first successful connection |

Creating a bastion whose name is already taken returns `409 DUPLICATE_NAME`.

List registered bastions with:

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:4200/api/v1/ssh-servers
```

## Routing a Server Through It

Set `via_uid` on a database server to the bastion's UID:

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "prod-behind-bastion",
    "protocol": "postgresql",
    "host": "10.0.1.20",
    "port": 5432,
    "database_name": "myapp",
    "username": "readonly_user",
    "password": "secret",
    "via_uid": "770e8400-e29b-41d4-a716-446655440000"
  }'
```

`host` and `port` are now resolved **from the bastion's perspective** — use the private address the bastion can reach, not one reachable from DBBat.

A `via_uid` of `null` means a direct dial. To remove a tunnel from an existing server without deleting it, send `clear_via_uid` on update:

```bash
curl -X PUT http://localhost:4200/api/v1/servers/$SERVER_UID \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"clear_via_uid": true}'
```

Nothing changes for the client: it still connects to DBBat's proxy port exactly as before, and the tunnel is an implementation detail of how DBBat reaches the upstream.

## Host Key Verification

Bastion host keys are pinned **trust on first use** (TOFU):

1. On the first successful connection, the bastion's host key is recorded as `ssh_known_host_key`.
2. Every later connection must present that same key, or the dial fails.

This detects a bastion whose key changes after you first connected — a re-provisioned host, or a man-in-the-middle. It does **not** verify the very first connection, so if an attacker is in position at the moment you register the bastion, TOFU will happily pin their key.

:::tip
For a high-value bastion, register it from a trusted network and check the pinned `ssh_known_host_key` against the host key you expect before granting anyone access through it.
:::

If a bastion is legitimately rebuilt with a new host key, an admin must clear the stored key before connections resume.

## Connection Pooling

DBBat keeps a **1:1 mapping between client sessions and upstream database connections** — that is unchanged, and is what makes per-session grant enforcement and query attribution work.

The SSH *transport* is different: connections to a given bastion are pooled and shared. Many proxied sessions routed through the same bastion multiplex over one SSH connection rather than each paying a fresh SSH handshake. The pool is keyed per bastion.

## Bastions Are Not Targets

SSH servers are deliberately kept out of every context where a database target is expected:

- They do not appear in the regular `GET /api/v1/servers` listing.
- They cannot be granted to a user — there is nothing to proxy.
- They are excluded from the grant-request target dropdown.

They surface only under `/api/v1/ssh-servers` and in the "via SSH server" selector when configuring a database server. Listing them requires the admin role.

## Operational Notes

- **Egress**: DBBat needs outbound TCP/22 to each bastion. On Kubernetes, check any egress NetworkPolicy allows this.
- **Credentials**: private keys and passphrases are encrypted at rest with the same AES-256-GCM scheme as database passwords, and are never returned by the API or shown in the UI.
- **Key choice**: give DBBat its own dedicated key pair and bastion user rather than reusing an operator's personal key, so access can be revoked independently.

## See Also

- [Server Configuration](/docs/configuration/databases) — full field reference
- [Security](/docs/security) — trust model and hardening checklist
- [API Reference](/docs/api) — `/api/v1/ssh-servers` endpoints
