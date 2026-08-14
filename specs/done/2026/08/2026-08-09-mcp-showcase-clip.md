# Showcase clip: an AI agent's query held for human approval

No GitHub issue yet — file one when picking this up.

## Goal

Add a second showcase scenario to `front/showcase/` in which the held statement
comes from an **AI agent over MCP** rather than from a `psql` session: the agent
calls the `query` tool, gets `approval_pending` back, a human clicks Approve in
the dbbat UI, and `await_approval` returns the rows. Render it into
`website/static/img/showcase/` and surface it on the MCP feature page and the
homepage.

## Why

The MCP feature shipped (`internal/mcp/`, `docs/mcp.md`,
`website/docs/features/mcp.md`) and its flagship demo is exactly this: an agent
issues a destructive statement, the statement freezes mid-flight, and a human
gets a Slack button. Nobody else in the database-access-governance space can
show that, because it needs a proxy.

Right now the MCP docs page reuses the existing `psql` clip with a caption
explaining that the agent's experience is the same hold. That is honest but it
is not the demo — the whole point is seeing the *agent* blocked.

This was deliberately deferred from
`specs/todos/2026-08-09-mcp-server-agent-access.md`: `make showcase` stands up
its own PostgreSQL container and a demo-mode dbbat and takes many minutes, so it
is on-demand only, and authoring an unrunnable Playwright scenario blind — plus
the website component wiring for a second video slot — was judged more likely to
need rewriting than to land. Do it with the runner available.

## Implementation

`front/showcase/` is a separate Playwright project driven by
`scripts/showcase.sh`, which owns the whole lifecycle (its own throwaway
PostgreSQL on 5499, its own demo-mode dbbat on 8099/5099). Read
`front/showcase/README.md` and `CLAUDE.md`'s "Website showcase media" section
first.

### The scenario

Model it on `front/showcase/approval-video.spec.ts` and
`front/showcase/lib/hold.ts`, which already own the choreography shared by the
clip and its poster. The substitutions:

- **Replace the `pg` client with an MCP client.** `front/showcase/lib/traffic.ts`
  builds the `pg` connection; the new equivalent posts JSON-RPC at
  `${API_URL}/api/v1/mcp` with `Authorization: Bearer <dbb_ key>`. Two calls:
  `tools/call` → `query` with the `UPDATE`, then `tools/call` →
  `await_approval` with the returned `execution_id`. Plain `fetch` is enough;
  do not pull in an MCP client library for this.
- **The demo-mode connector user needs an API key.** `front/showcase/global-setup.ts`
  provisions the scenario; mint a key there (`POST /api/v1/keys` requires web
  session or Basic Auth, which the setup already has) and hand it to the spec
  through `front/showcase/state.ts`.
- **Replace the terminal pane with an agent pane.** `front/showcase/lib/terminal.ts`
  injects a fake `psql` window. The agent variant should render a chat-style
  pane — the tool call as JSON going out, the `approval_pending` result coming
  back, then the final rows — using the same injected-DOM technique and the
  same rule that nothing about the *result* is faked: every value printed comes
  from the real MCP response.
- **Keep the UI half identical.** The human still approves from
  `connection-watch-panel` with the drawn cursor (`front/showcase/lib/cursor.ts`).
  The point of the clip is that the agent's held statement is the same object a
  human is looking at.

### Output and wiring

- Emit `mcp-approval-hold-av1.mp4` + `-h264.mp4` + `-poster.png/.webp`
  alongside the existing pair, through the same ffmpeg step in
  `scripts/showcase.sh`. `SHOWCASE_PROJECT=` should be able to select it alone.
- `website/static/img/showcase/manifest.json` is written every run; nothing to
  change there beyond regenerating.
- Point `website/docs/features/mcp.md`'s `<video>` at the new clip and drop the
  "the clip above shows the hold from a human's psql session" caveat.
- Consider a slot on the homepage (`website/src/components/ProductShowcase/`).
  It is the strongest differentiator the project has, but the homepage already
  carries one video and two of them may be one too many — decide with the
  rendered clip in hand, not before.

### Guardrails

- Never run `make showcase` against the shared dev stack; the script's own
  containers exist precisely because demo mode drops every table on startup.
- The suite never gates CI. `.github/workflows/showcase.yml` runs it on `front/`
  PRs with output discarded as a rot guard, so a broken spec is visible but not
  blocking — which is also why an unverified scenario should not be committed.

## Key files

- `front/showcase/approval-video.spec.ts`, `approval-poster.spec.ts`
- `front/showcase/lib/hold.ts`, `lib/terminal.ts`, `lib/traffic.ts`, `lib/cursor.ts`
- `front/showcase/global-setup.ts`, `state.ts`, `config.ts`
- `scripts/showcase.sh`
- `website/docs/features/mcp.md`, `website/src/components/ProductShowcase/`
- `docs/mcp.md` — the approval-hold section the clip illustrates
