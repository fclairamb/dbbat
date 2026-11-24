import { test, expect, afterSectionSave } from "./fixtures";

/**
 * Retention settings: the instance card, and the windows on a server group.
 *
 * A test-mode server has no archive bucket (DBB_ARCHIVE_URL unset), so the
 * archive window is not offered: that is part of what is checked here.
 */

test.describe("Retention", () => {
  test("instance card saves a delete window and shows what is in effect", async ({
    authenticatedPage: page,
  }) => {
    await page.goto("settings");
    await page.waitForLoadState("networkidle");

    await expect(page.getByTestId("retention-section")).toBeVisible();

    // No archive bucket: the archive window cannot be set.
    await expect(page.getByTestId("retention-archive-after-input")).toBeDisabled();
    await expect(
      page.getByTestId("retention-archive-after-effective"),
    ).toContainText("DBB_ARCHIVE_URL");

    await page.getByTestId("retention-delete-after-input").fill("8760h");
    // A new positive delete window destroys data: it is warned about.
    await expect(page.getByTestId("retention-delete-warning")).toBeVisible();
    await page.getByTestId("retention-save-button").click();
    await expect(page.getByText("Retention saved")).toBeVisible();

    await expect(
      page.getByTestId("retention-delete-after-effective"),
    ).toContainText("365 days");

    // Back to unset: nothing is deleted.
    await page.getByTestId("retention-delete-after-input").fill("");

    const cleared = page.waitForResponse(
      (r) => r.url().includes("/instance/retention") && r.request().method() === "PUT",
    );
    await page.getByTestId("retention-save-button").click();
    expect((await cleared).status()).toBe(204);

    await expect(
      page.getByTestId("retention-delete-after-effective"),
    ).toContainText("never", { timeout: 15000 });
  });

  test("a malformed window is refused", async ({ authenticatedPage: page }) => {
    await page.goto("settings");
    await page.waitForLoadState("networkidle");

    await page.getByTestId("retention-delete-after-input").fill("-5h");
    await page.getByTestId("retention-save-button").click();
    await expect(page.getByText(/negative|invalid/i).first()).toBeVisible();
  });

  test("a server group carries its own delete window", async ({
    authenticatedPage: page,
  }) => {
    const name = `retention-${Date.now()}`;

    await page.goto("server-groups");
    await page.waitForLoadState("networkidle");
    await page.getByTestId("create-server-group-button").click();
    await page.waitForSelector('[role="dialog"]');
    await page.getByTestId("server-group-name").fill(name);

    // Unset inherits: no warning.
    await expect(page.getByTestId("server-group-delete-warning")).toHaveCount(0);
    // No bucket: no archive field.
    await expect(
      page.getByTestId("server-group-archive-after-mode"),
    ).toHaveCount(0);

    await page.getByTestId("server-group-delete-after-mode").click();
    await page.getByTestId("server-group-delete-after-mode-custom").click();
    await page.getByTestId("server-group-delete-after-amount").fill("30");
    await page.getByTestId("server-group-delete-after-unit").click();
    await page.getByTestId("server-group-delete-after-unit-days").click();
    await expect(page.getByTestId("server-group-delete-warning")).toBeVisible();

    await page.getByTestId("server-group-submit").click();
    await expect(page.getByText(name)).toBeVisible({ timeout: 20000 });

    // Reopen: the value comes back in its largest unit.
    const row = page.locator("tr", { hasText: name });
    await row.locator('[data-testid^="edit-server-group-"]').click();
    await expect(page.getByTestId("server-group-name")).toBeVisible();
    await expect(page.getByTestId("server-group-delete-after-amount")).toHaveValue(
      "30",
    );
    await expect(page.getByTestId("server-group-delete-after-unit")).toContainText(
      "days",
    );
  });

  test("a server carries its own delete window", async ({
    authenticatedPage: page,
  }) => {
    const name = `e2e_retention_${Date.now()}`;

    await page.goto("servers");
    await page.waitForLoadState("networkidle");

    await page.getByTestId("add-database-button").click();
    await page.getByTestId("database-name-input").fill(name);
    await page.locator("#host").fill("localhost");
    await page.locator("#databaseName").fill("postgres");
    await page.locator("#username").fill("postgres");
    await page.locator("#password").fill("postgres");
    await page.getByTestId("database-create-submit").click();

    const row = page.locator("tr", { hasText: name }).first();
    await expect(row).toBeVisible({ timeout: 10000 });
    await row.locator('[data-testid^="database-edit-"]').click();

    const dialog = page.getByTestId("database-edit-section");
    await expect(dialog).toBeVisible();

    // 2 hours, written as hours and read back as hours.
    await page.getByTestId("database-edit-delete-after-mode").click();
    await page.getByTestId("database-edit-delete-after-mode-custom").click();
    await page.getByTestId("database-edit-delete-after-unit").click();
    await page.getByTestId("database-edit-delete-after-unit-hours").click();
    await page.getByTestId("database-edit-delete-after-amount").fill("2");
    await expect(page.getByTestId("database-edit-delete-warning")).toBeVisible();

    await page.getByTestId("database-edit-submit").click();
    await afterSectionSave(page);

    await page.reload();
    await page.waitForLoadState("networkidle");
    await page
      .locator("tr", { hasText: name })
      .first()
      .locator('[data-testid^="database-edit-"]')
      .click();
    await expect(page.getByTestId("database-edit-delete-after-amount")).toHaveValue(
      "2",
    );
    await expect(page.getByTestId("database-edit-delete-after-unit")).toContainText(
      "hours",
    );
  });

  // The e2e server has no archive bucket, so no connection can really be
  // archived: the API answers are faked and what is checked is the badge.
  test("an archived connection is badged in the list and on its page", async ({
    authenticatedPage: page,
  }) => {
    const uid = "3f1b0c9e-0000-4000-8000-0000000000a1";
    const at = new Date().toISOString();
    const base = {
      uid,
      user_id: "3f1b0c9e-0000-4000-8000-000000000003",
      database_id: "3f1b0c9e-0000-4000-8000-000000000004",
      source_ip: "127.0.0.1",
      connected_at: at,
      last_activity_at: at,
      disconnected_at: at,
      queries: 0,
      bytes_transferred: 0,
    };

    await page.route(/\/api\/v1\/connections(\?.*)?$/, (route) =>
      route.fulfill({
        json: { connections: [{ ...base, archived_at: at }] },
      }),
    );
    await page.route(`**/api/v1/connections/${uid}`, (route) =>
      route.fulfill({
        json: { ...base, archived_at: at, rows_location: "archive" },
      }),
    );

    await page.goto("connections");
    await expect(page.getByTestId("connection-archived-badge")).toBeVisible();

    await page.goto(`connections/${uid}`);
    await expect(page.getByTestId("connection-archived-badge")).toBeVisible();
  });
});
