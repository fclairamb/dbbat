import { test, expect } from "@playwright/test";
import { loginAs } from "./fixtures";

/**
 * Regression coverage for the TanStack Query cache leaking data across an
 * in-app identity change (login -> logout -> login as someone else, all
 * soft navigation, no page reload).
 *
 * The users list is a good probe: the backend lets admins and viewers see
 * every user, but restricts a "connector" role to seeing only itself
 * (internal/api/users.go handleListUsers). So after logging in as admin and
 * loading the full list, switching to "connector" must never render the
 * admin-cached rows (in particular "admin" itself) — it should only ever
 * show the connector's own row, whether from a fresh fetch or an
 * appropriately-cleared cache.
 */
test.describe("Query cache identity isolation", () => {
  test("logging out of admin and into connector does not leak admin-cached user rows", async ({
    page,
  }) => {
    // Log in as admin and load the users list so the query cache is
    // populated with the full, multi-row admin view.
    await loginAs(page, "admin", "admintest");
    await page.goto("users");
    await page.waitForLoadState("networkidle");

    await expect(page.getByTestId("edit-user-admin")).toBeVisible();
    await expect(page.getByTestId("edit-user-viewer")).toBeVisible();
    await expect(page.getByTestId("edit-user-connector")).toBeVisible();

    // Log out via the UI (in-app, no page reload).
    await page.getByTestId("user-menu-trigger").click();
    await page.getByTestId("logout-menu-item").click();
    await page.waitForURL((url) => url.pathname.includes("/login"), {
      timeout: 10000,
    });

    // Log back in as "connector" — a role restricted to seeing only itself.
    await page.getByTestId("login-username").fill("connector");
    await page.getByTestId("login-password").fill("connector");
    await page.getByTestId("login-submit").click();
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    await page.goto("users");
    await page.waitForLoadState("networkidle");

    // If the query cache had leaked across the identity change, the stale
    // admin-cached rows (including "admin" and "viewer") would still render
    // here despite connector's own fetch being scoped to itself.
    await expect(page.getByTestId("edit-user-connector")).toBeVisible();
    await expect(page.getByTestId("edit-user-admin")).toHaveCount(0);
    await expect(page.getByTestId("edit-user-viewer")).toHaveCount(0);
  });
});
