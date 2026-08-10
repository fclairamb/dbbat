---
sidebar_position: 7
sidebar_label: AI Agents (MCP)
title: AI Agents (MCP)
description: Give Claude Code, Claude Desktop or any MCP client governed access to your databases — scoped to a grant, time-boxed, every query logged, and a human in the loop on the statements that matter.
---

# AI Agents (MCP)

DBBat speaks the [Model Context Protocol](https://modelcontextprotocol.io), so an AI agent can query your databases **through the same governance every other client goes through**: scoped to the caller's grant, time-boxed, every statement logged — and, on the statements you care about, suspended mid-flight until a human approves it.

The demo is the point: an agent issues `DELETE FROM users`, the statement freezes on the wire, and somebody gets a Slack message with an Approve and a Deny button.

## Why this is not just another database MCP server

Most database MCP servers hold a connection string and run whatever the model asks. The governance, if there is any, lives in the tool layer — which is to say, in the same place the model is trying to talk its way past.

DBBat inverts that. **An agent's statement is executed by dialing DBBat's own proxy listener** with a real database client — the same library a human's tool would use — authenticating as the API key's owner:

```
Claude ──MCP/HTTP──► DBBat API ──loopback──► DBBat proxy ──► your database
```

There is no internal execution path. Read-only enforcement, DDL blocking, quotas, expiry, revocation, query logging, session capture and approval holds are not reimplemented for agents — they are the same code, reached over the same wire. An agent gets exactly what its key holder would get from `psql`, and nothing else.

## Connecting

Any existing DBBat API key works. Create one under **API Keys** in the UI, then:

```bash
claude mcp add --transport http dbbat https://dbbat.example.com/api/v1/mcp \
  --header "Authorization: Bearer dbb_your_key_here"
```

Or in `.mcp.json` / Claude Desktop's config:

```json
{
  "mcpServers": {
    "dbbat": {
      "type": "http",
      "url": "https://dbbat.example.com/api/v1/mcp",
      "headers": { "Authorization": "Bearer dbb_your_key_here" }
    }
  }
}
```

The endpoint is on by default (`DBB_MCP_ENABLED=true`). It accepts **API keys only** — not Basic Auth, not browser sessions — because the key also serves as the credential the loopback client presents to the proxy.

Sessions are stateless: every request is re-authenticated, so revoking a key stops an agent on its next call.

## Supported databases

Every database DBBat proxies.

| Protocol | Status | What an agent sends |
|---|---|---|
| PostgreSQL | Supported | SQL, `$1…` bind parameters |
| MySQL / MariaDB | Supported | SQL, `?` bind parameters |
| Oracle | Supported | SQL, `:1…` bind parameters |
| Microsoft SQL Server | Supported | SQL, `@p1…` bind parameters |
| MongoDB | Supported | one command, as `<command> <extended JSON>` |

The proxy listener for the protocol has to be running in the same process. If it is not, the tool says so rather than reaching for the database another way — there is no other way.

### MongoDB has no SQL

So `query` takes the command in the form DBBat itself writes into the query log:

```json
{ "database": "prod-mongo", "sql": "find {\"find\":\"users\",\"filter\":{\"active\":true},\"limit\":10}" }
```

That matters for approval patterns: a pattern matches the same text whether the command came from `mongosh` or from an agent. `describe` lists collections, and describing one reports the fields of a **sampled document** — MongoDB has no column catalog, and the tool says so instead of inventing a schema.

### Oracle: read the approval caveat

Oracle's wire protocol is hand-rolled in DBBat and a statement's SQL is located heuristically, so a frame DBBat cannot decode is forwarded ungated *and* unlogged. That is a property of the Oracle proxy, not of MCP — an agent is in exactly the same position as a human at `sqlplus` — but do not read "Oracle is supported" as "Oracle holds are airtight". Cursor re-executions DBBat saw parsed *are* gated, on every execution.

## The tools

Four, deliberately small.

| Tool | What it does |
|---|---|
| `list_databases` | The databases the key's owner currently holds a grant on, with the grant's expiry, its controls and its quota usage |
| `query` | Run one statement — `{database, sql, params?, max_rows?}` |
| `describe` | List tables, or one table's columns |
| `await_approval` | Wait for a statement suspended by an approval hold |

`list_databases` is what the agent plans against. It reports the expiry, so a model can tell "I have twenty minutes" from "I have two days", and the controls, so it does not waste a turn discovering the grant is read-only.

### Rows are capped by the server

`max_rows` is a request, not a decision. Unset gives 200; anything above 1000 gives 1000 with `truncated: true`. The cap is applied **before** the statement runs, so a model that decides it needs a million rows simply does not get them.

DBBat stops reading the result set at the cap rather than buffering it — an unbounded `SELECT *`, or a MongoDB cursor over a large collection, cannot turn one tool call into an out-of-memory.

The statement itself is never rewritten. DBBat does not quietly append a `LIMIT`: what runs is what is logged, and what approval patterns match.

## Human approval, mid-statement

This is the part nobody else can replicate, because it needs a proxy.

Put a pattern on the grant definition:

```json
{
  "name": "agent-prod-read",
  "duration_seconds": 3600,
  "controls": ["block_ddl"],
  "approval_patterns": ["(?i)^\\s*DELETE\\s+FROM", "(?i)^\\s*UPDATE"],
  "approver_user_group_uids": ["…sre group uid…"]
}
```

Now when the agent runs `DELETE FROM users`:

1. The statement reaches DBBat and matches the pattern.
2. It is **suspended on the wire** — nothing is forwarded to the database. The query appears in the UI as pending, addressable, with its full text.
3. Ten seconds later, the agent gets a structured answer rather than a hang:

   ```json
   {
     "status": "approval_pending",
     "execution_id": "9f2c…",
     "query_uid": "0192…",
     "approval_pattern": "(?i)^\\s*DELETE\\s+FROM",
     "message": "The statement is suspended awaiting human approval…"
   }
   ```

4. After `DBB_APPROVAL_SLACK_DELAY` (30s by default), Slack gets the escalation with Approve and Deny buttons.
5. The agent calls `await_approval`, which blocks and answers — the rows on approval, the approver's reason on denial, or the same pending status so it calls back.

**The agent is never left with a silent timeout.** Every result names the next action, which is what keeps a model from deciding a hold is a failure and retrying — a retry would park a second connection on the same human.

Self-approval is rejected, including for admins. An agent acting as its owner cannot wave its own statement through.

<video controls width="100%" poster="/img/showcase/approval-hold-poster.webp" style={{borderRadius: 8}}>
  <source src="/img/showcase/approval-hold-av1.mp4" type="video/mp4; codecs=av01.0.05M.08" />
  <source src="/img/showcase/approval-hold-h264.mp4" type="video/mp4" />
</video>

The clip above shows the hold from a human's `psql` session; the agent's
experience is the same hold, reported as `approval_pending`.

## What you see afterwards

Nothing about an agent's traffic is a special path, so it lands where everything else does:

- a connection and a query row per statement, attributed to the key's owner;
- captured result rows, under the usual storage settings;
- a pcapng session capture, if captures are enabled;
- an audit entry for every approval decision, naming who approved.

The loopback session advertises `application_name = dbbat-mcp`, so agent traffic is also distinguishable inside the target database's own views.

## Operating advice

- **Give the agent its own DBBat user.** Grants, quotas and attribution are per-user; an agent sharing a human's key makes the query log unreadable.
- **Approval patterns are the control that matters here.** An agent is a client that can write a `DELETE` it did not fully think through.
- **Keep grants short.** MCP changes nothing about expiry: an agent with a one-hour grant is exactly as bounded as a human with one.

For the protocol-level details — execution model, correlation of held queries, replica behavior, execution lifetimes — see [`docs/mcp.md`](https://github.com/fclairamb/dbbat/blob/main/docs/mcp.md) in the repository.
