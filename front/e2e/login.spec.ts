import { test, expect } from "@playwright/test";

test.describe("Login Flow", () => {
  test("should display login page", async ({ page }) => {
    await page.goto("login");
    await page.waitForLoadState("networkidle");

    // Take screenshot of login page
    await expect(page).toHaveTitle(/DBBat|Login/i);
    await page.screenshot({ path: "test-results/screenshots/login-page.png" });

    // Check for login form elements using test IDs
    await expect(page.getByTestId("login-title")).toBeVisible();
    await expect(page.getByTestId("login-username")).toBeVisible();
    await expect(page.getByTestId("login-password")).toBeVisible();
    await expect(page.getByTestId("login-submit")).toBeVisible();
  });

  test("should show error on invalid credentials", async ({ page }) => {
    await page.goto("login");
    await page.waitForLoadState("networkidle");

    // Wait for form to be ready
    await expect(page.getByTestId("login-title")).toBeVisible();

    // Fill in invalid credentials using test IDs
    await page.getByTestId("login-username").fill("wronguser");
    await page.getByTestId("login-password").fill("wrongpassword");

    // Submit form using test ID
    await page.getByTestId("login-submit").click();

    // Wait for error message to appear
    await expect(page.getByTestId("login-error")).toBeVisible({ timeout: 5000 });

    // Verify we're still on the login page
    expect(page.url()).toContain("/app/login");

    // Take screenshot of error state (after error is visible)
    await page.screenshot({
      path: "test-results/screenshots/login-error.png",
    });
  });

  test("should successfully login with valid credentials", async ({ page }) => {
    await page.goto("login");
    await page.waitForLoadState("networkidle");

    // Wait for form to be ready
    await expect(page.getByTestId("login-title")).toBeVisible();

    // Fill in valid credentials (default admin) using test IDs
    await page.getByTestId("login-username").fill("admin");
    await page.getByTestId("login-password").fill("admintest");

    // Take screenshot before login
    await page.screenshot({
      path: "test-results/screenshots/login-filled.png",
    });

    // Submit form using test ID
    await page.getByTestId("login-submit").click();

    // Wait for redirect away from login to authenticated area
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    // Wait for page to fully load
    await page.waitForLoadState("networkidle");

    // Take screenshot after successful login
    await page.screenshot({
      path: "test-results/screenshots/login-success.png",
      fullPage: true,
    });

    // Verify we're on an authenticated page (not login)
    const currentUrl = page.url();
    expect(currentUrl).not.toContain("/login");
    // Verify we can see the dashboard or another authenticated page
    const pageContent = await page.textContent("body");
    expect(pageContent).toBeTruthy();
  });

  test("should not allow access to protected data without auth", async ({
    page,
  }) => {
    // Try to access a protected route directly
    await page.goto("users");
    await page.waitForLoadState("networkidle");

    // Wait for either redirect to login page OR empty state (API calls fail with 401)
    // Use Promise.race to wait for whichever happens first
    await Promise.race([
      page.waitForURL((url) => url.pathname.includes("/login"), { timeout: 5000 }),
      page.getByText(/No users found|failed|unauthorized/i).waitFor({ timeout: 5000 }),
    ]).catch(() => {
      // If neither happens, that's okay - we'll check the state below
    });

    // Take screenshot
    await page.screenshot({
      path: "test-results/screenshots/auth-check.png",
      fullPage: true,
    });

    const url = page.url();
    const content = await page.textContent("body");

    // Either we're redirected to login, or we see empty state (API calls fail with 401)
    const isOnLoginPage = url.includes("/login");
    const showsEmptyState = content?.includes("No users found") || content?.includes("failed");

    expect(isOnLoginPage || showsEmptyState).toBe(true);
  });
  // The OAuth callback hands the browser a single-use exchange code, never a
  // session token (a token in a URL leaks into logs and history). This drives
  // the SPA half of that handoff: an unknown code must be posted to the
  // exchange endpoint, rejected, scrubbed from the URL, and reported.
  test("should redeem an OAuth exchange code and refuse a stale one", async ({
    page,
  }) => {
    const exchangeCalls: number[] = [];
    page.on("response", (response) => {
      if (response.url().includes("/api/v1/auth/oauth/exchange")) {
        exchangeCalls.push(response.status());
      }
    });

    await page.goto("login?code=deadbeefdeadbeefdeadbeefdeadbeef");
    await page.waitForLoadState("networkidle");

    // The code must never survive in the URL, used or not.
    await expect
      .poll(() => new URL(page.url()).searchParams.get("code"))
      .toBeNull();

    // The SPA asked the exchange endpoint, and was refused.
    await expect.poll(() => exchangeCalls.length).toBeGreaterThan(0);
    expect(exchangeCalls[0]).toBe(401);

    // A refused exchange leaves the user on the login page with an
    // explanation, not silently signed out.
    await expect(page.getByTestId("login-error")).toBeVisible({ timeout: 5000 });
    expect(page.url()).toContain("/app/login");

    await page.screenshot({
      path: "test-results/screenshots/login-oauth-exchange-refused.png",
    });
  });

  // Regression guard for the session token that used to ride in the OAuth
  // callback's redirect: no navigation of a successful login may carry a
  // credential in its URL.
  test("should never put a session token in the URL", async ({ page }) => {
    const urls: string[] = [];
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) {
        urls.push(frame.url());
      }
    });

    await page.goto("login");
    await page.waitForLoadState("networkidle");
    await page.getByTestId("login-username").fill("admin");
    await page.getByTestId("login-password").fill("admintest");
    await page.getByTestId("login-submit").click();

    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });
    await page.waitForLoadState("networkidle");

    urls.push(page.url());
    for (const url of urls) {
      expect(url).not.toContain("web_");
      expect(url).not.toContain("token=");
    }
  });
});
