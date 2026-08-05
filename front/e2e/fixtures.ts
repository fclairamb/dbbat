import { test as base, expect, type Page } from "@playwright/test";

/**
 * Test fixture that provides authenticated page context.
 * Uses the default admin credentials (admin/admintest) for login.
 *
 * Note: This requires the backend server to be running in RUN_MODE=test.
 */
/**
 * Drive the real login form as `username`/`password` and wait until the app
 * has navigated away from /login.
 *
 * Exported (rather than inlined in the fixture below) because the showcase
 * runner in ../showcase needs the same flow against demo-mode credentials.
 */
export async function loginAs(
  page: Page,
  username: string,
  password: string,
): Promise<void> {
  // Navigate to login page
  await page.goto("/");

  // Wait for page to be fully loaded
  await page.waitForLoadState("domcontentloaded");

  // Wait for login form to be visible using test IDs
  const loginLogo = page.getByTestId("login-logo");
  await loginLogo.waitFor({ state: "visible", timeout: 10000 });

  // Fill in credentials using test IDs
  await page.getByTestId("login-username").fill(username);
  await page.getByTestId("login-password").fill(password);

  // Submit login form using test ID
  await page.getByTestId("login-submit").click();

  // Wait for navigation away from login
  await page.waitForURL((url) => !url.pathname.includes("/login"), {
    timeout: 10000,
  });

  // Wait for authenticated page to be loaded
  await page.waitForLoadState("domcontentloaded");
}

export const test = base.extend<{ authenticatedPage: Page }>({
  authenticatedPage: async ({ page }, use) => {
    await loginAs(page, "admin", "admintest");

    // Use the authenticated page
    // eslint-disable-next-line react-hooks/rules-of-hooks
    await use(page);
  },
});

export { expect };
