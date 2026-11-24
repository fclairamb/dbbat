import { type Page } from "@playwright/test";
import { test, expect, loginAs } from "./fixtures";

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

async function openCreateGroupPage(page: Page) {
  await page.getByTestId("create-server-group-button").click();
  await expect(page.getByTestId("new-server-group-page")).toBeVisible();
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

    await openCreateGroupPage(page);
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

    await openCreateGroupPage(page);
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

    await openCreateGroupPage(page);
    await page.getByTestId("server-group-name").fill(groupName);
    await page.getByTestId("server-group-submit").click();
    await expect(page.getByText(groupName)).toBeVisible();

    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    await page.getByTestId("create-grant-definition-button").click();
    await expect(page.getByTestId("new-definition-page")).toBeVisible();

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
    await openCreateGroupPage(page);
    await page.getByTestId("server-group-name").fill(groupName);
    await page.getByTestId("server-group-submit").click();
    await expect(page.getByText(groupName)).toBeVisible();

    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");
    await page.getByTestId("create-grant-definition-button").click();
    await expect(page.getByTestId("new-definition-page")).toBeVisible();

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

  test("server group edit page opens by URL, survives reload and back returns to the list", async ({
    authenticatedPage: page,
  }) => {
    const name = `deeplink-${Date.now()}`;
    await page.goto(GROUPS_URL);
    await page.waitForLoadState("networkidle");
    await openCreateGroupPage(page);
    await page.getByTestId("server-group-name").fill(name);
    await page.getByTestId("server-group-submit").click();
    await expect(page).toHaveURL(/\/server-groups\/?$/);

    const row = page.locator("tr", { hasText: name });
    await row.locator('[data-testid^="edit-server-group-"]').click();
    await expect(page.getByTestId("edit-server-group-page")).toBeVisible();
    const url = page.url();
    expect(url).toMatch(/\/server-groups\/[^/]+$/);

    await page.reload();
    await expect(page.getByTestId("server-group-name")).toHaveValue(name);

    // Back returns to the list without saving the unsaved edit.
    await page.getByTestId("server-group-name").fill("never-saved");
    await page.goBack();
    await expect(page).toHaveURL(/\/server-groups\/?$/);
    await expect(page.getByText(name)).toBeVisible();
    await expect(page.getByText("never-saved")).toHaveCount(0);
  });

  test("an unknown uid shows the not-found state", async ({
    authenticatedPage: page,
  }) => {
    await page.goto(`${GROUPS_URL}/00000000-0000-0000-0000-000000000000`);
    await expect(page.getByTestId("entity-not-found")).toBeVisible();
    await expect(page.getByTestId("server-group-name")).toHaveCount(0);
  });

  test("a viewer is refused on the edit routes", async ({ page }) => {
    await loginAs(page, "viewer", "viewer");
    await page.goto(`${GROUPS_URL}/new`);
    await expect(page.getByText("Access Denied")).toBeVisible();
    await page.goto(`${GROUPS_URL}/00000000-0000-0000-0000-000000000000`);
    await expect(page.getByText("Access Denied")).toBeVisible();
  });
});
