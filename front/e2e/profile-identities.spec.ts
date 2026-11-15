import { test, expect } from "./fixtures";

/**
 * The profile page lists the SSO identities of the signed-in account with
 * link/unlink buttons. The E2E server configures a generic OIDC provider whose issuer is
 * never dialled, so this covers the entry point (user menu) and the page; the link
 * flow itself is covered by the Go tests against a recording provider.
 */
test.describe("Profile — linked identities", () => {
  test("reachable from the user menu and renders", async ({
    authenticatedPage: page,
  }) => {
    await page.goto("");
    await page.getByTestId("user-menu-trigger").click();
    await page.getByTestId("profile-menu-item").click();

    await expect(page.getByTestId("profile-page")).toBeVisible();
    // Whichever providers the server has configured (CI configures a generic
    // OIDC issuer), the account starts with none linked.
    await expect(page.getByTestId("identity-status-oidc")).toHaveText(
      "Not linked",
    );
    await expect(page.getByTestId("link-identity-oidc")).toBeVisible();
  });
});
