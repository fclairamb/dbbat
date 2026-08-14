/**
 * The MCP approval-hold video.
 *
 * The same hold as ./approval-video.spec.ts, from the other side: the statement
 * is issued by an **AI agent over MCP** rather than by a human at `psql`. That
 * is the demo the MCP feature exists for — an agent writes a destructive
 * statement, the statement freezes mid-flight, and a second human decides — and
 * it is only possible because dbbat is a proxy: the agent's `query` call and the
 * row the operator is looking at are the same object.
 *
 * What is real: the JSON-RPC calls against `POST /api/v1/mcp` with a `dbb_`
 * key, the loopback proxy session dbbat opens to run the statement, the hold,
 * the approve click, and the rows the released `UPDATE` returned. What is
 * drawn: the agent pane (./lib/agent.ts) and the mouse pointer
 * (./lib/cursor.ts).
 *
 * The clip opens on the connections list rather than on the session's own page,
 * and that is not a stylistic choice: MCP runs each statement on its own
 * loopback connection, so the session does not exist until the call has been
 * sent. Composing the call first and then opening the session dbbat parked it
 * on is the real order of events — see ./lib/agent.ts on why the pane survives
 * the navigation.
 *
 * The clip's poster is its own still — ./mcp-approval-poster.spec.ts — and the
 * two share their choreography through ./lib/agent-hold.ts.
 */
import { test as base, expect } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { ADMIN, API_URL, BASE_URL, WORK_DIR, shouldFreezeClock } from "./config";
import { readState } from "./state";
import { freezeClock } from "./fixtures";
import { ShowcaseApi, waitForLiveConnection } from "./lib/api";
import { installAgentPane } from "./lib/agent";
import { cursorClick, installFakeCursor } from "./lib/cursor";
import {
  AGENT_CLIP_PACING,
  AGENT_PANE_TITLE,
  AWAIT_TIMEOUT_SECONDS,
  agentBeat,
  drawAgentIntro,
  drawAwaitCall,
  drawPending,
  drawQueryCall,
  drawResult,
  focusWatchPanel,
  startAgentHold,
} from "./lib/agent-hold";
import { McpClient } from "./lib/mcp";
import { seedUpstream } from "./lib/traffic";

const VIDEO_DIR = join(WORK_DIR, "video");

const test = base.extend({});

test.describe.configure({ mode: "serial" });

test("video: an agent's UPDATE is held for approval and released from the UI", async ({
  page,
}) => {
  test.setTimeout(240_000);
  mkdirSync(VIDEO_DIR, { recursive: true });

  const api = new ShowcaseApi(API_URL);
  await api.login(ADMIN.username, ADMIN.password);

  const scenario = readState();
  const mcp = new McpClient(scenario.connectorApiKey);

  // Put the two `starter` rows back: the earlier projects' own UPDATE already
  // flipped them, and a released statement reporting `rows_affected 0` would
  // undersell what just happened. Written straight to the upstream, so it adds
  // nothing to dbbat's own connection or query lists.
  await seedUpstream();

  // Land the browser signed in: seeding the session token skips a login screen
  // nobody wants in a 30-second clip.
  await page.addInitScript(
    ([key, token]) => localStorage.setItem(key, token),
    ["dbbat_session_token", api.sessionToken] as const,
  );
  await installFakeCursor(page);
  await installAgentPane(page, AGENT_PANE_TITLE);
  if (shouldFreezeClock("video")) {
    await freezeClock(page);
  }

  // 1. The agent composes its call, watched from the connections list — the
  //    session it is about to open will appear here.
  await page.goto(new URL("connections", BASE_URL).toString());
  await page.waitForLoadState("networkidle");
  await expect(page.getByRole("heading", { name: "Connections" })).toBeVisible();
  await drawAgentIntro(page);
  await agentBeat(page, AGENT_CLIP_PACING, 0.5);
  await drawQueryCall(page, AGENT_CLIP_PACING);

  // 2. And sends it. Not awaited: it answers `approval_pending` ten seconds
  //    from now, and until then its connection is what we go and watch.
  const pending = startAgentHold(mcp);

  try {
    const uid = await waitForLiveConnection(
      api,
      scenario.serverUid,
      scenario.connectorUid,
    );
    await agentBeat(page, AGENT_CLIP_PACING);

    // 3. The operator opens the session dbbat parked the statement on.
    await page.goto(new URL(`connections/${uid}?watch=1`, BASE_URL).toString());
    await page.waitForLoadState("networkidle");
    await expect(page.getByTestId("watch-toggle")).toHaveText(/Stop watching/);
    await expect(page.getByTestId("watch-stream-status")).toBeVisible();
    await focusWatchPanel(page);

    const approvals = page.getByTestId("pending-approvals");
    await expect(approvals).toBeVisible({ timeout: 30_000 });
    await expect(approvals).toContainText("UPDATE customers");

    // 4. Ten seconds in, the agent gets a structured answer rather than a hang.
    const held = await drawPending(page, pending);
    await agentBeat(page, AGENT_CLIP_PACING);

    // 5. It goes back to waiting rather than retrying — the contract the
    //    pending message spells out.
    await drawAwaitCall(page, held, AGENT_CLIP_PACING);
    const settled = mcp.awaitApproval(
      held.execution_id ?? "",
      AWAIT_TIMEOUT_SECONDS,
    );
    await agentBeat(page, AGENT_CLIP_PACING);

    // 6. The second pair of eyes releases it.
    const approve = approvals.locator('[data-testid^="approve-query-"]').first();
    await expect(approve).toBeVisible();
    await cursorClick(page, approve);

    const result = await settled;
    expect(result.status).toBe("ok");
    await drawResult(page, result);

    // The hold is gone, the live feed remembers who released it, and the agent
    // has its rows.
    await expect(approvals).toBeHidden({ timeout: 20_000 });
    await expect(
      page.getByTestId("watch-feed").getByTestId("approval-status-approved"),
    ).toBeVisible({ timeout: 20_000 });
    // Dwell on the payoff: the rows in the pane are what the clip is for, and
    // they land in its last three seconds.
    await page.waitForTimeout(3200);
  } finally {
    // Nothing of ours to close: the loopback session is dbbat's and it ended
    // when the statement did. A failure before the approve click leaves the
    // execution parked, which the 30-minute reaper collects — and the whole
    // instance is torn down with the run anyway.
    await pending.catch(() => undefined);
  }

  // Finalise the recording, then park the raw WebM where the transcode step
  // (scripts/showcase.sh) picks it up.
  const video = page.video();
  await page.close();
  if (video) {
    await video.saveAs(join(VIDEO_DIR, "mcp-approval-hold.webm"));
  }
});
