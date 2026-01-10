import { test, expect } from "./fixtures";

test.describe("Users Management", () => {
  test("should display users list page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("users");

    // Wait for page to load
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot of users page
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-list.png",
      fullPage: true,
    });

    // Verify we're on the users page
    await expect(authenticatedPage).toHaveURL(/\/users/);

    // Check for common elements (table, buttons, etc.)
    const pageContent = await authenticatedPage.textContent("body");
    expect(pageContent).toBeTruthy();
  });

  test("should show create user button or form", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("users");
    await authenticatedPage.waitForLoadState("networkidle");

    // Look for create/add button (could be various text)
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new/i,
    });

    if (await createButton.isVisible()) {
      await createButton.click();

      // Take screenshot of create user dialog/form
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/users-create-dialog.png",
      });

      // Look for form fields
      const formContent = await authenticatedPage.textContent("body");
      expect(formContent?.toLowerCase()).toMatch(/username|password|name/);
    }
  });

  test("should navigate to users page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("users");
    await authenticatedPage.waitForLoadState("load");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-navigation.png",
      fullPage: true,
    });

    // Verify we're on the users page
    await expect(authenticatedPage).toHaveURL(/\/users/);
  });
});
