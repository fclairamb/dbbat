import { test, expect } from "./fixtures";

/**
 * The API-keys page has to answer one question no other screen can: which of my
 * keys work for Oracle?
 *
 * Oracle login is O5LOGON, so it needs a verifier derived from the key at mint
 * time, and keys are stored hashed — a key created before Oracle support can
 * never be used against an Oracle database, while working perfectly for the API
 * and every other protocol. Test mode seeds one of each (`admin-test-key` with a
 * verifier, `admin-legacy-key` without) precisely so this is observable.
 */
test.describe("API keys — Oracle capability", () => {
  test("marks a key that cannot be used for Oracle", async ({
    authenticatedPage: page,
  }) => {
    // Relative, like every other spec: `baseURL` is `http://localhost:8080/app/`
    // and a leading slash would drop the `/app/` base, landing on the Go
    // server's own 404 instead of the SPA.
    await page.goto("api-keys");
    await page.waitForLoadState("networkidle");

    // `dbb_legacy_admin_key` truncates to this prefix (first 8 chars), which is
    // what the badge's test id is keyed on. `.first()` because test-mode
    // provisioning re-seeds its stable keys on every server start, so a database
    // that has seen several starts holds several rows with this prefix.
    await expect(
      page.getByTestId("oracle-unsupported-dbb_lega").first(),
    ).toBeVisible();

    await expect(page.getByTestId("oracle-unsupported-alert")).toContainText(
      "cannot be used for Oracle",
    );

    await page.screenshot({
      path: "test-results/screenshots/api-keys-oracle-capability.png",
      fullPage: true,
    });
  });

  test("a freshly created key is Oracle-ready", async ({
    authenticatedPage: page,
  }) => {
    await page.goto("api-keys");
    await page.waitForLoadState("networkidle");

    await page.getByRole("button", { name: "Create Key" }).click();
    await page.getByLabel("Name").fill("oracle-capability-probe");
    await page.getByRole("button", { name: "Create", exact: true }).click();

    // Close the "here is your key" dialog and read the row back off the list.
    await page.getByTestId("api-key-created-close").click();
    await page.waitForLoadState("networkidle");

    const row = page
      .getByRole("row")
      .filter({ hasText: "oracle-capability-probe" });
    await expect(row).toContainText("Ready");
  });
});
