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
 */
import type { Client } from "pg";
import { test as base, expect } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import {
  ADMIN,
  API_URL,
  BASE_URL,
  SERVER_NAME,
  WORK_DIR,
  shouldFreezeClock,
} from "./config";
import { readState } from "./state";
import { freezeClock } from "./fixtures";
import { ShowcaseApi, type ConnectionRow } from "./lib/api";
import { cursorClick, installFakeCursor } from "./lib/cursor";
import {
  mountTerminal,
  prompt,
  submit,
  typeText,
  writeLine,
  writeLines,
} from "./lib/terminal";
import { formatResult, proxyClient } from "./lib/traffic";

const VIDEO_DIR = join(WORK_DIR, "video");
const SELECT_SQL =
  "SELECT company, plan, mrr_eur FROM customers WHERE plan = 'starter' ORDER BY company;";
const UPDATE_SQL =
  "UPDATE customers SET plan = 'business' WHERE plan = 'starter';";

const test = base.extend({});

test.describe.configure({ mode: "serial" });

test("video: an UPDATE is held for approval and released from the UI", async ({
  page,
}) => {
  test.setTimeout(180_000);
  mkdirSync(VIDEO_DIR, { recursive: true });

  const api = new ShowcaseApi(API_URL);
  await api.login(ADMIN.username, ADMIN.password);

  // A live session through the proxy — this is what the operator will watch.
  const client: Client = proxyClient();
  await client.connect();

  const scenario = readState();

  try {
    await client.query("SELECT 1");
    const uid = await waitForConnection(
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

    await mountTerminal(page, `psql — ${SERVER_NAME} (via dbbat)`);

    // Park the watch panel at the top of the frame: the held statement and its
    // Approve button render inside it, and by default they sit below the fold.
    await page
      .getByTestId("connection-watch-panel")
      .evaluate((el) => el.scrollIntoView({ block: "start" }));

    await writeLines(
      page,
      [
        `psql (16.4, server 15.8) — connected through dbbat on ${SERVER_NAME}`,
        'Type "help" for help.',
        "",
      ],
      "muted",
    );
    await page.waitForTimeout(700);

    // 1. An ordinary read: no pattern matches, nothing is held.
    await prompt(page);
    await typeText(page, SELECT_SQL);
    await submit(page);
    const selectResult = await client.query(SELECT_SQL);
    await writeLines(
      page,
      formatResult(selectResult.fields, selectResult.rows),
    );
    await writeLine(page, "");
    await page.waitForTimeout(900);

    // 2. A write that matches the grant's approval pattern. The client blocks
    //    here — deliberately not awaited.
    await prompt(page);
    await typeText(page, UPDATE_SQL);
    await submit(page);
    const held = client.query(UPDATE_SQL);
    await writeLine(page, "…held for approval", "warn");

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

    // The hold is gone and the statement has landed in the session's query
    // history. (The live-feed row carries no "Approved" badge — the resolve
    // event does not repeat approval_status — so the disappearance of the
    // held card is the signal to assert on.)
    await expect(pending).toBeHidden({ timeout: 20_000 });
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

/**
 * Poll for the proxy session the `pg` client just opened.
 *
 * Scoped to the showcase's own server row and user, and to a session that has
 * not disconnected — global-setup's traffic run left closed connections behind
 * and picking one of those up would produce a video of a dead page.
 */
async function waitForConnection(
  api: ShowcaseApi,
  serverUid: string,
  userUid: string,
): Promise<string> {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const body = await api.get<{ connections?: ConnectionRow[] }>(
      `/connections?database_id=${serverUid}&user_id=${userUid}`,
    );
    const live = (body.connections ?? []).find(
      (c) => c.disconnected_at === null || c.disconnected_at === undefined,
    );
    if (live) {
      return live.uid;
    }
    await new Promise((resolve) => setTimeout(resolve, 300));
  }
  throw new Error("showcase: the proxy session never showed up in /connections");
}
