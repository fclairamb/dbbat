/**
 * The four website screenshots.
 *
 * These are captures, not assertions — but they are still real: every page is
 * the production build of the app, driven through the real UI, against a live
 * demo-mode instance whose query history was produced by an actual PostgreSQL
 * client going through the proxy (see lib/traffic.ts).
 *
 * The `expect`s are here so a UI change that breaks the scenario fails loudly
 * instead of silently producing a screenshot of an empty page — that is what
 * makes this suite usable as a rot guard on `front/` changes.
 */
import { DEFINITION_NAME, SERVER_NAME } from "./config";
import { asset, expect, settle, test } from "./fixtures";

test.describe.configure({ mode: "serial" });

test("screenshot: adding a server", async ({ showcasePage: page }) => {
  await page.goto("servers");
  await settle(page);

  await page.getByTestId("add-database-button").click();
  await expect(page.getByTestId("database-name-input")).toBeVisible();

  // Filled but not submitted: demo mode only accepts one upstream, and the
  // screenshot is about the form, not about creating a second server.
  await page.getByTestId("database-name-input").fill("orders-replica");
  await page.locator("#description").fill("Read replica, EU-West");
  await page.locator("#host").fill("orders-replica.internal");
  await page.locator("#port").fill("5432");
  await page.locator("#databaseName").fill("orders");
  await page.locator("#username").fill("dbbat_proxy");
  await page.locator("#password").fill("s3cret-rotated-nightly");

  // Filling the tail of the form scrolls the dialog's own scroll container;
  // rewind it so the frame shows the protocol and the name, not a mid-form
  // slice.
  await page
    .locator('[role="dialog"] .overflow-y-auto')
    .first()
    .evaluate((el) => {
      el.scrollTop = 0;
    });

  await settle(page, 250);
  await page.screenshot({ path: asset("add-server.png") });

  await page.getByRole("button", { name: "Cancel" }).click();
});

test("screenshot: requesting a grant", async ({ showcasePage: page }) => {
  await page.goto("grant-requests");
  await settle(page);

  await page.getByTestId("request-grant-button").click();
  await expect(page.getByTestId("grant-request-justification")).toBeVisible();

  await page.getByTestId("grant-request-definition").click();
  await page.getByRole("option", { name: DEFINITION_NAME }).click();

  await page.getByTestId("grant-request-database").click();
  await page.getByRole("option", { name: SERVER_NAME }).click();

  await page
    .getByTestId("grant-request-justification")
    .fill(
      "INC-4821: reconciling duplicated invoice rows reported by finance. Need write access for the fix window.",
    );

  await settle(page, 250);
  await page.screenshot({ path: asset("grant-request.png") });

  await page.keyboard.press("Escape");
});

test("screenshot: the query list", async ({ showcasePage: page }) => {
  await page.goto("queries");
  await settle(page);

  // The traffic generated in global-setup has to be there, otherwise the
  // screenshot is of an empty table.
  const rows = page.locator("tbody tr");
  await expect(rows.first()).toBeVisible();
  expect(await rows.count()).toBeGreaterThan(1);

  await settle(page, 250);
  await page.screenshot({ path: asset("query-list.png") });
});

test("screenshot: a query's captured result rows", async ({
  showcasePage: page,
}) => {
  await page.goto("queries");
  await settle(page);

  // The SELECT with the widest result set — the one worth showing.
  const row = page
    .locator("tbody tr", { hasText: "FROM customers ORDER BY mrr_eur" })
    .first();
  await expect(row).toBeVisible();
  await row.locator("a").first().click();
  await settle(page);

  // Result-row capture is on by default; if it produced nothing, the capture
  // would be pointless, so fail rather than emit it.
  const resultTable = page.locator("table").last();
  await expect(resultTable.locator("tbody tr").first()).toBeVisible();
  await expect(page.getByText("Northwind Logistics")).toBeVisible();

  await settle(page, 250);
  await page.screenshot({ path: asset("query-results.png") });
});
