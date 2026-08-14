# MCP server: governed database access for AI agents

## Goal

Ship a first-class MCP (Model Context Protocol) server so AI agents — Claude
Code, Claude Desktop, or anything MCP-capable — can query databases *through
dbbat's existing governance pipeline*: scoped to the caller's grants,
time-boxed, every query logged, and approval holds intact. The flagship demo:
an agent issues `DELETE FROM users`, the statement freezes mid-flight, and a
human gets a Slack Approve/Deny button.

## Why

Every player in the database-access-governance space is converging on "govern
the AI agent's access to production data" (see
`docs/competitive-landscape.md`, watch list). hoop.dev leads with that
narrative; Teleport shipped MCP support. DBBat is unusually well placed
because the hard parts already exist — API keys, time-boxed grants, per-query
logging, mid-flight approval holds. This feature is mostly plumbing plus a
demo nobody else can replicate: statement-level human approval of a live
agent query.

No GitHub issue yet — file one when picking this up.

## Implementation

### Transport & auth

- Streamable-HTTP MCP endpoint on the existing gin server (`internal/api/server.go`),
  e.g. `POST /mcp`. Agents connect remotely with an existing `dbb_` API key as
  the Bearer token, reusing the API-key middleware — the MCP session acts as
  the key's owner, so grants, quotas, and audit attribution all come for free.
- Optional later: a `dbbat mcp` stdio subcommand for local setups that just
  forwards to the HTTP endpoint. Not needed for v1.
- Library: the official Go SDK (`github.com/modelcontextprotocol/go-sdk`) —
  verify current name/maturity at implementation time.

### Execution path — critical design decision

**Execute agent queries by dialing dbbat's own proxy listener** (loopback)
with a real protocol client, authenticating as the API-key's user, rather
than adding a parallel internal execution path. This guarantees the exact
same auth → grant → interception → approval → logging pipeline as any other
client, with zero new enforcement code. A bypass path is precisely the bug
class fixed in #306/#308 (statements skipping the approval gate) — don't
reintroduce it.

- Phase 1: PostgreSQL + MySQL only — client drivers (`jackc/pgx/v5`,
  `go-mysql-org/go-mysql`) are already in `go.mod`.
- Phase 2: Oracle (via go-ora as a client), MongoDB, SQL Server.

### Tool surface (keep small)

- `list_databases` — databases the caller currently holds a grant on, with
  grant expiry and controls (read_only etc.), so the agent can plan.
- `query` — `{database, sql, params?, max_rows?}` → rows as JSON. Server-side
  row cap regardless of what the agent asks for.
- `describe` (nice-to-have) — table/column introspection convenience.

### Approval-hold UX for a blocked agent

A held statement blocks the wire connection — fine for psql, awkward for an
MCP request. Sketch: if the query is still pending after a short window
(~10s), return a structured `approval_pending` result carrying the query UID,
plus an `await_approval` tool that long-polls the resolution (the approval
registry in `internal/approval/registry.go` and the events broker in
`internal/events/` already have everything needed). The Slack escalation
timer (`DBB_APPROVAL_SLACK_DELAY`) covers notifying the human. Exact shape to
be settled during implementation; the constraint is that the agent must never
silently time out on a held query.

### Config

- `DBB_MCP_ENABLED` (default `true` — the endpoint is API-key-gated anyway).

### Key files

- `internal/api/server.go` — route + middleware wiring
- `internal/api/keys.go` / API-key middleware — bearer auth reuse
- new `internal/mcp/` — server, tools, loopback execution
- `internal/approval/registry.go`, `internal/events/` — await-approval plumbing
- `internal/api/openapi.yml` — document the endpoint (even if MCP, not REST)
- `docs/` + `website/` — usage doc and a showcase clip of the approval-hold
  demo (extend `front/showcase/`)
