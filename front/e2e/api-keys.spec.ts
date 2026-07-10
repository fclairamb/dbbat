import { test, expect } from "./fixtures";
import type { Page } from "@playwright/test";

/**
 * Covers the "own keys by default, admin review view" behavior.
 *
 * Test mode (`DBB_RUN_MODE=test`) provisions three stable, never-expiring keys:
 *   - admin     → "admin-test-key"
 *   - connector → "connector-test-key"
 *   - viewer    → "viewer-test-key"
 * (see main.go provisionTestData / internal/store/api_keys.go).
 */

async function login(page: Page, username: string, password: string) {
  await page.goto("/");
  await page.waitForLoadState("domcontentloaded");
  await page.getByTestId("login-logo").waitFor({ state: "visible", timeout: 10000 });
  await page.getByTestId("login-username").fill(username);
  await page.getByTestId("login-password").fill(password);
  await page.getByTestId("login-submit").click();
  await page.waitForURL((url) => !url.pathname.includes("/login"), {
    timeout: 10000,
  });
  await page.waitForLoadState("domcontentloaded");
}

test.describe("API Keys", () => {
  test("admin defaults to own keys and can review all keys", async ({
    authenticatedPage,
  }) => {
    const page = authenticatedPage;
    await page.goto("api-keys");
    await page.waitForLoadState("networkidle");

    // Default "My keys" view: only the admin's own seeded key is listed.
    await expect(page.getByText("admin-test-key")).toBeVisible();
    await expect(page.getByText("connector-test-key")).toHaveCount(0);
    await expect(page.getByText("viewer-test-key")).toHaveCount(0);

    // Admins get the scope switcher; the self view has no Owner column.
    await expect(page.getByTestId("api-keys-scope")).toBeVisible();
    await expect(page.getByTestId("api-key-owner")).toHaveCount(0);

    // Switch to the fleet-review "All keys" view.
    await page.getByTestId("api-keys-scope-all").click();
    await page.waitForLoadState("networkidle");

    // Every user's key is now listed.
    await expect(page.getByText("admin-test-key")).toBeVisible();
    await expect(page.getByText("connector-test-key")).toBeVisible();
    await expect(page.getByText("viewer-test-key")).toBeVisible();

    // The Owner column resolves the owning usernames.
    const owners = page.getByTestId("api-key-owner");
    await expect(owners.filter({ hasText: "connector" })).toHaveCount(1);
    await expect(owners.filter({ hasText: "viewer" })).toHaveCount(1);

    // All three seeded keys never expire → all visually flagged as long-term.
    await expect(page.getByTestId("api-key-longterm-flag")).toHaveCount(3);
  });

  test("non-admin sees only their own keys and no scope switcher", async ({
    page,
  }) => {
    await login(page, "viewer", "viewer");
    await page.goto("api-keys");
    await page.waitForLoadState("networkidle");

    // Only the viewer's own key; nobody else's.
    await expect(page.getByText("viewer-test-key")).toBeVisible();
    await expect(page.getByText("admin-test-key")).toHaveCount(0);
    await expect(page.getByText("connector-test-key")).toHaveCount(0);

    // No admin-only affordances.
    await expect(page.getByTestId("api-keys-scope")).toHaveCount(0);
    await expect(page.getByTestId("api-key-owner")).toHaveCount(0);
  });

  test("revoke confirmation dialog opens from a key row", async ({
    authenticatedPage,
  }) => {
    const page = authenticatedPage;
    await page.goto("api-keys");
    await page.waitForLoadState("networkidle");

    const row = page.getByRole("row", { name: /admin-test-key/ });
    await expect(row).toBeVisible();

    // The Ban action is the only button in the row.
    await row.getByRole("button").last().click();

    const dialog = page.getByRole("alertdialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText(/Revoke API Key/i)).toBeVisible();

    // Cancel without revoking to keep seeded data stable for other specs.
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog).toBeHidden();
  });
});
