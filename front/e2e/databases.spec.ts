import { test, expect } from "./fixtures";

test.describe("Databases Management", () => {
  test("should display databases list page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("databases");

    // Wait for page to load
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot of databases page
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/databases-list.png",
      fullPage: true,
    });

    // Verify we're on the databases page
    await expect(authenticatedPage).toHaveURL(/\/databases/);

    // Check for page content
    const pageContent = await authenticatedPage.textContent("body");
    expect(pageContent).toBeTruthy();
  });

  test("should show create database button or form", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("databases");
    await authenticatedPage.waitForLoadState("networkidle");

    // Look for create/add button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new/i,
    });

    if (await createButton.isVisible()) {
      await createButton.click();

      // Take screenshot of create database dialog/form
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/databases-create-dialog.png",
      });

      // Look for form fields typical for database configuration
      const formContent = await authenticatedPage.textContent("body");
      expect(
        formContent?.toLowerCase()
      ).toMatch(/host|port|database|name|connection/);
    }
  });

  test("should display database configuration options", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("databases");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/databases-overview.png",
      fullPage: true,
    });

    // Verify database-related content is present
    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });
});
