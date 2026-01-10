import { test as base, expect, type Page } from "@playwright/test";

/**
 * Test fixture that provides authenticated page context.
 * Uses the default admin credentials (admin/admintest) for login.
 *
 * Note: This requires the backend server to be running in RUN_MODE=test.
 */
export const test = base.extend<{ authenticatedPage: Page }>({
  authenticatedPage: async ({ page }, use) => {
    // Navigate to login page
    await page.goto("/");

    // Wait for page to be fully loaded
    await page.waitForLoadState("domcontentloaded");

    // Wait for login form to be visible using test IDs
    const loginLogo = page.getByTestId("login-logo");
    await loginLogo.waitFor({ state: "visible", timeout: 10000 });

    // Fill in credentials (default admin) using test IDs
    await page.getByTestId("login-username").fill("admin");
    await page.getByTestId("login-password").fill("admintest");

    // Submit login form using test ID
    await page.getByTestId("login-submit").click();

    // Wait for navigation away from login
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    // Wait for authenticated page to be loaded
    await page.waitForLoadState("domcontentloaded");

    // Use the authenticated page
    await use(page);
  },
});

export { expect };
