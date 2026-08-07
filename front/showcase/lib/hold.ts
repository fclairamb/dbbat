/**
 * The held-`UPDATE` scenario, shared by the clip and by the still that fronts
 * it.
 *
 * `approval-hold-poster.png` is the `<video>` element's poster and what the
 * website shows under `prefers-reduced-motion`, so the two have to depict the
 * same session: same statements, same terminal pane, same framing. Keeping the
 * choreography in one place is what stops them drifting apart — they differ
 * only in pacing, because nobody watches a still happen.
 *
 * Everything here is real: the statements are executed by a `pg` client
 * dialling through the proxy, and the hold is a live database session parked
 * on a human. Only the terminal pane is drawn (see ./terminal.ts).
 */
import type { Client, QueryResult } from "pg";
import type { Page } from "@playwright/test";
import { SERVER_NAME } from "../config";
import {
  mountTerminal,
  prompt,
  submit,
  typeText,
  writeLine,
  writeLines,
} from "./terminal";
import { formatResult } from "./traffic";

/** An ordinary read: no approval pattern matches it, nothing is held. */
export const SELECT_SQL =
  "SELECT company, plan, mrr_eur FROM customers WHERE plan = 'starter' ORDER BY company;";

/** The write that matches the grant's pattern, and so parks on a human. */
export const UPDATE_SQL =
  "UPDATE customers SET plan = 'business' WHERE plan = 'starter';";

/** How fast the pane is driven. */
export interface HoldPacing {
  /** Delay between typed characters in the terminal pane. */
  msPerChar: number;
  /** Dwell between beats, so a viewer can read each one. */
  beatMs: number;
}

/** Pacing for the clip: typed at human speed, with room to read. */
export const CLIP_PACING: HoldPacing = { msPerChar: 32, beatMs: 800 };

/** Pacing for the still: none of it is watched happening. */
export const STILL_PACING: HoldPacing = { msPerChar: 0, beatMs: 0 };

async function beat(page: Page, pacing: HoldPacing, factor = 1): Promise<void> {
  if (pacing.beatMs > 0) {
    await page.waitForTimeout(pacing.beatMs * factor);
  }
}

/**
 * Play the scenario up to the moment the `UPDATE` is parked.
 *
 * Returns the client's still-pending promise for the held statement: it
 * resolves when a human releases it, and is deliberately not awaited here.
 */
export async function playHold(
  page: Page,
  client: Client,
  pacing: HoldPacing,
): Promise<{ held: Promise<QueryResult> }> {
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
  await beat(page, pacing, 0.875);

  // 1. An ordinary read: no pattern matches, nothing is held.
  await prompt(page);
  await typeText(page, SELECT_SQL, pacing.msPerChar);
  await submit(page);
  const selectResult = await client.query(SELECT_SQL);
  await writeLines(page, formatResult(selectResult.fields, selectResult.rows));
  await writeLine(page, "");
  await beat(page, pacing, 1.125);

  // 2. A write that matches the grant's approval pattern. The client blocks
  //    here — deliberately not awaited.
  await prompt(page);
  await typeText(page, UPDATE_SQL, pacing.msPerChar);
  await submit(page);
  const held = client.query(UPDATE_SQL);
  await writeLine(page, "…held for approval", "warn");

  return { held };
}
