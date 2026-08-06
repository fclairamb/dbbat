/**
 * The approval-hold poster.
 *
 * This frame is the `<video>` element's `poster=` and what the website shows a
 * visitor who asked for reduced motion — so it is a *still*, and it is held to
 * the same bar as the other four: regenerating it against an unchanged UI has
 * to produce a byte-identical PNG.
 *
 * It used to be grabbed mid-recording by ../approval-video.spec.ts, which is
 * why it was the last capture still churning. A clip does not pin its clock
 * (watching a live "held for Ns" counter tick is the point of that one), and
 * the frame carries two things that move with the wall clock: the counter
 * itself, and the grant's rendered validity window. Neither can be normalised
 * afterwards the way lib/normalise.ts restates the query timeline, because the
 * frame is what renders them.
 *
 * So the poster is its own scenario, and the two moving parts are pinned
 * before the frame instead of restated after it:
 *
 * - the grant's window is dated from SHOWCASE_EPOCH *before the page loads it*,
 *   and put back verbatim the moment the frame is taken — see pinGrantWindow();
 * - the browser clock is pinned to the held statement's own `executed_at` plus
 *   a fixed offset, so "held 12s" is a constant rather than however long the
 *   capture took to reach the shutter.
 *
 * Everything else is exactly the clip's scenario, shared with it through
 * lib/hold.ts: a real session through the proxy, a real `UPDATE` parked on a
 * real approval pattern, and a real second pair of eyes releasing it.
 */
import type { Client } from "pg";
import type { Page } from "@playwright/test";
import { ADMIN, API_URL } from "./config";
import { asset, expect, settle, test } from "./fixtures";
import { readState } from "./state";
import { ShowcaseApi, waitForLiveConnection } from "./lib/api";
import { STILL_PACING, playHold } from "./lib/hold";
import {
  pinGrantWindow,
  restoreGrantWindow,
  type GrantWindow,
} from "./lib/normalise";
import { proxyClient } from "./lib/traffic";
import { writeLine } from "./lib/terminal";

/**
 * How long the poster says the statement has been parked.
 *
 * Long enough to read as "a human is deciding", short enough to stay in
 * seconds — HeldFor switches to "1m 2s" past the minute, and a poster showing
 * a minute-old hold tells a slightly different story than the clip does.
 */
const HELD_FOR_S = 12;

test.describe.configure({ mode: "serial" });

test("screenshot: an UPDATE held for approval", async ({
  showcasePage: page,
}) => {
  test.setTimeout(180_000);

  const api = new ShowcaseApi(API_URL);
  await api.login(ADMIN.username, ADMIN.password);
  const scenario = readState();

  // A live session through the proxy — this is what the operator is looking at.
  const client: Client = proxyClient();
  await client.connect();

  let grantWindow: GrantWindow | undefined;

  try {
    await client.query("SELECT 1");
    const uid = await waitForLiveConnection(
      api,
      scenario.serverUid,
      scenario.connectorUid,
    );

    // Before the page fetches it: the Grant card renders this window, and the
    // session above already authenticated through the real one.
    grantWindow = await pinGrantWindow(scenario.grantUid);

    await page.goto(`connections/${uid}?watch=1`);
    await settle(page);
    await expect(page.getByTestId("watch-toggle")).toHaveText(/Stop watching/);
    await expect(page.getByTestId("watch-stream-status")).toBeVisible();

    const { held } = await playHold(page, client, STILL_PACING);

    const pending = page.getByTestId("pending-approvals");
    await expect(pending).toBeVisible({ timeout: 30_000 });
    await expect(pending).toContainText("UPDATE customers");
    // The read has to have reached the live feed too: a poster whose "Live
    // queries" list is still saying "Waiting for queries…" is not the picture.
    await expect(page.getByTestId("watch-feed")).toContainText(
      "SELECT company",
    );

    await pinHeldFor(page, api, uid);

    await settle(page, 250);
    await page.screenshot({ path: asset("approval-hold-poster.png") });

    // The frame is taken; give the grant its real window back before anything
    // else needs to authenticate through it (the video project is next).
    await restoreGrantWindow(scenario.grantUid, grantWindow);
    grantWindow = undefined;

    // The second pair of eyes releases it. Not part of the frame — it is here
    // so a UI change that breaks the release fails this suite loudly, the same
    // way the other captures assert their scenario actually happened.
    await pending.locator('[data-testid^="approve-query-"]').first().click();
    const result = await held;
    await writeLine(page, `UPDATE ${result.rowCount ?? 0}`, "ok");
    await expect(pending).toBeHidden({ timeout: 20_000 });
  } finally {
    if (grantWindow) {
      await restoreGrantWindow(scenario.grantUid, grantWindow).catch(
        () => undefined,
      );
    }
    await client.end().catch(() => undefined);
  }
});

/**
 * Pin the browser clock to the hold's own start plus HELD_FOR_S.
 *
 * The counter is `Date.now() - executed_at`, recomputed on a one-second
 * interval, and `executed_at` is a real timestamp on a row this run just
 * produced. The showcase's usual pin (SHOWCASE_EPOCH + 30s) is hours behind
 * that, which would render the hold as `held 0s` — the label clamps at zero
 * rather than going negative, but it would be a lie either way.
 *
 * Rewriting the row instead would be the tidier half of the trick, except the
 * pending list is fetched once and refreshed only when the stream says
 * something changed, so the page would keep showing the value it already had.
 * Moving the clock is read by the very next interval tick.
 */
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
