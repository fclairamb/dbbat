import { type Page } from "@playwright/test";
import { test, expect, loginAs } from "./fixtures";

/**
 * Coverage for user groups and the grant-definition scoping they enable.
 *
 * Scope is what turns auto-approve into a real policy lever, so the pieces
 * an admin drives from the UI — create a group, put a user in it, restrict a
 * definition to it — need to keep working.
 */

const GROUPS_URL = "user-groups";
const DEFS_URL = "grant-definitions";

async function openCreateGroupPage(page: Page) {
  await page.getByTestId("create-user-group-button").click();
  await expect(page.getByTestId("new-user-group-page")).toBeVisible();
  await expect(page.getByTestId("user-group-name")).toBeVisible();
}

test.describe("User Groups", () => {
  test("admin can create a group with a member", async ({
    authenticatedPage: page,
  }) => {
    const name = `analysts-${Date.now()}`;

    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");
    await expect(page).toHaveURL(/\/user-groups/);

    await openCreateGroupPage(page);
    await page.getByTestId("user-group-name").fill(name);
    await page
      .getByTestId("user-group-description")
      .fill("Analysts who self-serve read-only access");

    // Pick the seeded `viewer` user as a member.
    const members = page.getByTestId("user-group-members");
    await expect(members).toBeVisible();
    await members.getByText("viewer", { exact: true }).click();

    await page.getByTestId("user-group-submit").click();

    await expect(page.getByText(name)).toBeVisible();
    await page.screenshot({
      path: "test-results/screenshots/user-groups-list.png",
      fullPage: true,
    });

    // The member shows up in the group row.
    const row = page.locator("tr", { hasText: name });
    await expect(row.getByText("viewer")).toBeVisible();
  });

  test("group appears as a scope option in the definition editor", async ({
    authenticatedPage: page,
  }) => {
    const groupName = `sre-${Date.now()}`;

    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");

    await openCreateGroupPage(page);
    await page.getByTestId("user-group-name").fill(groupName);
    await page.getByTestId("user-group-submit").click();
    await expect(page.getByText(groupName)).toBeVisible();

    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    await page.getByTestId("create-grant-definition-button").click();
    await expect(page.getByTestId("new-definition-page")).toBeVisible();

    const groupPicker = page.getByTestId("grant-definition-user-groups");
    await expect(groupPicker).toBeVisible();
    await expect(groupPicker.getByText(groupName)).toBeVisible();

    // Server groups are the second scope axis and must be offered too — a
    // definition scopes on groups of servers, never on servers directly.
    await expect(
      page.getByTestId("grant-definition-server-groups")
    ).toBeVisible();

    await page.screenshot({
      path: "test-results/screenshots/grant-definition-scope.png",
      fullPage: true,
    });
  });

  test("a definition scoped to a group persists its scope", async ({
    authenticatedPage: page,
  }) => {
    const stamp = Date.now();
    const groupName = `scoped-grp-${stamp}`;
    const defName = `scoped-def-${stamp}`;

    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");
    await openCreateGroupPage(page);
    await page.getByTestId("user-group-name").fill(groupName);
    await page.getByTestId("user-group-submit").click();
    await expect(page.getByText(groupName)).toBeVisible();

    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");
    await page.getByTestId("create-grant-definition-button").click();
    await expect(page.getByTestId("new-definition-page")).toBeVisible();

    await page.getByTestId("grant-definition-name").fill(defName);
    await page.getByTestId("grant-definition-duration-value").fill("2");
    await page
      .getByTestId("grant-definition-user-groups")
      .getByText(groupName, { exact: true })
      .click();
    await page.getByTestId("grant-definition-submit").click();

    // The list summarises the scope instead of "all users".
    const row = page.locator("tr", { hasText: defName });
    await expect(row).toBeVisible();
    await expect(row.getByText("1 user group")).toBeVisible();
    await expect(row.getByText("all databases")).toBeVisible();

    // Re-opening the editor shows the scope checked, not blank.
    await page.locator(`[data-testid^="edit-grant-definition-"]`).first();
    const editButton = row.locator('[data-testid^="edit-grant-definition-"]');
    await editButton.click();
    await expect(page.getByTestId("edit-definition-page")).toBeVisible();

    const checkbox = page.getByTestId("grant-definition-user-groups").locator(
      'button[role="checkbox"][data-state="checked"]'
    );
    await expect(checkbox).toHaveCount(1);
  });

  test("user group edit page opens by URL, survives reload and back returns to the list", async ({
    authenticatedPage: page,
  }) => {
    const name = `deeplink-${Date.now()}`;
    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");
    await openCreateGroupPage(page);
    await page.getByTestId("user-group-name").fill(name);
    await page.getByTestId("user-group-submit").click();
    await expect(page).toHaveURL(/\/user-groups\/?$/);

    const row = page.locator("tr", { hasText: name });
    await row.locator('[data-testid^="edit-user-group-"]').click();
    await expect(page.getByTestId("edit-user-group-page")).toBeVisible();
    const url = page.url();
    expect(url).toMatch(/\/user-groups\/[^/]+$/);

    await page.reload();
    await expect(page.getByTestId("user-group-name")).toHaveValue(name);

    // Back returns to the list without saving the unsaved edit.
    await page.getByTestId("user-group-name").fill("never-saved");
    await page.goBack();
    await expect(page).toHaveURL(/\/user-groups\/?$/);
    await expect(page.getByText(name)).toBeVisible();
    await expect(page.getByText("never-saved")).toHaveCount(0);
  });

  test("an unknown uid shows the not-found state", async ({
    authenticatedPage: page,
  }) => {
    await page.goto(`${GROUPS_URL}/00000000-0000-0000-0000-000000000000`);
    await expect(page.getByTestId("entity-not-found")).toBeVisible();
    await expect(page.getByTestId("user-group-name")).toHaveCount(0);
  });

  test("a viewer is refused on the edit routes", async ({ page }) => {
    await loginAs(page, "viewer", "viewer");
    await page.goto(`${GROUPS_URL}/new`);
    await expect(page.getByText("Access Denied")).toBeVisible();
    await page.goto(`${GROUPS_URL}/00000000-0000-0000-0000-000000000000`);
    await expect(page.getByText("Access Denied")).toBeVisible();
  });
});
