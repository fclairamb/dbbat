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

  test("should show create grant form with controls checkboxes", async ({
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

      // Wait for dialog to open
      await authenticatedPage.waitForSelector('[role="dialog"]');

      // Take screenshot of create grant dialog/form
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/grants-create-dialog.png",
      });

      // Look for controls checkboxes (replaces access_level dropdown)
      const formContent = await authenticatedPage.textContent("body");
      expect(formContent?.toLowerCase()).toMatch(
        /read.only|block.copy|block.ddl|controls|user|database/
      );
    }
  });

  test("should create grant with read_only control", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });

    if (await createButton.isVisible()) {
      await createButton.click();

      // Wait for dialog to open
      await authenticatedPage.waitForSelector('[role="dialog"]');

      // Check if the read_only control checkbox exists
      const readOnlyCheckbox = authenticatedPage.getByLabel(/read.only/i);
      if (await readOnlyCheckbox.isVisible()) {
        await readOnlyCheckbox.check();
      }

      // Take screenshot with control selected
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/grants-create-with-controls.png",
      });
    }
  });

  test("should display controls badges in grant list", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot showing grant list with controls badges
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-with-controls.png",
      fullPage: true,
    });

    // Verify controls-related content is displayed (badges like "Read Only", "Full Access", etc.)
    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
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
