import { test, expect } from "./fixtures";

test.describe("Session Expiration", () => {
  test("should redirect to login with session expired message when session is invalidated", async ({
    authenticatedPage,
  }) => {
    // Navigate to a page with a refresh button (connections page)
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Verify we're on the connections page
    await expect(authenticatedPage).toHaveURL(/\/connections/);

    // Take screenshot before session invalidation
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/session-before-invalidation.png",
      fullPage: true,
    });

    // Get the session token from localStorage
    const token = await authenticatedPage.evaluate(() => {
      return localStorage.getItem("dbbat_session_token");
    });
    expect(token).toBeTruthy();

    // Call the logout API to invalidate the session
    // This simulates what happens when the session expires or is revoked on the server
    const logoutResponse = await authenticatedPage.request.post("/api/v1/auth/logout", {
      headers: {
        Authorization: `Bearer ${token}`,
      },
    });
    expect(logoutResponse.ok()).toBe(true);

    // Now click the refresh button to trigger an API call
    // This should receive a 401 and redirect to login
    const refreshButton = authenticatedPage.getByTestId("refresh-button");
    await expect(refreshButton).toBeVisible();
    await refreshButton.click();

    // Wait for redirect to login page with session_expired parameter
    await authenticatedPage.waitForURL(/\/login.*session_expired=true/, {
      timeout: 10000,
    });

    // Wait for the page to fully render
    await authenticatedPage.waitForLoadState("networkidle");

    // Verify the session expired alert is displayed
    const sessionExpiredAlert = authenticatedPage.getByTestId("session-expired-alert");
    await expect(sessionExpiredAlert).toBeVisible({ timeout: 5000 });
    await expect(sessionExpiredAlert).toContainText("Your session has expired");

    // Take screenshot of the session expired state
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/session-expired.png",
      fullPage: true,
    });

    // Verify we can log in again
    await authenticatedPage.getByTestId("login-username").fill("admin");
    await authenticatedPage.getByTestId("login-password").fill("admintest");
    await authenticatedPage.getByTestId("login-submit").click();

    // Wait for redirect to authenticated area
    await authenticatedPage.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    // Verify we're logged in again
    await expect(authenticatedPage).not.toHaveURL(/\/login/);

    // Take screenshot after re-login
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/session-relogin.png",
      fullPage: true,
    });
  });
});
