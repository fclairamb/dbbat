import { test, expect } from "./fixtures";

/**
 * The observability filter bar.
 *
 * Two properties matter more than any individual control, and both are here:
 *
 *  1. **Filters round-trip through the URL.** Picking one puts it in the search
 *     params, and loading that URL restores the picked state — which is what
 *     makes a filtered view shareable and the back button meaningful.
 *  2. **Changing a filter clears the pagination cursor.** `before` is produced
 *     by one result set; carried into another it silently hides the head of the
 *     list, which is the failure nobody notices.
 */
test.describe("Observability filters", () => {
  test("connections: picking a user round-trips through the URL", async ({
    authenticatedPage: page,
  }) => {
    await page.goto("connections");
    await page.waitForLoadState("networkidle");

    await expect(page.getByTestId("observability-filters")).toBeVisible();

    // Open the User select and pick the first real option (the first entry is
    // the "Any user" reset).
    await page.getByTestId("filter-user").click();
    const options = page.locator('[role="option"]');
    await expect(options.first()).toBeVisible();

    const optionCount = await options.count();
    if (optionCount < 2) {
      test.skip(true, "No users available to filter by in this environment");
      return;
    }

    const picked = options.nth(1);
    const pickedLabel = (await picked.textContent())?.trim() ?? "";
    await picked.click();

    // The choice is now in the URL...
    await expect(page).toHaveURL(/[?&]user_id=/);

    // ...and reloading that URL restores it, rather than resetting the bar.
    await page.reload();
    await page.waitForLoadState("networkidle");
    await expect(page.getByTestId("filter-user")).toContainText(pickedLabel);
  });

  test("connections: changing a filter clears the pagination cursor", async ({
    authenticatedPage: page,
  }) => {
    // Start from a URL that already carries a cursor, as the "Older" button
    // produces. The cursor value need not name a real row — what is under test
    // is that it does not survive a filter change.
    await page.goto(
      "connections?size=50&before=ffffffff-ffff-7fff-bfff-ffffffffffff",
    );
    await page.waitForLoadState("networkidle");
    await expect(page).toHaveURL(/[?&]before=/);

    await page.getByTestId("filter-user").click();
    const options = page.locator('[role="option"]');
    await expect(options.first()).toBeVisible();

    if ((await options.count()) < 2) {
      test.skip(true, "No users available to filter by in this environment");
      return;
    }

    await options.nth(1).click();

    await expect(page).toHaveURL(/[?&]user_id=/);
    await expect(page).not.toHaveURL(/[?&]before=/);
  });

  test("queries: the provenance and approval filters round-trip", async ({
    authenticatedPage: page,
  }) => {
    await page.goto("queries");
    await page.waitForLoadState("networkidle");

    await expect(page.getByTestId("observability-filters")).toBeVisible();

    // Grant provenance is a checkbox list: it says which *access* the session
    // ran under.
    await page.getByTestId("filter-grant-provenance-option-approved").click();
    await expect(page).toHaveURL(/[?&]grant_provenance=approved/);

    // Approval status is a different axis: which individual *statements* a
    // human released. Both must coexist in the URL.
    await page.getByTestId("filter-approval-status").click();
    await page.locator('[role="option"]', { hasText: "Pending" }).click();
    await expect(page).toHaveURL(/[?&]approval_status=pending/);
    await expect(page).toHaveURL(/[?&]grant_provenance=approved/);

    // Reload: both survive.
    await page.reload();
    await page.waitForLoadState("networkidle");
    await expect(
      page.getByTestId("filter-grant-provenance-option-approved"),
    ).toBeChecked();
    await expect(page.getByTestId("filter-approval-status")).toContainText(
      "Pending",
    );

    // Clearing everything empties the search params again.
    await page.getByTestId("filter-clear-all").click();
    await expect(page).not.toHaveURL(/[?&]grant_provenance=/);
    await expect(page).not.toHaveURL(/[?&]approval_status=/);
  });

  test("users list shows last login and last SQL connection", async ({
    authenticatedPage: page,
  }) => {
    await page.goto("users");
    await page.waitForLoadState("networkidle");

    const headerRow = page.locator("thead tr");
    await expect(
      headerRow.getByText("Last login", { exact: true }),
    ).toBeVisible();
    await expect(
      headerRow.getByText("Last SQL connection", { exact: true }),
    ).toBeVisible();

    // The admin driving this test just signed in, so their Last login cell can
    // never read "Never".
    await expect(page.getByTestId("last-login-admin")).not.toHaveText("Never");
  });
});
