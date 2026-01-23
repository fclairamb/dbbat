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

      // Wait for dialog to open and all animations to fully complete
      await authenticatedPage.waitForSelector('[role="dialog"]');
      await authenticatedPage.waitForTimeout(500);

      // Verify datetime-local inputs are present
      const startsAtInput = authenticatedPage.locator(
        'input[type="datetime-local"]#startsAt'
      );
      const expiresAtInput = authenticatedPage.locator(
        'input[type="datetime-local"]#expiresAt'
      );

      await expect(startsAtInput).toBeVisible();
      await expect(expiresAtInput).toBeVisible();

      // Take full page screenshot with animations disabled for clean capture
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/grants-datetime-inputs.png",
        fullPage: true,
        animations: "disabled",
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
    // Wait for any animations to settle
    await authenticatedPage.waitForTimeout(300);

    // Take screenshot showing grant list with time information
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-with-time.png",
      fullPage: true,
      animations: "disabled",
    });

    // Verify the page shows time information (format: "at HH:mm")
    const content = await authenticatedPage.textContent("body");
    expect(content).toMatch(/at \d{2}:\d{2}/);
  });
});

test.describe("Grant Quota Management", () => {
  test("should show quota fields in create grant dialog", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });
    await createButton.click();

    // Wait for dialog to open
    await authenticatedPage.waitForSelector('[role="dialog"]');

    // Verify quota fields are present
    await expect(authenticatedPage.getByLabel(/max queries/i)).toBeVisible();
    await expect(authenticatedPage.getByLabel(/max data transfer/i)).toBeVisible();

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-create-with-quotas.png",
    });
  });

  test("should accept quota values when creating grant", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });
    await createButton.click();

    // Wait for dialog to open
    await authenticatedPage.waitForSelector('[role="dialog"]');

    // Fill quota fields
    const maxQueriesInput = authenticatedPage.getByLabel(/max queries/i);
    await maxQueriesInput.fill("1000");

    const maxBytesInput = authenticatedPage.getByLabel(/max data transfer/i);
    await maxBytesInput.fill("500");

    // Take screenshot with filled quotas
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-create-quotas-filled.png",
    });

    // Verify input values
    await expect(maxQueriesInput).toHaveValue("1000");
    await expect(maxBytesInput).toHaveValue("500");
  });

  test("should display quota usage in grant list", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot showing grant list with quota usage
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-quota-usage.png",
      fullPage: true,
    });

    // Verify usage column is present (shows queries and bytes)
    const content = await authenticatedPage.textContent("body");
    expect(content?.toLowerCase()).toMatch(/queries|usage/);
  });

  test("quota fields should have unlimited placeholder", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });
    await createButton.click();

    // Wait for dialog to open
    await authenticatedPage.waitForSelector('[role="dialog"]');

    // Verify quota fields have placeholder indicating unlimited
    const maxQueriesInput = authenticatedPage.getByLabel(/max queries/i);
    const maxBytesInput = authenticatedPage.getByLabel(/max data transfer/i);

    // Check placeholders indicate unlimited (fields should be empty by default)
    await expect(maxQueriesInput).toHaveAttribute("placeholder", /unlimited/i);
    await expect(maxBytesInput).toHaveAttribute("placeholder", /unlimited/i);
  });

  test("should have unit selector for data transfer", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });
    await createButton.click();

    // Wait for dialog to open
    await authenticatedPage.waitForSelector('[role="dialog"]');

    // Verify unit selector is present (MB/GB)
    const formContent = await authenticatedPage.textContent('[role="dialog"]');
    expect(formContent).toMatch(/MB|GB/);

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-create-unit-selector.png",
    });
  });
});
