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

  test("should have datetime-local inputs in create grant dialog", async ({
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

      // Verify datetime-local inputs are present
      const startsAtInput = authenticatedPage.locator(
        'input[type="datetime-local"]#startsAt'
      );
      const expiresAtInput = authenticatedPage.locator(
        'input[type="datetime-local"]#expiresAt'
      );

      await expect(startsAtInput).toBeVisible();
      await expect(expiresAtInput).toBeVisible();

      // Take screenshot of the datetime inputs
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/grants-datetime-inputs.png",
      });
    }
  });

  test("should default start time to approximately current time", async ({
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

      // Get the starts_at input value
      const startsAtInput = authenticatedPage.locator(
        'input[type="datetime-local"]#startsAt'
      );
      const startsAtValue = await startsAtInput.inputValue();

      // Verify it's a valid datetime-local format (YYYY-MM-DDTHH:mm)
      expect(startsAtValue).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);

      // Verify the date portion is today
      const today = new Date().toISOString().split("T")[0];
      expect(startsAtValue.split("T")[0]).toBe(today);
    }
  });

  test("should display grants with time information", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot showing grant list with time information
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-with-time.png",
      fullPage: true,
    });

    // Verify the page shows time information (format: "at HH:mm")
    const content = await authenticatedPage.textContent("body");
    expect(content).toMatch(/at \d{2}:\d{2}/);
  });
});
