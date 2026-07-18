import { type Page } from "@playwright/test";
import { test, expect } from "./fixtures";

/**
 * Coverage for the "Approve and enable auto-approve" combined action on the
 * grant-requests admin view: it should approve the pending request AND flip
 * the underlying definition's auto-approve on, in one click, so future
 * requests against it skip review entirely.
 */

const DEFS_URL = "grant-definitions";

async function openCreateDialog(page: Page) {
  await page.getByTestId("create-grant-definition-button").click();
  await page.waitForSelector('[role="dialog"]');
  await expect(page.getByTestId("grant-definition-name")).toBeVisible();
}

async function submitDialog(page: Page) {
  await page.getByTestId("grant-definition-submit").click();
  await expect(page.locator('[role="dialog"]')).toBeHidden();
}

async function submitRequest(
  page: Page,
  opts: { definitionName: string; justification: string }
) {
  await page.goto("grant-requests");
  await page.waitForLoadState("networkidle");
  await page.getByTestId("request-grant-button").click();
  await page.waitForSelector('[role="dialog"]');

  await page.getByTestId("grant-request-definition").click();
  await page.getByRole("option", { name: opts.definitionName }).click();

  await page.getByTestId("grant-request-database").click();
  await page.getByRole("option").first().click();

  await page.getByTestId("grant-request-justification").fill(opts.justification);
  await page.getByTestId("grant-request-submit").click();
  await expect(page.locator('[role="dialog"]')).toBeHidden();
}

test.describe("Grant Requests: approve and enable auto-approve", () => {
  test("clicking the combined action approves the request and enables auto-approve on its definition", async ({
    authenticatedPage: page,
  }) => {
    // Create a manual (non-auto-approve) definition.
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    const name = `E2E ApproveEnable ${Date.now()}`;
    await openCreateDialog(page);
    await page.getByTestId("grant-definition-name").fill(name);
    await page
      .getByTestId("grant-definition-description")
      .fill("manual definition, promoted via approve+enable");
    await page.getByTestId("grant-definition-duration-value").fill("1");
    await page.getByTestId("grant-definition-duration-unit").click();
    await page.getByRole("option", { name: "Hours" }).click();
    await page.locator("#def-read_only").click();
    await submitDialog(page);

    // Submit a request against it — since it's not auto-approved, this lands
    // pending rather than being approved instantly.
    await submitRequest(page, {
      definitionName: name,
      justification: "E2E approve+enable request one",
    });

    // As admin, find the pending request and use the combined action.
    await page.goto("grant-requests");
    await page.waitForLoadState("networkidle");

    const pendingRow = page.locator("tr", { hasText: name }).first();
    await expect(pendingRow).toBeVisible();
    await expect(pendingRow).toContainText("pending");

    const approveEnableButton = pendingRow.locator(
      '[data-testid^="approve-and-enable-auto-approve-"]'
    );
    await expect(approveEnableButton).toBeVisible();
    await approveEnableButton.click();

    // The request should transition to approved.
    await expect(pendingRow).toContainText("approved", { timeout: 10000 });

    // Definition badge in this same row should now show "auto-approve".
    await expect(pendingRow).toContainText("auto-approve");

    // The grant-definitions table should also reflect the toggle as on.
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");
    const defRow = page.locator("tr", { hasText: name });
    const toggle = defRow.locator(
      '[data-testid^="grant-definition-auto-approve-"]'
    );
    await expect(toggle).toHaveAttribute("data-state", "checked");

    // A second request against the now-auto-approved definition should be
    // approved instantly, confirming the definition update actually stuck.
    await submitRequest(page, {
      definitionName: name,
      justification: "E2E approve+enable request two",
    });
    await page.goto("grant-requests");
    await page.waitForLoadState("networkidle");
    await page.getByRole("button", { name: "All" }).click();
    const rows = page.locator("tr", { hasText: name });
    await expect(rows).toHaveCount(2);
    for (const row of await rows.all()) {
      await expect(row).toContainText("approved");
    }
  });
});
