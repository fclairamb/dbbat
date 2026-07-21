import { type Page } from "@playwright/test";
import { test, expect } from "./fixtures";

/**
 * Coverage for user groups and the grant-definition scoping they enable.
 *
 * Scope is what turns auto-approve into a real policy lever, so the pieces
 * an admin drives from the UI — create a group, put a user in it, restrict a
 * definition to it — need to keep working.
 */

const GROUPS_URL = "user-groups";
const DEFS_URL = "grant-definitions";

async function openCreateGroupDialog(page: Page) {
  await page.getByTestId("create-user-group-button").click();
  await page.waitForSelector('[role="dialog"]');
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

    await openCreateGroupDialog(page);
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

    await openCreateGroupDialog(page);
    await page.getByTestId("user-group-name").fill(groupName);
    await page.getByTestId("user-group-submit").click();
    await expect(page.getByText(groupName)).toBeVisible();

    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    await page.getByTestId("create-grant-definition-button").click();
    await page.waitForSelector('[role="dialog"]');

    const groupPicker = page.getByTestId("grant-definition-groups");
    await expect(groupPicker).toBeVisible();
    await expect(groupPicker.getByText(groupName)).toBeVisible();

    // Databases are the second scope axis and must be offered too.
    await expect(page.getByTestId("grant-definition-databases")).toBeVisible();

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
    await openCreateGroupDialog(page);
    await page.getByTestId("user-group-name").fill(groupName);
    await page.getByTestId("user-group-submit").click();
    await expect(page.getByText(groupName)).toBeVisible();

    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");
    await page.getByTestId("create-grant-definition-button").click();
    await page.waitForSelector('[role="dialog"]');

    await page.getByTestId("grant-definition-name").fill(defName);
    await page.getByTestId("grant-definition-duration-value").fill("2");
    await page
      .getByTestId("grant-definition-groups")
      .getByText(groupName, { exact: true })
      .click();
    await page.getByTestId("grant-definition-submit").click();

    // The list summarises the scope instead of "all users".
    const row = page.locator("tr", { hasText: defName });
    await expect(row).toBeVisible();
    await expect(row.getByText("1 group")).toBeVisible();
    await expect(row.getByText("all databases")).toBeVisible();

    // Re-opening the editor shows the scope checked, not blank.
    await page.locator(`[data-testid^="edit-grant-definition-"]`).first();
    const editButton = row.locator('[data-testid^="edit-grant-definition-"]');
    await editButton.click();
    await page.waitForSelector('[role="dialog"]');

    const checkbox = page.getByTestId("grant-definition-groups").locator(
      'button[role="checkbox"][data-state="checked"]'
    );
    await expect(checkbox).toHaveCount(1);
  });
});
