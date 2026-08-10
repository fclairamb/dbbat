/**
 * The held-`UPDATE` scenario as an **AI agent** experiences it, shared by the
 * MCP clip and by the still that fronts it.
 *
 * The sibling of ./hold.ts, and structured the same way for the same reason:
 * `mcp-approval-hold-poster.png` is the `<video>` element's poster and what the
 * website shows under `prefers-reduced-motion`, so the two have to depict the
 * same exchange. They differ only in pacing.
 *
 * What is real: the JSON-RPC calls against `POST /api/v1/mcp` with a `dbb_`
 * key, the loopback proxy session dbbat opens to run the statement, the hold,
 * the human who releases it, and every value the pane prints. What is drawn:
 * the pane itself (./agent.ts) and the mouse pointer (./cursor.ts).
 *
 * The beats are exported separately rather than as one `playHold()` because the
 * scenario spans two pages: the call is composed on the connections list,
 * because the session it will open does not exist yet, and the hold is watched
 * on that session's own page once it does. Both specs walk the same beats in
 * the same order.
 */
import type { Page } from "@playwright/test";
import { SERVER_NAME } from "../config";
import { typeAgentLines, writeAgentLines } from "./agent";
import type { McpClient, McpQueryOutput } from "./mcp";
import { formatResult } from "./traffic";

/**
 * The write the agent issues.
 *
 * It matches the grant's approval pattern (`(?i)^\s*UPDATE\b`, see
 * ../config.ts), which is what parks it on a human. `RETURNING` is there so the
 * released statement answers with rows rather than a bare count — the clip's
 * last beat is the agent finally getting its data.
 */
export const AGENT_UPDATE_SQL =
  "UPDATE customers SET plan = 'business' WHERE plan = 'starter' RETURNING company, plan, mrr_eur;";

/** How long `await_approval` is asked to block for. Capped at 120 server-side. */
export const AWAIT_TIMEOUT_SECONDS = 120;

/** The pane's title bar. */
export const AGENT_PANE_TITLE = `claude — dbbat MCP (${SERVER_NAME})`;

/** How fast the pane is driven. Mirrors ./hold.ts's pacing pair. */
export interface AgentPacing {
  msPerChar: number;
  beatMs: number;
}

/** Pacing for the clip: composed at a readable speed, with room to take it in. */
export const AGENT_CLIP_PACING: AgentPacing = { msPerChar: 13, beatMs: 800 };

/** Pacing for the still: none of it is watched happening. */
export const AGENT_STILL_PACING: AgentPacing = { msPerChar: 0, beatMs: 0 };

/** Dwell between beats, so a viewer can read each one. */
export async function agentBeat(
  page: Page,
  pacing: AgentPacing,
  factor = 1,
): Promise<void> {
  if (pacing.beatMs > 0) {
    await page.waitForTimeout(pacing.beatMs * factor);
  }
}

/** One outgoing tool call, rendered the way an agent sends it. */
function callLines(name: string, args: Record<string, unknown>): string[] {
  return [
    `▸ tools/call  ${name}`,
    ...JSON.stringify(args, null, 2)
      .split("\n")
      .map((line) => `  ${line}`),
  ];
}

/**
 * The first sentence of a message, marked as clipped.
 *
 * The tool's `message` is three sentences of instruction to a model and would
 * take four lines of the pane. Truncating a real string is the only editing
 * done to any value here, and the ellipsis says so.
 */
function firstSentence(message: string): string {
  const end = message.indexOf(". ");
  return end === -1 ? message : `${message.slice(0, end + 1)} …`;
}

/** The `approval_pending` answer, rendered from the real response. */
function pendingLines(out: McpQueryOutput): string[] {
  return [
    `◂ ${out.status}   matched ${out.approval_pattern ?? ""}`,
    `  query_uid      ${out.query_uid ?? ""}`,
    `  execution_id   ${out.execution_id ?? ""}`,
    `  ${firstSentence(out.message ?? "")}`,
  ];
}

/** The final answer, rendered from the real response. */
function resultLines(out: McpQueryOutput): string[] {
  const heading =
    `◂ ${out.status}   row_count ${out.row_count}` +
    (out.rows_affected === undefined
      ? ""
      : `   rows_affected ${out.rows_affected}`) +
    `   duration_ms ${out.duration_ms}`;

  const table = formatResult(
    (out.columns ?? []).map((name) => ({ name })),
    out.rows ?? [],
  ).map((line) => `  ${line}`);

  return [heading, ...table];
}

/** Write a response block: its heading in `cls`, its body plain. */
async function writeBlock(
  page: Page,
  lines: string[],
  cls: "ok" | "warn",
): Promise<void> {
  await writeAgentLines(page, lines.slice(0, 1), cls);
  await writeAgentLines(page, lines.slice(1), "json");
}

/** The one muted line of context: where the agent is talking to, and as what. */
export async function drawAgentIntro(page: Page): Promise<void> {
  await writeAgentLines(
    page,
    ["POST /api/v1/mcp   ·   Authorization: Bearer dbb_…"],
    "muted",
  );
}

/** Compose the `query` call. Nothing has been sent yet when this returns. */
export async function drawQueryCall(
  page: Page,
  pacing: AgentPacing,
): Promise<void> {
  await typeAgentLines(
    page,
    callLines("query", { database: SERVER_NAME, sql: AGENT_UPDATE_SQL }),
    pacing.msPerChar,
  );
}

/**
 * Send it.
 *
 * Deliberately not awaited by the caller: `query` blocks for ten seconds and
 * then answers `approval_pending`, and in the meantime the loopback connection
 * it opened is what the browser half has to find.
 */
export function startAgentHold(mcp: McpClient): Promise<McpQueryOutput> {
  return mcp.query(SERVER_NAME, AGENT_UPDATE_SQL);
}

/**
 * Render the parked statement's answer.
 *
 * Awaiting the promise here is what makes the block real rather than
 * anticipated: these are the values the tool actually returned.
 */
export async function drawPending(
  page: Page,
  pending: Promise<McpQueryOutput>,
): Promise<McpQueryOutput> {
  const out = await pending;
  if (out.status !== "approval_pending") {
    throw new Error(
      `showcase: the agent's UPDATE was not held (status ${out.status}) — is DBB_APPROVAL_ENABLED set, and does the grant carry the pattern?`,
    );
  }

  await writeBlock(page, pendingLines(out), "warn");

  return out;
}

/** Compose the `await_approval` call — what the pending message told it to do. */
export async function drawAwaitCall(
  page: Page,
  out: McpQueryOutput,
  pacing: AgentPacing,
): Promise<void> {
  await typeAgentLines(
    page,
    callLines("await_approval", {
      execution_id: out.execution_id,
      timeout_seconds: AWAIT_TIMEOUT_SECONDS,
    }),
    pacing.msPerChar,
  );
}

/** Render the released statement's answer. */
export async function drawResult(
  page: Page,
  out: McpQueryOutput,
): Promise<void> {
  await writeBlock(page, resultLines(out), "ok");
}

/**
 * Park the watch panel at the top of the frame.
 *
 * The held statement and its Approve button render inside it, and by default
 * they sit below the fold.
 */
export async function focusWatchPanel(page: Page): Promise<void> {
  await page
    .getByTestId("connection-watch-panel")
    .evaluate((el) => el.scrollIntoView({ block: "start" }));
}
