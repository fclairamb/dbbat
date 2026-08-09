import { type Page } from "@playwright/test";
import { test, expect } from "./fixtures";

/**
 * Coverage for server groups — the unit rights are scoped on.
 *
 * Two things have to keep working from the UI: an admin can name a set of
 * servers and put a definition's scope on it, and the live-membership warning
 * is actually shown before an edit that moves live grants is saved. The second
 * is not decoration: server groups are the one place a live grant's reach
 * changes without a new version, so the operator has to be told.
 */

const GROUPS_URL = "server-groups";
const DEFS_URL = "grant-definitions";

async function openCreateGroupDialog(page: Page) {
  await page.getByTestId("create-server-group-button").click();
  await page.waitForSelector('[role="dialog"]');
  await expect(page.getByTestId("server-group-name")).toBeVisible();
}

test.describe("Server Groups", () => {
  test("admin can create a group holding a server", async ({
    authenticatedPage: page,
  }) => {
    const name = `replicas-${Date.now()}`;

    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");
    await expect(page).toHaveURL(/\/server-groups/);

    await openCreateGroupDialog(page);
    await page.getByTestId("server-group-name").fill(name);
    await page
      .getByTestId("server-group-description")
      .fill("Read replicas the analysts self-serve against");

    // Pick the first configured server as a member.
    const members = page.getByTestId("server-group-members");
    await expect(members).toBeVisible();
    await members.locator('button[role="checkbox"]').first().click();

    await page.getByTestId("server-group-submit").click();

    await expect(page.getByText(name)).toBeVisible();
    await page.screenshot({
      path: "test-results/screenshots/server-groups-list.png",
      fullPage: true,
    });
  });

  test("a new group has no live grants and therefore no warning", async ({
    authenticatedPage: page,
  }) => {
    const name = `fresh-${Date.now()}`;

    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");

    await openCreateGroupDialog(page);
    await page.getByTestId("server-group-name").fill(name);

    // Nothing is bound to a group that does not exist yet, so the blast-radius
    // warning must stay hidden — it is a real signal, not permanent chrome.
    await expect(
      page.getByTestId("server-group-live-membership-warning")
    ).toHaveCount(0);

    await page.getByTestId("server-group-submit").click();
    await expect(page.getByText(name)).toBeVisible();
  });

  test("group appears as a scope option in the definition editor", async ({
    authenticatedPage: page,
  }) => {
    const groupName = `staging-${Date.now()}`;

    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");

    await openCreateGroupDialog(page);
    await page.getByTestId("server-group-name").fill(groupName);
    await page.getByTestId("server-group-submit").click();
    await expect(page.getByText(groupName)).toBeVisible();

    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    await page.getByTestId("create-grant-definition-button").click();
    await page.waitForSelector('[role="dialog"]');

    const picker = page.getByTestId("grant-definition-server-groups");
    await expect(picker).toBeVisible();
    await expect(picker.getByText(groupName)).toBeVisible();
  });

  test("a definition scoped to a server group persists its scope", async ({
    authenticatedPage: page,
  }) => {
    const stamp = Date.now();
    const groupName = `scoped-srv-${stamp}`;
    const defName = `scoped-srv-def-${stamp}`;

    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");
    await openCreateGroupDialog(page);
    await page.getByTestId("server-group-name").fill(groupName);
    await page.getByTestId("server-group-submit").click();
    await expect(page.getByText(groupName)).toBeVisible();

    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");
    await page.getByTestId("create-grant-definition-button").click();
    await page.waitForSelector('[role="dialog"]');

    await page.getByTestId("grant-definition-name").fill(defName);
    await page.getByTestId("grant-definition-duration-value").fill("2");
    await page
      .getByTestId("grant-definition-server-groups")
      .getByText(groupName, { exact: true })
      .click();
    await page.getByTestId("grant-definition-submit").click();

    // The list summarises the scope instead of "all databases".
    const row = page.locator("tr", { hasText: defName });
    await expect(row).toBeVisible();
    await expect(row.getByText("1 server group")).toBeVisible();
    await expect(row.getByText("all users")).toBeVisible();
  });
});
