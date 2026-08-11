import { test, expect } from "./fixtures";

test.describe("Users Management", () => {
  test("should display users list page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("users");

    // Wait for page to load
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot of users page
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-list.png",
      fullPage: true,
    });

    // Verify we're on the users page
    await expect(authenticatedPage).toHaveURL(/\/users/);

    // Check for common elements (table, buttons, etc.)
    const pageContent = await authenticatedPage.textContent("body");
    expect(pageContent).toBeTruthy();
  });

  test("should show create user button or form", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("users");
    await authenticatedPage.waitForLoadState("networkidle");

    // Look for create/add button (could be various text)
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new/i,
    });

    if (await createButton.isVisible()) {
      await createButton.click();

      // Take screenshot of create user dialog/form
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/users-create-dialog.png",
      });

      // Look for form fields
      const formContent = await authenticatedPage.textContent("body");
      expect(formContent?.toLowerCase()).toMatch(/username|password|name/);
    }
  });

  test("should navigate to users page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("users");
    await authenticatedPage.waitForLoadState("load");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-navigation.png",
      fullPage: true,
    });

    // Verify we're on the users page
    await expect(authenticatedPage).toHaveURL(/\/users/);
  });

  test("should prevent demoting the last admin", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("users");
    await authenticatedPage.waitForLoadState("networkidle");

    // In test mode "admin" is the only admin user: its Administrator
    // checkbox must be locked in the edit dialog.
    await authenticatedPage.getByTestId("edit-user-admin").click();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeVisible();
    await expect(
      authenticatedPage.getByTestId("edit-user-username")
    ).toHaveValue("admin");
    await expect(
      authenticatedPage.getByTestId("edit-user-role-admin")
    ).toBeDisabled();

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-edit-last-admin-locked.png",
    });

    // Close the dialog without changes
    await authenticatedPage.keyboard.press("Escape");
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeHidden();
  });

  test("should promote and demote a user's admin role", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("users");
    await authenticatedPage.waitForLoadState("networkidle");

    // Scope assertions to the row of the "connector" test user
    const connectorRow = authenticatedPage.getByRole("row").filter({
      has: authenticatedPage.getByTestId("edit-user-connector"),
    });
    await expect(connectorRow).toBeVisible();
    await expect(
      connectorRow.getByText("admin", { exact: true })
    ).toHaveCount(0);

    // Promote: open the edit dialog and check the Administrator role
    await authenticatedPage.getByTestId("edit-user-connector").click();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeVisible();
    await expect(
      authenticatedPage.getByTestId("edit-user-username")
    ).toHaveValue("connector");

    const adminCheckbox = authenticatedPage.getByTestId(
      "edit-user-role-admin"
    );
    await expect(adminCheckbox).toHaveAttribute("aria-checked", "false");
    await adminCheckbox.click();
    await expect(adminCheckbox).toHaveAttribute("aria-checked", "true");

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-edit-promote-dialog.png",
    });

    await authenticatedPage.getByTestId("edit-user-submit").click();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeHidden();

    // Verify the admin badge now shows in the connector row
    await expect(
      connectorRow.getByText("admin", { exact: true })
    ).toBeVisible();

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-promoted.png",
      fullPage: true,
    });

    // Demote: uncheck the Administrator role again
    await authenticatedPage.getByTestId("edit-user-connector").click();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeVisible();
    await expect(adminCheckbox).toHaveAttribute("aria-checked", "true");
    await adminCheckbox.click();
    await expect(adminCheckbox).toHaveAttribute("aria-checked", "false");
    await authenticatedPage.getByTestId("edit-user-submit").click();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeHidden();

    // Verify the admin badge is gone from the connector row
    await expect(
      connectorRow.getByText("admin", { exact: true })
    ).toHaveCount(0);

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-demoted.png",
      fullPage: true,
    });
  });

  // The E2E server runs with DBB_OIDC_ROLE_MAPPING="viewer=analysts", so
  // `viewer` is owned by the directory and re-applied at every login. The page
  // has to say so — otherwise an admin edits it, sees a success toast, and the
  // next SSO login quietly reverts them.
  test("should mark SSO-managed roles and warn before editing one", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("users");
    await authenticatedPage.waitForLoadState("networkidle");

    const viewerRow = authenticatedPage.getByRole("row").filter({
      has: authenticatedPage.getByTestId("edit-user-viewer"),
    });

    // The list badges the managed role, and carries the "last synced from SSO"
    // column (test mode seeds one sync against the viewer).
    await expect(viewerRow.getByTestId("managed-role-viewer")).toBeVisible();
    await expect(authenticatedPage.getByTestId("last-sync-viewer")).toBeVisible();

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-sso-managed-roles.png",
      fullPage: true,
    });

    // The edit form says the same, and answers "why did this change?" with the
    // last sync, groups included.
    await authenticatedPage.getByTestId("edit-user-viewer").click();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeVisible();
    await expect(
      authenticatedPage.getByTestId("managed-role-badge-viewer")
    ).toBeVisible();
    const lastSync = authenticatedPage.getByTestId("last-role-sync");
    await expect(lastSync).toBeVisible();
    await expect(lastSync).toContainText("analysts");

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-edit-sso-managed.png",
    });

    await authenticatedPage.keyboard.press("Escape");
    await expect(authenticatedPage.getByTestId("edit-user-dialog")).toBeHidden();

    // Touching a managed role must be confirmed. Cancelling leaves the edit
    // dialog open with the change still pending, not saved.
    await authenticatedPage.getByTestId("edit-user-connector").click();
    const viewerCheckbox = authenticatedPage.getByTestId(
      "edit-user-role-viewer"
    );
    await expect(viewerCheckbox).toHaveAttribute("aria-checked", "false");
    await viewerCheckbox.click();
    await authenticatedPage.getByTestId("edit-user-submit").click();

    const warning = authenticatedPage.getByTestId(
      "edit-user-managed-roles-dialog"
    );
    await expect(warning).toBeVisible();
    await expect(warning).toContainText("viewer");

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/users-managed-role-warning.png",
    });

    await authenticatedPage.getByTestId("edit-user-managed-roles-cancel").click();
    await expect(warning).toBeHidden();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeVisible();

    // Confirming saves it: the role is editable, just not durable.
    await authenticatedPage.getByTestId("edit-user-submit").click();
    await authenticatedPage
      .getByTestId("edit-user-managed-roles-confirm")
      .click();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeHidden();

    const connectorRow = authenticatedPage.getByRole("row").filter({
      has: authenticatedPage.getByTestId("edit-user-connector"),
    });
    await expect(
      connectorRow.getByText("viewer", { exact: true })
    ).toBeVisible();

    // Put the connector user back the way the other tests expect it.
    await authenticatedPage.getByTestId("edit-user-connector").click();
    await viewerCheckbox.click();
    await authenticatedPage.getByTestId("edit-user-submit").click();
    await authenticatedPage
      .getByTestId("edit-user-managed-roles-confirm")
      .click();
    await expect(
      authenticatedPage.getByTestId("edit-user-dialog")
    ).toBeHidden();
    await expect(
      connectorRow.getByText("viewer", { exact: true })
    ).toHaveCount(0);
  });
});
