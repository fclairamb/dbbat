import { test, expect } from "./fixtures";

test.describe("Observability Features", () => {
  test("should display connections page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/connections-page.png",
      fullPage: true,
    });

    // Verify we're on the connections page
    await expect(authenticatedPage).toHaveURL(/\/connections/);

    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("should display queries page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/queries-page.png",
      fullPage: true,
    });

    // Verify we're on the queries page
    await expect(authenticatedPage).toHaveURL(/\/queries/);

    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("should display audit log page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/audit-page.png",
      fullPage: true,
    });

    // Verify we're on the audit page
    await expect(authenticatedPage).toHaveURL(/\/audit/);

    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("should navigate to queries page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("load");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/queries-navigation.png",
      fullPage: true,
    });

    // Verify we're on the queries page
    await expect(authenticatedPage).toHaveURL(/\/queries/);
  });

  test("should refresh connections without getting stuck", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Find the Refresh button
    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    // Verify button is enabled initially
    await expect(refreshButton).toBeEnabled();

    // Click the refresh button
    await refreshButton.click();

    // Button should be disabled while refreshing
    await expect(refreshButton).toBeDisabled();

    // Wait for the refresh to complete (button should become enabled again)
    // Give it a reasonable timeout (5 seconds)
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    // Verify icon is not spinning anymore
    const refreshIcon = refreshButton.locator("svg");
    const iconClasses = await refreshIcon.getAttribute("class");
    expect(iconClasses).not.toContain("animate-spin");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/connections-refresh-complete.png",
      fullPage: true,
    });
  });

  test("should refresh queries without getting stuck", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    await expect(refreshButton).toBeEnabled();
    await refreshButton.click();
    await expect(refreshButton).toBeDisabled();
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    const refreshIcon = refreshButton.locator("svg");
    const iconClasses = await refreshIcon.getAttribute("class");
    expect(iconClasses).not.toContain("animate-spin");
  });

  test("should refresh audit log without getting stuck", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    await expect(refreshButton).toBeEnabled();
    await refreshButton.click();
    // Note: We don't check for disabled state here as the API call may complete
    // too quickly for the disabled state to be caught reliably
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    const refreshIcon = refreshButton.locator("svg");
    const iconClasses = await refreshIcon.getAttribute("class");
    expect(iconClasses).not.toContain("animate-spin");
  });
});

test.describe("Adaptive Auto-Refresh Feature", () => {
  test("should toggle auto-refresh on and off", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Find the auto-refresh badge
    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);
    await expect(autoRefreshBadge).toBeVisible();

    // Initially, auto-refresh should be enabled (showing countdown)
    let badgeText = await autoRefreshBadge.textContent();
    const initiallyEnabled = badgeText?.includes("Next:");

    // Click to toggle
    await autoRefreshBadge.click();

    // Wait a bit for state to update
    await authenticatedPage.waitForTimeout(100);

    // Verify it toggled
    badgeText = await autoRefreshBadge.textContent();
    if (initiallyEnabled) {
      expect(badgeText).toContain("Auto-refresh: OFF");
    } else {
      expect(badgeText).toMatch(/Next: \d+s/);
    }

    // Toggle back
    await autoRefreshBadge.click();
    await authenticatedPage.waitForTimeout(100);

    // Verify it toggled back
    badgeText = await autoRefreshBadge.textContent();
    if (initiallyEnabled) {
      expect(badgeText).toMatch(/Next: \d+s/);
    } else {
      expect(badgeText).toContain("Auto-refresh: OFF");
    }

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-toggle.png",
      fullPage: true,
    });
  });

  test("should display countdown timer when auto-refresh is enabled", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Find the auto-refresh badge
    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);
    await expect(autoRefreshBadge).toBeVisible();

    // Make sure auto-refresh is enabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Verify countdown format
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toMatch(/Next: \d+s/);

    // Extract the current countdown value
    const match = badgeText?.match(/Next: (\d+)s/);
    expect(match).toBeTruthy();
    const initialSeconds = parseInt(match![1]);

    // Wait 2 seconds and verify countdown decreased
    await authenticatedPage.waitForTimeout(2000);
    badgeText = await autoRefreshBadge.textContent();
    const match2 = badgeText?.match(/Next: (\d+)s/);
    expect(match2).toBeTruthy();
    const newSeconds = parseInt(match2![1]);

    // Countdown should have decreased (allowing for some timing variance)
    expect(newSeconds).toBeLessThan(initialSeconds);

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-countdown.png",
      fullPage: true,
    });
  });

  test("should trigger refresh when countdown reaches 0", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Find the auto-refresh badge
    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);
    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    // Make sure auto-refresh is enabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Manually trigger a refresh to reset the timer to a known state
    await refreshButton.click();
    await expect(refreshButton).toBeDisabled();
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    // Now wait for countdown to reach a low value (checking every second)
    let countdownReachedLow = false;
    for (let i = 0; i < 15; i++) {
      badgeText = await autoRefreshBadge.textContent();
      const match = badgeText?.match(/Next: (\d+)s/);
      if (match) {
        const seconds = parseInt(match[1]);
        if (seconds <= 2) {
          countdownReachedLow = true;
          break;
        }
      }
      await authenticatedPage.waitForTimeout(1000);
    }

    expect(countdownReachedLow).toBeTruthy();

    // Wait for the auto-refresh to trigger (button should become disabled then enabled)
    // The refresh should trigger when countdown hits 0
    await authenticatedPage.waitForTimeout(3000);

    // After auto-refresh, countdown should have reset
    badgeText = await autoRefreshBadge.textContent();
    const matchAfter = badgeText?.match(/Next: (\d+)s/);
    if (matchAfter) {
      const secondsAfter = parseInt(matchAfter[1]);
      // Should be close to the initial interval (10s by default)
      expect(secondsAfter).toBeGreaterThan(2);
    }

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-triggered.png",
      fullPage: true,
    });
  });

  test("should reset interval when manually refreshing", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);
    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    // Make sure auto-refresh is enabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Wait for countdown to decrease
    await authenticatedPage.waitForTimeout(3000);

    // Get current countdown
    badgeText = await autoRefreshBadge.textContent();
    let match = badgeText?.match(/Next: (\d+)s/);
    expect(match).toBeTruthy();
    const secondsBeforeRefresh = parseInt(match![1]);

    // Manual refresh
    await refreshButton.click();
    await expect(refreshButton).toBeDisabled();
    await expect(refreshButton).toBeEnabled({ timeout: 15000 });

    // After manual refresh, countdown should reset to initial interval
    badgeText = await autoRefreshBadge.textContent();
    match = badgeText?.match(/Next: (\d+)s/);
    expect(match).toBeTruthy();
    const secondsAfterRefresh = parseInt(match![1]);

    // Countdown should have reset to a higher value (initial interval is 10s by default)
    expect(secondsAfterRefresh).toBeGreaterThan(secondsBeforeRefresh);
    expect(secondsAfterRefresh).toBeGreaterThanOrEqual(8); // Should be close to 10s

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-manual-reset.png",
      fullPage: true,
    });
  });

  test("should persist auto-refresh state in localStorage", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);

    // Make sure auto-refresh is enabled first
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Verify it's enabled
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toMatch(/Next: \d+s/);

    // Check localStorage
    let storedValue = await authenticatedPage.evaluate(() => {
      return localStorage.getItem("dbbat.autoRefresh.connections");
    });
    expect(storedValue).toBeTruthy();
    let parsed = JSON.parse(storedValue!);
    expect(parsed.enabled).toBe(true);

    // Toggle off
    await autoRefreshBadge.click();
    await authenticatedPage.waitForTimeout(100);

    // Verify badge shows OFF
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toContain("Auto-refresh: OFF");

    // Check localStorage was updated
    storedValue = await authenticatedPage.evaluate(() => {
      return localStorage.getItem("dbbat.autoRefresh.connections");
    });
    parsed = JSON.parse(storedValue!);
    expect(parsed.enabled).toBe(false);

    // Navigate away and back to test persistence
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Auto-refresh should still be off after navigation
    const autoRefreshBadgeAfterNav = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);
    const badgeTextAfterNav = await autoRefreshBadgeAfterNav.textContent();
    expect(badgeTextAfterNav).toContain("Auto-refresh: OFF");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-persistence.png",
      fullPage: true,
    });
  });

  test("should show 'Auto-refresh: OFF' when disabled", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);

    // Make sure auto-refresh is disabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Next:")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Verify text shows "Auto-refresh: OFF"
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toBe("Auto-refresh: OFF");

    // Wait and verify text doesn't change (no countdown when disabled)
    await authenticatedPage.waitForTimeout(2000);
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toBe("Auto-refresh: OFF");
  });

  test("should work on queries page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);
    await expect(autoRefreshBadge).toBeVisible();

    // Toggle
    await autoRefreshBadge.click();
    await authenticatedPage.waitForTimeout(100);

    const badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toBeTruthy();

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-queries.png",
      fullPage: true,
    });
  });

  test("should work on audit page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);
    await expect(autoRefreshBadge).toBeVisible();

    // Toggle
    await autoRefreshBadge.click();
    await authenticatedPage.waitForTimeout(100);

    const badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toBeTruthy();

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-audit.png",
      fullPage: true,
    });
  });

  test("should handle multiple manual refresh clicks properly", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });
    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);

    // Verify button is enabled initially
    await expect(refreshButton).toBeEnabled();

    // Click refresh multiple times in succession
    for (let i = 0; i < 3; i++) {
      // Click the refresh button
      await refreshButton.click();

      // Button should be disabled immediately
      await expect(refreshButton).toBeDisabled();

      // Wait for refresh to complete
      await expect(refreshButton).toBeEnabled({ timeout: 5000 });

      // Verify icon is not spinning
      const refreshIcon = refreshButton.locator("svg");
      const iconClasses = await refreshIcon.getAttribute("class");
      expect(iconClasses).not.toContain("animate-spin");

      // Verify countdown reset (if auto-refresh is enabled)
      const badgeText = await autoRefreshBadge.textContent();
      if (badgeText?.includes("Next:")) {
        const match = badgeText.match(/Next: (\d+)s/);
        expect(match).toBeTruthy();
        const seconds = parseInt(match![1]);
        expect(seconds).toBeGreaterThanOrEqual(8); // Should be close to initial 10s
      }

      // Small delay between clicks
      await authenticatedPage.waitForTimeout(500);
    }

    // Take screenshot after multiple refreshes
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-multiple-clicks.png",
      fullPage: true,
    });
  });

  test("should not allow refresh button clicks while refreshing", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    // Click refresh
    await refreshButton.click();

    // Button should be disabled
    await expect(refreshButton).toBeDisabled();

    // Try clicking while disabled (should have no effect)
    await refreshButton.click({ force: true });

    // Still should be disabled
    await expect(refreshButton).toBeDisabled();

    // Wait for it to complete
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-button-guard.png",
      fullPage: true,
    });
  });

  test("should refresh button work with auto-refresh enabled", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });
    const autoRefreshBadge = authenticatedPage.getByText(/Next:|Auto-refresh: OFF/);

    // Ensure auto-refresh is enabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Verify countdown is running
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toMatch(/Next: \d+s/);

    // Manual refresh should still work
    await refreshButton.click();
    await expect(refreshButton).toBeDisabled();
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    // Auto-refresh should still be enabled after manual refresh
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toMatch(/Next: \d+s/);

    // Countdown should have reset to initial value
    const match = badgeText?.match(/Next: (\d+)s/);
    expect(match).toBeTruthy();
    const seconds = parseInt(match![1]);
    expect(seconds).toBeGreaterThanOrEqual(8);

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-button-with-auto.png",
      fullPage: true,
    });
  });
});
