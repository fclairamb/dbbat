import { test, expect } from "./fixtures";

/**
 * A grant is an instance of a grant definition: the admin picks a definition,
 * a user and a database, and the shape (controls, quotas, approval gating,
 * duration) comes from the definition. There is deliberately no ad-hoc grant
 * form any more, so these tests drive the Assign Grant dialog rather than the
 * old Create Grant one.
 */
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

  test("the assign dialog asks for a definition, a user and a database", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    await authenticatedPage.getByTestId("assign-grant-button").click();
    await authenticatedPage.waitForSelector('[role="dialog"]');

    await expect(
      authenticatedPage.getByTestId("assign-grant-definition")
    ).toBeVisible();
    await expect(authenticatedPage.getByTestId("assign-grant-user")).toBeVisible();
    await expect(
      authenticatedPage.getByTestId("assign-grant-database")
    ).toBeVisible();

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-assign-dialog.png",
    });
  });

  test("there is no ad-hoc shape to type: no controls or quota inputs", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    await authenticatedPage.getByTestId("assign-grant-button").click();
    await authenticatedPage.waitForSelector('[role="dialog"]');

    // The old form let an admin invent a grant here. That is exactly what this
    // change removed, so its inputs must be gone — not merely unused.
    await expect(authenticatedPage.getByLabel(/max queries/i)).toHaveCount(0);
    await expect(
      authenticatedPage.getByLabel(/max data transfer/i)
    ).toHaveCount(0);
    await expect(authenticatedPage.getByTestId("grant-priority-input")).toHaveCount(
      0
    );
    await expect(
      authenticatedPage.locator('input[type="datetime-local"]#expiresAt')
    ).toHaveCount(0);
  });

  test("picking a definition previews the shape it will grant", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    await authenticatedPage.getByTestId("assign-grant-button").click();
    await authenticatedPage.waitForSelector('[role="dialog"]');

    await authenticatedPage.getByTestId("assign-grant-definition").click();
    await authenticatedPage.getByRole("option").first().click();

    // The admin has to be able to see what they are handing out before they
    // hand it out, since they no longer type it themselves.
    const shape = authenticatedPage.getByTestId("assign-grant-shape");
    await expect(shape).toBeVisible();
    await expect(shape).toContainText(/controls/i);
    await expect(shape).toContainText(/duration/i);

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-assign-shape-preview.png",
    });
  });

  test("should default start time to approximately current time", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    await authenticatedPage.getByTestId("assign-grant-button").click();
    await authenticatedPage.waitForSelector('[role="dialog"]');

    const startsAtInput = authenticatedPage.locator(
      'input[type="datetime-local"]#startsAt'
    );
    const startsAtValue = await startsAtInput.inputValue();

    // Verify it's a valid datetime-local format (YYYY-MM-DDTHH:mm)
    expect(startsAtValue).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);

    // Verify the date portion is today
    const today = new Date().toISOString().split("T")[0];
    expect(startsAtValue.split("T")[0]).toBe(today);
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

    // The controls come from each grant's definition, but they are still what
    // the list has to show.
    const content = (await authenticatedPage.textContent("body")) ?? "";
    expect(content.toLowerCase()).toMatch(/read only|full access/);
  });

  test("each grant names the definition it was issued from", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    const definitions = authenticatedPage.locator(
      '[data-testid^="grant-definition-"]'
    );
    expect(await definitions.count()).toBeGreaterThan(0);

    for (const value of await definitions.allTextContents()) {
      expect(value.trim().length).toBeGreaterThan(0);
    }

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-definition-column.png",
      fullPage: true,
    });
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

  test("should render applied limits and unlimited fallback in usage column", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Test mode seeds a quota-bounded definition (admin, 100 queries / 1 GB)
    // and unlimited ones (connector write, viewer read-only). The limits are
    // read off each grant's definition now, which is exactly what this asserts.
    const body = (await authenticatedPage.textContent("body")) ?? "";

    // A grant with a limit shows "used / limit queries" (e.g. "0 / 100 queries").
    expect(body).toMatch(/\/\s*100\s*queries/);

    // The data-transfer limit is rendered too (1 GB).
    expect(body).toMatch(/\/\s*1 GB/);

    // Unlimited grants show an explicit "unlimited" marker instead of nothing.
    expect(body.toLowerCase()).toContain("unlimited");

    // A visual progress indicator is present for the quota grant.
    const progressBars = authenticatedPage.locator('[role="progressbar"]');
    expect(await progressBars.count()).toBeGreaterThan(0);

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-applied-limits.png",
      fullPage: true,
    });
  });
});

test.describe("Grant Priority", () => {
  test("grant list shows the priority of each grant", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Test mode seeds grants from definitions of different shapes, so the
    // column must be populated for every row. (The auto-priority *form*
    // behaviour now lives on the definition editor — see
    // grant-definitions.spec.ts.)
    const priorities = authenticatedPage.locator(
      '[data-testid^="grant-priority-"]'
    );
    expect(await priorities.count()).toBeGreaterThan(0);

    for (const value of await priorities.allTextContents()) {
      expect(value.trim()).toMatch(/^-?\d+$/);
    }

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-priority.png",
      fullPage: true,
    });
  });
});
