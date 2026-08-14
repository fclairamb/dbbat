/**
 * The MCP approval-hold poster.
 *
 * The `<video poster>` for ./mcp-approval-video.spec.ts's clip, and what the
 * website shows a visitor who asked for reduced motion — so it is a *still*,
 * held to the same bar as the other captures: regenerating it against an
 * unchanged UI has to produce a byte-identical PNG.
 *
 * The two moving parts are pinned exactly as ./approval-poster.spec.ts pins
 * them, and for the reasons spelled out in its header: the grant's rendered
 * validity window is dated from SHOWCASE_EPOCH before the page loads it and put
 * straight back, and the browser clock is pinned to the held statement's own
 * `executed_at` plus a fixed offset so "held 12s" is a constant.
 *
 * Everything else is the clip's scenario, walked in the same order through
 * ./lib/agent-hold.ts: a real MCP call, a real loopback proxy session, a real
 * hold, and a real second pair of eyes releasing it.
 */
import type { Page } from "@playwright/test";
import { ADMIN, API_URL } from "./config";
import { asset, expect, settle, test } from "./fixtures";
import { readState } from "./state";
import { ShowcaseApi, waitForLiveConnection } from "./lib/api";
import { installAgentPane } from "./lib/agent";
import {
  AGENT_PANE_TITLE,
  AGENT_STILL_PACING,
  AWAIT_TIMEOUT_SECONDS,
  drawAgentIntro,
  drawAwaitCall,
  drawPending,
  drawQueryCall,
  drawResult,
  focusWatchPanel,
  startAgentHold,
} from "./lib/agent-hold";
import { McpClient } from "./lib/mcp";
import {
  pinGrantWindow,
  restoreGrantWindow,
  type GrantWindow,
} from "./lib/normalise";
import { seedUpstream } from "./lib/traffic";

/** How long the poster says the statement has been parked. See the sibling. */
const HELD_FOR_S = 12;

test.describe.configure({ mode: "serial" });

test("screenshot: an agent's UPDATE held for approval", async ({
  showcasePage: page,
}) => {
  test.setTimeout(240_000);

  const api = new ShowcaseApi(API_URL);
  await api.login(ADMIN.username, ADMIN.password);
  const scenario = readState();
  const mcp = new McpClient(scenario.connectorApiKey);

  await seedUpstream();

  // The fixture has already signed the browser in; the pane has to be
  // installed before the navigations below so it survives them.
  await installAgentPane(page, AGENT_PANE_TITLE);

  await page.goto("connections");
  await settle(page);
  await drawAgentIntro(page);
  await drawQueryCall(page, AGENT_STILL_PACING);

  // In flight before the browser has a session to look at — see lib/agent-hold.
  const pending = startAgentHold(mcp);

  let grantWindow: GrantWindow | undefined;

  try {
    const uid = await waitForLiveConnection(
      api,
      scenario.serverUid,
      scenario.connectorUid,
    );

    // Before the page fetches it: the Grant card renders this window, and the
    // loopback session above already authenticated through the real one.
    grantWindow = await pinGrantWindow(scenario.grantUid);

    await page.goto(`connections/${uid}?watch=1`);
    await settle(page);
    await expect(page.getByTestId("watch-toggle")).toHaveText(/Stop watching/);
    await expect(page.getByTestId("watch-stream-status")).toBeVisible();
    await focusWatchPanel(page);

    const approvals = page.getByTestId("pending-approvals");
    await expect(approvals).toBeVisible({ timeout: 30_000 });
    await expect(approvals).toContainText("UPDATE customers");

    const held = await drawPending(page, pending);

    await pinHeldFor(page, api, uid);

    await settle(page, 250);
    await page.screenshot({ path: asset("mcp-approval-hold-poster.png") });

    // The frame is taken; give the grant its real window back before anything
    // else needs to authenticate through it.
    await restoreGrantWindow(scenario.grantUid, grantWindow);
    grantWindow = undefined;

    // Release it. Not part of the frame — it is here so a UI or MCP change that
    // breaks the release fails this suite loudly, the same way the other
    // captures assert their scenario actually happened.
    await drawAwaitCall(page, held, AGENT_STILL_PACING);
    const settled = mcp.awaitApproval(
      held.execution_id ?? "",
      AWAIT_TIMEOUT_SECONDS,
    );
    await approvals.locator('[data-testid^="approve-query-"]').first().click();

    const result = await settled;
    expect(result.status).toBe("ok");
    await drawResult(page, result);
    await expect(approvals).toBeHidden({ timeout: 20_000 });
  } finally {
    if (grantWindow) {
      await restoreGrantWindow(scenario.grantUid, grantWindow).catch(
        () => undefined,
      );
    }
    await pending.catch(() => undefined);
  }
});

/** Pin the browser clock to the hold's own start plus HELD_FOR_S. */
async function pinHeldFor(
  page: Page,
  api: ShowcaseApi,
  connectionUid: string,
): Promise<void> {
  const body = await api.get<{
    queries?: { connection_id: string; executed_at: string }[];
  }>("/queries/pending");
  const hold = (body.queries ?? []).find(
    (q) => q.connection_id === connectionUid,
  );
  if (!hold) {
    throw new Error(
      `showcase: no pending approval on connection ${connectionUid}`,
    );
  }

  await page.clock.setFixedTime(
    new Date(new Date(hold.executed_at).getTime() + HELD_FOR_S * 1000),
  );
  // The counter only reads the clock on its own one-second tick.
  await page.waitForTimeout(1_300);
  await expect(page.getByTestId("approval-held-for")).toHaveText(
    `held ${HELD_FOR_S}s`,
  );
}
