import { test, expect } from "./fixtures";

test.describe("Grant Definitions Management", () => {
  test("edit dialog is pre-populated with the definition's existing values", async ({
    authenticatedPage,
  }) => {
    const page = authenticatedPage;
    const name = `E2E Edit Prefill ${Date.now()}`;

    await page.goto("grant-definitions");
    await page.waitForLoadState("networkidle");

    // Create a definition with a distinctive name and a checked control.
    await page.getByTestId("create-grant-definition-button").click();
    await page.waitForSelector('[role="dialog"]');
    await page.getByTestId("grant-definition-name").fill(name);
    // Check the read_only control so we can assert it stays checked on edit.
    const readOnly = page.getByLabel(/read.only/i);
    if (await readOnly.isVisible()) {
      await readOnly.check();
    }
    await page.getByTestId("grant-definition-submit").click();

    // Wait for the create dialog to close and the row to appear.
    await expect(page.getByRole("dialog")).toBeHidden();
    const row = page.locator("tr", { hasText: name });
    await expect(row).toBeVisible();

    // Open the edit dialog for that row.
    await row.locator('[data-testid^="edit-grant-definition-"]').click();
    await page.waitForSelector('[role="dialog"]');

    // The core assertion: the name field must be pre-filled with the existing
    // value, not blank (the create/edit stale-state bug).
    await expect(page.getByTestId("grant-definition-name")).toHaveValue(name);
    await expect(page.getByText("Edit definition")).toBeVisible();
    if (await readOnly.isVisible()) {
      await expect(readOnly).toBeChecked();
    }

    // Close the edit dialog.
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toBeHidden();

    // The "New Definition" flow must still open with an empty name field
    // (no bleed-through from the edited definition).
    await page.getByTestId("create-grant-definition-button").click();
    await page.waitForSelector('[role="dialog"]');
    await expect(page.getByTestId("grant-definition-name")).toHaveValue("");
    await expect(page.getByText("New definition")).toBeVisible();
  });
});
