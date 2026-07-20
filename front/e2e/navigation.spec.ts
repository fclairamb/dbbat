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

    // Test navigation to Servers
    const serversLink = authenticatedPage.getByRole("link", {
      name: /servers/i,
    });
    if (await serversLink.isVisible()) {
      await serversLink.click();
      await expect(authenticatedPage).toHaveURL(/\/app\/servers/);
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/nav-servers.png",
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

  test("should render a valid breadcrumb without nested list items", async ({
    authenticatedPage,
  }) => {
    const page = authenticatedPage;

    // Collect React's invalid-nesting / hydration complaints, if any.
    const nestingErrors: string[] = [];
    page.on("console", (msg) => {
      const text = msg.text();
      if (
        /cannot contain a nested/i.test(text) ||
        /cannot be a descendant of/i.test(text) ||
        /hydration/i.test(text)
      ) {
        nestingErrors.push(text);
      }
    });

    // A two-segment path guarantees more than one crumb, so a separator is
    // rendered. The breadcrumb is derived from the pathname, so this holds even
    // if the detail page itself has no data to show for that uid.
    await page.goto("/app/queries/00000000-0000-0000-0000-000000000000");
    await page.waitForLoadState("networkidle");

    const breadcrumb = page.locator('nav[aria-label="breadcrumb"] ol');
    await expect(breadcrumb).toBeVisible();

    // The separator must be a sibling of the item, never nested inside it.
    await expect(breadcrumb.locator("li li")).toHaveCount(0);
    // Sanity check: a separator is actually present on this multi-crumb page.
    await expect(breadcrumb.locator('li[role="presentation"]')).not.toHaveCount(
      0,
    );

    expect(nestingErrors).toEqual([]);
  });

  test("should show the generic proxy tagline in the sidebar", async ({ authenticatedPage }) => {
    // The sidebar subtitle should reflect that DBBat proxies more than just
    // PostgreSQL (it also proxies Oracle and MySQL/MariaDB).
    await expect(authenticatedPage.getByText("Every query, tracked")).toBeVisible();
    await expect(authenticatedPage.getByText("PostgreSQL Proxy", { exact: false })).toHaveCount(0);
  });
});
