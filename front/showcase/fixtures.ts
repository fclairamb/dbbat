/**
 * Showcase fixtures.
 *
 * Reuses the e2e suite's `loginAs` helper (../e2e/fixtures.ts) so the login
 * flow has exactly one implementation, then layers on the two things the
 * marketing captures need and tests do not: a pinned clock and a drawn cursor.
 */
import { test as base, expect, type Page } from "@playwright/test";
import { loginAs } from "../e2e/fixtures";
import { ADMIN, OUT_DIR, shouldFreezeClock } from "./config";
import { readState } from "./state";
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
  await page.clock.setFixedTime(new Date(readState().fixedTime));
}

/**
 * Park the adaptive-refresh badge on a value that cannot tick.
 *
 * The observability pages count down to their next auto-refresh ("Next: 9s")
 * on a real one-second setInterval. `clock.setFixedTime` pins Date.now() but
 * deliberately leaves timers running, so the digit is however many seconds the
 * page happened to take to load and settle — the last thing in a still that
 * still moved between two runs. Freezing timers instead would take React's
 * scheduler and every Radix transition down with it.
 *
 * So the stills capture the toggle in its off position, which is a real state
 * of a real control rather than a frozen counter, and is stable by
 * construction. The video never calls this: watching the UI keep up with a
 * live session is the whole point of that one.
 */
async function disableAutoRefresh(page: Page): Promise<void> {
  await page.addInitScript(() => {
    for (const key of ["dbbat.autoRefresh.queries", "dbbat.autoRefresh.connections"]) {
      localStorage.setItem(key, JSON.stringify({ enabled: false }));
    }
  });
}

export const test = base.extend<{ showcasePage: Page }>({
  showcasePage: async ({ page }, use) => {
    if (shouldFreezeClock("screenshot")) {
      await freezeClock(page);
      await disableAutoRefresh(page);
    }
    // No fake cursor here: it is a video affordance. In a still it just leaves
    // an unexplained dot floating over the UI.
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
