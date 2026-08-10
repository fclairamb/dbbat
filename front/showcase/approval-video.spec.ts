/**
 * The approval-hold video.
 *
 * This is the one scenario the e2e suite cannot do (see the note at the top of
 * ../e2e/approvals.spec.ts): a hold is a *live database session* parked on a
 * human, and Playwright alone cannot create one. The showcase runner can,
 * because it drives a real `pg` client through the proxy in the same process
 * as the browser.
 *
 * What is real: the connection, the SELECT, the UPDATE, the hold, the approve
 * click, the rows the statement touched. What is drawn: the terminal pane (a
 * real terminal emulator is not reproducible in CI) and the mouse pointer
 * (Playwright's recorder does not capture one).
 *
 * The clip's poster is *not* taken from here — it is its own still, captured
 * by ./approval-poster.spec.ts against a pinned clock so it diffs cleanly. The
 * two share their choreography through ./lib/hold.ts, so the frame the website
 * shows under `prefers-reduced-motion` still depicts this session.
 */
import type { Client } from "pg";
import { test as base, expect } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { ADMIN, API_URL, BASE_URL, WORK_DIR, shouldFreezeClock } from "./config";
import { readState } from "./state";
import { freezeClock } from "./fixtures";
import { ShowcaseApi, waitForLiveConnection } from "./lib/api";
import { cursorClick, installFakeCursor } from "./lib/cursor";
import { CLIP_PACING, playHold } from "./lib/hold";
import { writeLine } from "./lib/terminal";
import { proxyClient, seedUpstream } from "./lib/traffic";

const VIDEO_DIR = join(WORK_DIR, "video");

const test = base.extend({});

test.describe.configure({ mode: "serial" });

test("video: an UPDATE is held for approval and released from the UI", async ({
  page,
}) => {
  test.setTimeout(180_000);
  mkdirSync(VIDEO_DIR, { recursive: true });

  const api = new ShowcaseApi(API_URL);
  await api.login(ADMIN.username, ADMIN.password);

  // Put the two `starter` rows back. The poster project runs first and its own
  // UPDATE already flipped them, so without this the released statement here
  // reports `UPDATE 0` — true, and a poor advertisement for the thing the clip
  // is about. Written straight to the upstream, so it adds nothing to dbbat's
  // connection or query lists.
  await seedUpstream();

  // A live session through the proxy — this is what the operator will watch.
  const client: Client = proxyClient();
  await client.connect();

  const scenario = readState();

  try {
    await client.query("SELECT 1");
    const uid = await waitForLiveConnection(
      api,
      scenario.serverUid,
      scenario.connectorUid,
    );

    // Land the browser straight on the watch panel: seeding the session token
    // skips a login screen nobody wants in a 25-second clip.
    await page.addInitScript(
      ([key, token]) => localStorage.setItem(key, token),
      ["dbbat_session_token", api.sessionToken] as const,
    );
    await installFakeCursor(page);
    if (shouldFreezeClock("video")) {
      await freezeClock(page);
    }

    await page.goto(new URL(`connections/${uid}?watch=1`, BASE_URL).toString());
    await page.waitForLoadState("networkidle");
    await expect(page.getByTestId("watch-toggle")).toHaveText(/Stop watching/);
    await expect(page.getByTestId("watch-stream-status")).toBeVisible();

    const { held } = await playHold(page, client, CLIP_PACING);

    const pending = page.getByTestId("pending-approvals");
    await expect(pending).toBeVisible({ timeout: 30_000 });
    await expect(pending).toContainText("UPDATE customers");
    await page.waitForTimeout(1400);

    // 3. The second pair of eyes releases it.
    const approve = pending.locator('[data-testid^="approve-query-"]').first();
    await expect(approve).toBeVisible();
    await cursorClick(page, approve);

    const result = await held;
    await writeLine(page, `UPDATE ${result.rowCount ?? 0}`, "ok");

    // The hold is gone, the live feed remembers who released it, and the
    // statement has landed in the session's query history.
    await expect(pending).toBeHidden({ timeout: 20_000 });
    await expect(
      page.getByTestId("watch-feed").getByTestId("approval-status-approved"),
    ).toBeVisible({ timeout: 20_000 });
    await expect(
      page.locator("tbody tr", { hasText: "UPDATE customers" }).first(),
    ).toBeVisible();
    await page.waitForTimeout(1600);
  } finally {
    await client.end().catch(() => undefined);
  }

  // Finalise the recording, then park the raw WebM where the transcode step
  // (scripts/showcase.sh) picks it up.
  const video = page.video();
  await page.close();
  if (video) {
    await video.saveAs(join(VIDEO_DIR, "approval-hold.webm"));
  }
});
