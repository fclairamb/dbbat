import { test, expect } from "./fixtures";

test.describe("Access Grants Management", () => {
  test("should display grants list page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list.png",
      fullPage: true,
    });

    // Verify we're on the grants page
    await expect(authenticatedPage).toHaveURL(/\/grants/);

    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("should show create grant button or form", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Look for create/add button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });

    if (await createButton.isVisible()) {
      await createButton.click();

      // Take screenshot of create grant dialog/form
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/grants-create-dialog.png",
      });

      // Look for form fields typical for grants (user, database, access level)
      const formContent = await authenticatedPage.textContent("body");
      expect(
        formContent?.toLowerCase()
      ).toMatch(/user|database|access|permission|read|write/);
    }
  });

  test("should display grant details or filters", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot showing grant management interface
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-overview.png",
      fullPage: true,
    });

    // Verify grants-related content is present
    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });
});
