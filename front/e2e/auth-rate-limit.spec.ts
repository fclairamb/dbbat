import { test, expect } from "./fixtures";

// Regression coverage for: a 429 from the auth rate limiter must never be
// treated as "the session is invalid". GET /auth/me is rate limited
// per-user (internal/api/ratelimit.go PostAuthMiddleware) and can answer
// 429 for a perfectly valid token; POST /auth/login is rate limited
// per-IP. Neither should destroy a stored session or show a generic
// "Login failed".
test.describe("Auth rate limiting", () => {
  test("a 429 on GET /auth/me keeps the stored session token and does not redirect to /login", async ({
    authenticatedPage,
  }) => {
    const token = await authenticatedPage.evaluate(() =>
      localStorage.getItem("dbbat_session_token"),
    );
    expect(token).toBeTruthy();

    // Every /auth/me call (the bootstrap check that runs on page load)
    // answers 429, as the real rate limiter would once a user is over
    // their per-minute budget.
    await authenticatedPage.route("**/api/v1/auth/me", async (route) => {
      await route.fulfill({
        status: 429,
        contentType: "application/json",
        body: JSON.stringify({
          code: "RATE_LIMITED",
          message: "Too many requests. Try again later.",
          retry_after: 60,
        }),
      });
    });

    // Reload: AuthProvider re-mounts and re-runs the stored-token check
    // against the mocked, always-429 endpoint.
    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");

    // The rate-limited notice must be shown instead of silently bouncing
    // the user away.
    await expect(
      authenticatedPage.getByTestId("session-check-rate-limited"),
    ).toBeVisible({ timeout: 10000 });

    // Never redirected to the login page.
    expect(authenticatedPage.url()).not.toContain("/login");

    // And, critically, the token was never cleared.
    const tokenAfter = await authenticatedPage.evaluate(() =>
      localStorage.getItem("dbbat_session_token"),
    );
    expect(tokenAfter).toBe(token);

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auth-me-rate-limited.png",
      fullPage: true,
    });
  });

  test("recovers automatically once the rate limit clears", async ({
    authenticatedPage,
  }) => {
    let calls = 0;
    await authenticatedPage.route("**/api/v1/auth/me", async (route) => {
      calls += 1;
      if (calls === 1) {
        // First check after reload is rate limited, with a short
        // Retry-After so the scheduled automatic re-validate fires quickly.
        await route.fulfill({
          status: 429,
          headers: { "Retry-After": "1" },
          contentType: "application/json",
          body: JSON.stringify({
            code: "RATE_LIMITED",
            message: "Too many requests. Try again later.",
            retry_after: 1,
          }),
        });
        return;
      }
      // Subsequent calls go through to the real backend.
      await route.continue();
    });

    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");

    await expect(
      authenticatedPage.getByTestId("session-check-rate-limited"),
    ).toBeVisible({ timeout: 10000 });

    // The single automatic re-validate (scheduled from Retry-After: 1)
    // succeeds against the real backend and the app recovers on its own,
    // without the user ever re-entering credentials.
    await expect(
      authenticatedPage.getByTestId("session-check-rate-limited"),
    ).not.toBeVisible({ timeout: 10000 });
    expect(authenticatedPage.url()).not.toContain("/login");
  });

  // POST /auth/login sits behind two different rate limiters — the
  // per-username auth-failure tracker and the IP-based PreAuthMiddleware —
  // and both now answer with the canonical Error schema
  // (`code: "RATE_LIMITED"`, see writeRateLimited in
  // internal/api/errors.go). The second scenario below is the ad-hoc
  // `{error, message, retry_after}` body those middlewares used to write:
  // the frontend gates on the HTTP status rather than the body shape (see
  // AuthContext.tsx login()), so it must stay friendly against an older
  // deployment or a proxy that still returns the legacy shape.
  for (const scenario of [
    {
      name: "canonical Error schema (code: RATE_LIMITED)",
      body: {
        code: "RATE_LIMITED",
        message: "Too many requests. Try again later.",
        retry_after: 30,
      },
    },
    {
      name: "legacy ad-hoc body (no code field)",
      body: {
        error: "rate_limit_exceeded",
        message: "Too many requests. Please retry after 30 seconds.",
        retry_after: 30,
      },
    },
  ]) {
    test(`a 429 on POST /auth/login (${scenario.name}) shows a rate-limit message, not a generic login failure`, async ({
      page,
    }) => {
      await page.route("**/api/v1/auth/login", async (route) => {
        await route.fulfill({
          status: 429,
          headers: { "Retry-After": "30" },
          contentType: "application/json",
          body: JSON.stringify(scenario.body),
        });
      });

      await page.goto("login");
      await page.waitForLoadState("networkidle");
      await expect(page.getByTestId("login-title")).toBeVisible();

      await page.getByTestId("login-username").fill("admin");
      await page.getByTestId("login-password").fill("admintest");
      await page.getByTestId("login-submit").click();

      const errorAlert = page.getByTestId("login-error");
      await expect(errorAlert).toBeVisible({ timeout: 5000 });
      await expect(errorAlert).toContainText(/too many login attempts/i);
      await expect(errorAlert).not.toContainText("Login failed");

      expect(page.url()).toContain("/app/login");

      await page.screenshot({
        path: "test-results/screenshots/login-rate-limited.png",
      });
    });
  }
});
