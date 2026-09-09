import type { Page } from "@playwright/test";
import { test, expect } from "./fixtures";

/**
 * What an empty statement feed means on a closed session.
 *
 * DBB_CONNECTION_RETENTION can be set longer than
 * DBB_QUERY_STORAGE_RETENTION, so a session between the two windows keeps its
 * ledger row — who connected from where, to which database, under which grant
 * — and loses every statement it ran. Before the split only an *open* session
 * could be in that state; now it is the ordinary condition of every closed
 * session in that band, and the page must not present it as "this session ran
 * nothing".
 *
 * The server decides which of the two it is (`statements_retained` on
 * GET /connections/{uid}, derived from the query window), so the responses are
 * mocked here: a test-mode server has retention disabled, which makes the flag
 * permanently true and the interesting branch unreachable.
 */

const CLOSED_UID = "3f1b0c9e-0000-4000-8000-0000000000a1";

async function mockClosedConnection(
  page: Page,
  uid: string,
  statementsRetained: boolean,
): Promise<void> {
  const connectedAt = new Date(Date.now() - 90 * 24 * 3600_000).toISOString();
  const disconnectedAt = new Date(Date.now() - 89 * 24 * 3600_000).toISOString();

  await page.route(`**/api/v1/connections/${uid}`, (route) =>
    route.fulfill({
      json: {
        uid,
        user_id: "3f1b0c9e-0000-4000-8000-000000000003",
        database_id: "3f1b0c9e-0000-4000-8000-000000000004",
        source_ip: "127.0.0.1",
        connected_at: connectedAt,
        last_activity_at: disconnectedAt,
        disconnected_at: disconnectedAt,
        // A lifetime counter: the session did run statements, they are simply
        // not in the store any more. This is exactly why the counter cannot
        // answer the question on its own.
        queries: 12,
        bytes_transferred: 4096,
        dump: { available: false },
        grant: null,
        statements_retained: statementsRetained,
      },
    }),
  );

  // No statements survive — the state both branches render from.
  await page.route("**/api/v1/queries?**", (route) =>
    route.fulfill({ json: { queries: [] } }),
  );
}

test.describe("Connection statement retention", () => {
  test("a closed session past the statement window says so", async ({
    authenticatedPage,
  }) => {
    await mockClosedConnection(authenticatedPage, CLOSED_UID, false);

    await authenticatedPage.goto(`connections/${CLOSED_UID}`);
    await authenticatedPage.waitForLoadState("domcontentloaded");

    const feed = authenticatedPage.getByTestId("watch-feed");
    await expect(feed).toBeVisible();
    await expect(feed).toContainText("past the retention window");
    await expect(feed).toContainText("lifetime count");
    await expect(feed).not.toContainText("No queries recorded");
  });

  test("a closed session inside the window still reads as having run nothing", async ({
    authenticatedPage,
  }) => {
    await mockClosedConnection(authenticatedPage, CLOSED_UID, true);

    await authenticatedPage.goto(`connections/${CLOSED_UID}`);
    await authenticatedPage.waitForLoadState("domcontentloaded");

    const feed = authenticatedPage.getByTestId("watch-feed");
    await expect(feed).toBeVisible();
    await expect(feed).toContainText("No queries recorded for this connection");
    await expect(feed).not.toContainText("past the retention window");
  });
});
