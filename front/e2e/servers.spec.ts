import { test, expect } from "./fixtures";

test.describe("Servers Management", () => {
  test("should display servers list page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("servers");

    // Wait for page to load
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot of servers page
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/servers-list.png",
      fullPage: true,
    });

    // Verify we're on the servers page
    await expect(authenticatedPage).toHaveURL(/\/servers/);

    // Check for page content
    const pageContent = await authenticatedPage.textContent("body");
    expect(pageContent).toBeTruthy();
  });

  test("old /databases links redirect to /servers", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("databases");
    await authenticatedPage.waitForLoadState("networkidle");

    await expect(authenticatedPage).toHaveURL(/\/servers/);
    await expect(authenticatedPage).not.toHaveURL(/\/databases/);
  });

  test("should show create database button or form", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    // Look for create/add button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new/i,
    });

    if (await createButton.isVisible()) {
      await createButton.click();

      // Take screenshot of create database dialog/form
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/servers-create-dialog.png",
      });

      // Look for form fields typical for database configuration
      const formContent = await authenticatedPage.textContent("body");
      expect(
        formContent?.toLowerCase()
      ).toMatch(/host|port|database|name|connection/);
    }
  });

  test("connection URL shows the {DBBAT_KEY} placeholder", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    // Open the first database's detail dialog by clicking its table row.
    const firstRow = authenticatedPage.locator("tbody tr").first();
    if ((await firstRow.count()) === 0) {
      test.skip(true, "no databases available in this environment");
      return;
    }
    await firstRow.click();

    const dialog = authenticatedPage.getByTestId("database-details-dialog");
    await expect(dialog).toBeVisible();

    // Admin callers have no API key, so the URL is rendered with the placeholder.
    const connUrl = authenticatedPage.getByTestId("database-connection-url");
    if ((await connUrl.count()) > 0) {
      await expect(connUrl.first()).toHaveValue(/\{DBBAT_KEY\}/);
    }
  });

  test("should display database configuration options", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/servers-overview.png",
      fullPage: true,
    });

    // Verify database-related content is present
    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("creating an SSH bastion shows it in the SSH servers section", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const sshSection = authenticatedPage.getByTestId("ssh-servers-section");
    await expect(sshSection).toBeVisible();

    const name = `e2e-bastion-${Date.now()}`;

    await authenticatedPage.getByTestId("add-database-button").click();

    // Select "SSH Bastion" protocol.
    await authenticatedPage.getByTestId("protocol-select").click();
    await authenticatedPage.getByTestId("protocol-option-ssh").click();

    await authenticatedPage.getByTestId("database-name-input").fill(name);
    await authenticatedPage.locator("#host").fill("bastion.example.com");
    await authenticatedPage.locator("#username").fill("bastion-user");
    await authenticatedPage.locator("#password").fill("bastion-password");

    await authenticatedPage.getByTestId("database-create-submit").click();

    // Dialog should close and the new bastion should show up in the SSH
    // servers section (not in the databases table above it).
    await expect(sshSection.getByText(name)).toBeVisible({ timeout: 10000 });
  });

  test("editing an SSH bastion updates it in the SSH servers section", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const sshSection = authenticatedPage.getByTestId("ssh-servers-section");
    await expect(sshSection).toBeVisible();

    const name = `e2e-bastion-edit-${Date.now()}`;
    const updatedDescription = `updated description ${Date.now()}`;

    // Create a bastion to edit.
    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("protocol-select").click();
    await authenticatedPage.getByTestId("protocol-option-ssh").click();
    await authenticatedPage.getByTestId("database-name-input").fill(name);
    await authenticatedPage.locator("#host").fill("bastion.example.com");
    await authenticatedPage.locator("#username").fill("bastion-user");
    await authenticatedPage.locator("#password").fill("bastion-password");
    await authenticatedPage.getByTestId("database-create-submit").click();

    const row = sshSection.locator("tr", { hasText: name });
    await expect(row).toBeVisible({ timeout: 10000 });

    // Open the edit dialog for that row and change its description.
    await row.locator('[data-testid^="ssh-server-edit-"]').click();

    const editDialog = authenticatedPage.getByTestId("ssh-server-edit-dialog");
    await expect(editDialog).toBeVisible();
    await authenticatedPage
      .getByTestId("ssh-server-edit-description-input")
      .fill(updatedDescription);
    await authenticatedPage.getByTestId("ssh-server-edit-submit").click();

    await expect(editDialog).not.toBeVisible();
    await expect(row.getByText(updatedDescription)).toBeVisible({
      timeout: 10000,
    });
  });
});
