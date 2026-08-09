// Package mcp exposes dbbat's databases to AI agents over the Model Context
// Protocol, without adding a second way into a database.
//
// # The one design decision that matters
//
// Every statement an agent runs is executed by **dialing dbbat's own proxy
// listener over loopback** with a real protocol client (pgx for PostgreSQL,
// go-mysql for MySQL/MariaDB), authenticating as the API key's owner with that
// same key as the password — which is exactly how any `dbb_` key holder
// connects with psql or the mysql CLI.
//
// There is deliberately no internal execution path. The consequence is that
// authentication, grant resolution, `read_only`/`block_ddl`/`block_copy`,
// quotas, query logging, session capture and the mid-flight approval gate are
// not re-implemented here — they are the *same code*, reached over the same
// wire, and this package cannot drift from them because it never touches them.
//
// A bypass path is precisely the bug class fixed in #306/#308 (statements
// skipping the approval gate). Do not add one: if you find yourself reaching
// for *store.Store or a driver connected straight to a customer database in
// order to run an agent's SQL, the design has been broken.
//
// The store is read here for exactly one thing — listing the grants the caller
// holds, which is metadata the caller can already read from
// GET /api/v1/grants — and never to execute or authorize a statement.
//
// # Transport and identity
//
// The endpoint is Streamable HTTP, served on the existing gin router under
// /api/v1/mcp, and it runs **stateless**: each HTTP request carries the
// `Authorization: Bearer dbb_…` header, is authenticated by the ordinary API
// middleware, and gets a freshly built [github.com/modelcontextprotocol/go-sdk/mcp.Server]
// whose tool closures are bound to that caller. Nothing about a caller
// survives a request, so a revoked key stops working on the next call rather
// than at the end of some session.
//
// # Approval holds
//
// A held statement blocks the wire connection. That is fine for psql and
// awkward for an MCP request, so `query` waits a short grace window and then
// hands the agent a structured `approval_pending` (or `still_running`) result
// naming an execution id, while the loopback connection stays parked in the
// background. The `await_approval` tool long-polls that execution. The agent
// therefore never silently times out on a held query: every return is a status
// that names the next action.
package mcp
