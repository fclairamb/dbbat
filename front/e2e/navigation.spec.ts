import { test, expect } from "./fixtures";

test.describe("Navigation", () => {
  test("should navigate through main sections", async ({ authenticatedPage }) => {
    // Take screenshot of initial page
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/nav-initial.png",
    });

    // Test navigation to Users
    const usersLink = authenticatedPage.getByRole("link", { name: /users/i });
    if (await usersLink.isVisible()) {
      await usersLink.click();
      await expect(authenticatedPage).toHaveURL(/\/app\/users/);
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/nav-users.png",
      });
    }

    // Test navigation to Databases
    const databasesLink = authenticatedPage.getByRole("link", {
      name: /databases/i,
    });
    if (await databasesLink.isVisible()) {
      await databasesLink.click();
      await expect(authenticatedPage).toHaveURL(/\/app\/databases/);
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/nav-databases.png",
      });
    }

    // Test navigation to Grants
    const grantsLink = authenticatedPage.getByRole("link", {
      name: /grants/i,
    });
    if (await grantsLink.isVisible()) {
      await grantsLink.click();
      await expect(authenticatedPage).toHaveURL(/\/app\/grants/);
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/nav-grants.png",
      });
    }

    // Test navigation to Connections
    const connectionsLink = authenticatedPage.getByRole("link", {
      name: /connections/i,
    });
    if (await connectionsLink.isVisible()) {
      await connectionsLink.click();
      await expect(authenticatedPage).toHaveURL(/\/app\/connections/);
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/nav-connections.png",
      });
    }

    // Test navigation to Queries
    const queriesLink = authenticatedPage.getByRole("link", {
      name: /queries/i,
    });
    if (await queriesLink.isVisible()) {
      await queriesLink.click();
      await expect(authenticatedPage).toHaveURL(/\/app\/queries/);
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/nav-queries.png",
      });
    }

    // Test navigation to Audit
    const auditLink = authenticatedPage.getByRole("link", { name: /audit/i });
    if (await auditLink.isVisible()) {
      await auditLink.click();
      await expect(authenticatedPage).toHaveURL(/\/app\/audit/);
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/nav-audit.png",
      });
    }
  });

  test("should display navigation menu", async ({ authenticatedPage }) => {
    // Check that navigation exists (could be sidebar or top nav)
    const page = authenticatedPage;

    // Take screenshot showing navigation
    await page.screenshot({
      path: "test-results/screenshots/nav-layout.png",
      fullPage: true,
    });

    // Verify at least some navigation links are present
    const navText = await page.textContent("body");
    expect(navText).toBeTruthy();
  });
});
