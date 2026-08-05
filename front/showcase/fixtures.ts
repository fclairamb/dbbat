/**
 * Showcase fixtures.
 *
 * Reuses the e2e suite's `loginAs` helper (../e2e/fixtures.ts) so the login
 * flow has exactly one implementation, then layers on the two things the
 * marketing captures need and tests do not: a pinned clock and a drawn cursor.
 */
import { test as base, expect, type Page } from "@playwright/test";
import { loginAs } from "../e2e/fixtures";
import { ADMIN, OUT_DIR, fixedTime, shouldFreezeClock } from "./config";
import { installFakeCursor } from "./lib/cursor";
import { join } from "node:path";

/**
 * Pin Date.now() without stopping timers.
 *
 * `clock.install()` + `pauseAt()` would also freeze setTimeout/setInterval,
 * which stalls React Query's refetching and the watch panel's reconnect
 * backoff. `setFixedTime` only pins the clock, which is all the "3 minutes
 * ago" labels read.
 */
export async function freezeClock(page: Page): Promise<void> {
  await page.clock.setFixedTime(fixedTime());
}

export const test = base.extend<{ showcasePage: Page }>({
  showcasePage: async ({ page }, use) => {
    if (shouldFreezeClock("screenshot")) {
      await freezeClock(page);
    }
    await installFakeCursor(page);
    await loginAs(page, ADMIN.username, ADMIN.password);
    // eslint-disable-next-line react-hooks/rules-of-hooks
    await use(page);
  },
});

/** Absolute path of a finished asset inside the showcase output directory. */
export function asset(name: string): string {
  return join(OUT_DIR, name);
}

/**
 * Settle the page before a capture: network quiet, fonts loaded, and one
 * animation frame so Tailwind transitions have finished.
 */
export async function settle(page: Page, extraMs = 400): Promise<void> {
  await page.waitForLoadState("networkidle");
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(extraMs);
}

export { expect };
