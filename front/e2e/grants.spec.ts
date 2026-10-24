import { test, expect, loginAs } from "./fixtures";

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

    // Verify the date portion is today. `datetime-local`'s value is always the
    // browser's LOCAL date/time per the HTML spec, so the expectation must be
    // built from local date components — not `toISOString()`, which is UTC and
    // disagrees with the local date for part of every day in any timezone
    // ahead of UTC.
    const now = new Date();
    const today = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
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

  test("?uid= narrows the list to the linked grant (Slack deep link)", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator('[data-testid^="grant-row-"]');
    const total = await rows.count();
    expect(total).toBeGreaterThan(0);
    const testId = await rows.first().getAttribute("data-testid");
    const uid = testId!.replace("grant-row-", "");

    await authenticatedPage.goto(`grants?uid=${uid}`);
    await authenticatedPage.waitForLoadState("networkidle");

    await expect(authenticatedPage.getByTestId("grant-focus-banner")).toBeVisible();
    await expect(rows).toHaveCount(1);
    await expect(authenticatedPage.getByTestId(`grant-row-${uid}`)).toBeVisible();

    await authenticatedPage.getByTestId("grant-focus-clear").click();
    await expect(authenticatedPage.getByTestId("grant-focus-banner")).toHaveCount(0);
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

// A user revokes their own grant, the way they already revoke their own API
// keys: the grant is seeded through the admin API on a database and a
// definition of its own, so no other test's seeded grant is touched.
test.describe("Self-revocation", () => {
  const API_BASE = "http://localhost:8080/api/v1";

  test("a connector revokes its own grant from the grants page", async ({
    page,
    request,
  }) => {
    const login = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    expect(login.status()).toBe(200);
    const { token } = await login.json();
    const headers = { Authorization: `Bearer ${token}` };

    const suffix = `${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;

    const users = await request.get(`${API_BASE}/users`, { headers });
    expect(users.status()).toBe(200);
    const connector = (await users.json()).users.find(
      (u: { username: string }) => u.username === "connector",
    );
    expect(connector).toBeTruthy();

    const server = await request.post(`${API_BASE}/servers`, {
      headers,
      data: {
        name: `e2e_self_revoke_${suffix}`,
        host: "localhost",
        port: 5432,
        database_name: "postgres",
        username: "postgres",
        password: "postgres",
      },
    });
    expect(server.status()).toBeLessThan(300);
    const { uid: databaseUid } = await server.json();

    const def = await request.post(`${API_BASE}/grant-definitions`, {
      headers,
      data: {
        name: `e2e self revoke ${suffix}`,
        slug: `e2e-self-revoke-${suffix.replace(/_/g, "-")}`,
        duration_seconds: 3600,
        controls: ["read_only"],
      },
    });
    expect(def.status()).toBeLessThan(300);
    const { uid: definitionUid } = await def.json();

    const grant = await request.post(`${API_BASE}/grants`, {
      headers,
      data: {
        grant_definition_id: definitionUid,
        user_id: connector.uid,
        database_id: databaseUid,
      },
    });
    expect(grant.status()).toBeLessThan(300);
    const { uid: grantUid } = await grant.json();

    await loginAs(page, "connector", "connector");
    await page.goto("grants");
    await page.waitForLoadState("networkidle");

    const revokeButton = page.getByTestId(`revoke-grant-${grantUid}`);
    await expect(revokeButton).toBeEnabled();
    await revokeButton.click();
    await page.getByTestId("revoke-grant-confirm").click();

    await expect(page.getByText("Grant revoked successfully")).toBeVisible();
    await expect(page.getByTestId(`revoke-grant-${grantUid}`)).toHaveCount(0);

    const after = await request.get(`${API_BASE}/grants/${grantUid}`, {
      headers,
    });
    expect((await after.json()).revoked_at).toBeTruthy();
  });
});
