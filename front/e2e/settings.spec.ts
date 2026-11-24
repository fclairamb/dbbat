import { test, expect, loginAs } from "./fixtures";

test.describe("Settings — instance configuration", () => {
  test("local listeners are grouped into HTTP and TCP sections", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("settings");
    await authenticatedPage.waitForLoadState("networkidle");

    const httpGroup = authenticatedPage.getByTestId("http-listener-group");
    const tcpGroup = authenticatedPage.getByTestId("tcp-listener-group");

    await expect(httpGroup).toBeVisible();
    await expect(tcpGroup).toBeVisible();

    // The HTTP group only advertises the API/Web UI listener.
    await expect(httpGroup).toContainText("API / Web UI");
    await expect(httpGroup).not.toContainText("PostgreSQL");

    // The TCP group advertises the three SQL proxy listeners, not the API.
    await expect(tcpGroup).toContainText("PostgreSQL");
    await expect(tcpGroup).toContainText("Oracle");
    await expect(tcpGroup).toContainText("MySQL");
  });

  test("Web UI host and connection host are two distinct, editable sections", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("settings");
    await authenticatedPage.waitForLoadState("networkidle");

    const webUISection = authenticatedPage.getByTestId("web-ui-host-section");
    const connectionSection = authenticatedPage.getByTestId(
      "connection-host-section"
    );

    await expect(webUISection).toBeVisible();
    await expect(connectionSection).toBeVisible();

    // Web UI host field accepts a full base URL.
    const webUIInput = authenticatedPage.getByTestId(
      "public-web-ui-url-input"
    );
    await webUIInput.fill("https://dbbat.example.com");

    // Connection host field is separate and keeps its own value.
    const hostInput = authenticatedPage.getByTestId("public-host-input");
    await hostInput.fill("db.example.com");

    await authenticatedPage
      .getByTestId("save-public-settings-btn")
      .click();

    await expect(authenticatedPage.getByText(/settings saved/i)).toBeVisible({
      timeout: 10000,
    });

    // Reload and confirm both values round-tripped independently.
    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");

    await expect(
      authenticatedPage.getByTestId("public-web-ui-url-input")
    ).toHaveValue("https://dbbat.example.com");
    await expect(
      authenticatedPage.getByTestId("public-host-input")
    ).toHaveValue("db.example.com");
  });
});

/**
 * The instance-wide per-statement time limit (`limits.statement_timeout`).
 *
 * Unlike the public.* endpoints above, this field has a three-way meaning the
 * UI has to make legible: empty falls back to `DBB_STATEMENT_TIMEOUT`, "0"
 * disables the limit outright, and a duration sets it. The "currently in
 * effect" line is what tells an operator which of the three they are looking
 * at, so it is asserted alongside every write.
 *
 * Serial, and one test rather than three: the parameter is instance-wide
 * shared state, so a set and the clear that undoes it must not interleave.
 */
test.describe("Settings — instance-wide statement timeout", () => {
  test.describe.configure({ mode: "serial" });

  test("setting, reading back and clearing the global statement timeout", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("settings");
    await authenticatedPage.waitForLoadState("networkidle");

    const section = authenticatedPage.getByTestId("limits-section");
    await expect(section).toBeVisible();

    const input = authenticatedPage.getByTestId(
      "limits-statement-timeout-input"
    );
    const save = authenticatedPage.getByTestId("limits-save-button");

    // Set a real limit.
    await input.fill("30s");
    await save.click();

    await expect(authenticatedPage.getByText(/limits saved/i).first()).toBeVisible({
      timeout: 10000,
    });

    // The resolved line updates from the refetched instance, and says the
    // value came from this setting rather than from the environment.
    await expect(section).toContainText("Currently in effect: 30s");
    await expect(section).toContainText("from this setting");

    // It round-trips: the stored parameter, not just the typed value.
    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");

    await expect(
      authenticatedPage.getByTestId("limits-statement-timeout-input")
    ).toHaveValue("30s");
    await expect(
      authenticatedPage.getByTestId("limits-section")
    ).toContainText("Currently in effect: 30s");

    // A value that is not a Go duration is refused at the edge rather than
    // silently disabling the limit — the failure mode this field exists to
    // prevent.
    await authenticatedPage
      .getByTestId("limits-statement-timeout-input")
      .fill("thirty");
    await authenticatedPage.getByTestId("limits-save-button").click();

    await expect(
      authenticatedPage.getByText(/must be a Go duration/i)
    ).toBeVisible({ timeout: 10000 });

    // "0" is a valid choice, and a different one from empty: it disables the
    // limit outright instead of falling back to DBB_STATEMENT_TIMEOUT.
    await authenticatedPage
      .getByTestId("limits-statement-timeout-input")
      .fill("0");
    await authenticatedPage.getByTestId("limits-save-button").click();

    await expect(authenticatedPage.getByText(/limits saved/i).first()).toBeVisible({
      timeout: 10000,
    });
    await expect(
      authenticatedPage.getByTestId("limits-section")
    ).toContainText("Currently in effect: no limit");

    // Clearing the field deletes the parameter, so the resolved value falls
    // back to the environment — which the E2E server does not set, so there is
    // no limit and no source to name.
    await authenticatedPage
      .getByTestId("limits-statement-timeout-input")
      .fill("");
    // The previous save's toast is still on screen, so it cannot prove this
    // save landed: wait for the PUT itself before reloading, or the reload
    // cancels the request in flight.
    const cleared = authenticatedPage.waitForResponse(
      (r) =>
        r.url().includes("/instance/limits") && r.request().method() === "PUT"
    );
    await authenticatedPage.getByTestId("limits-save-button").click();
    expect((await cleared).status()).toBe(204);

    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");

    await expect(
      authenticatedPage.getByTestId("limits-statement-timeout-input")
    ).toHaveValue("");
    await expect(
      authenticatedPage.getByTestId("limits-section")
    ).toContainText("nothing is configured");
  });
});

test.describe("Settings — parameter edit route", () => {
  // Saving the public settings stores parameters, so the raw list is never
  // empty afterwards.
  async function seedParameter(page: import("@playwright/test").Page) {
    await page.goto("settings");
    await page.waitForLoadState("networkidle");
    await page.getByTestId("public-host-input").fill("params.example.com");
    await page.getByTestId("save-public-settings-btn").click();
    await expect(page.getByText(/settings saved/i)).toBeVisible({
      timeout: 10000,
    });
  }

  async function openFirstParameter(page: import("@playwright/test").Page) {
    await page.getByTestId("raw-parameters-section").getByText("Raw parameters").click();
    await page
      .getByTestId("raw-parameters-section")
      .locator('[data-testid^="edit-parameter-"]')
      .first()
      .click();
    await expect(page.getByTestId("edit-parameter-page")).toBeVisible();
    // The list may have been cached before the last save; the page re-seeds
    // from the refetched row, so read the value once it has settled.
    await page.waitForLoadState("networkidle");
  }

  test("the edit page opens by URL, survives reload and back returns to settings", async ({
    authenticatedPage: page,
  }) => {
    await seedParameter(page);
    await openFirstParameter(page);
    const url = page.url();
    expect(url).toMatch(/\/settings\/parameters\/[^/]+$/);
    const value = await page.getByTestId("parameter-value").inputValue();

    await page.reload();
    await expect(page.getByTestId("parameter-value")).toHaveValue(value);

    await page.goto(url);
    await expect(page.getByTestId("parameter-value")).toHaveValue(value);

    // An unsaved edit is dropped when going back.
    await page.getByTestId("parameter-value").fill("never-saved");
    await page.getByTestId("form-cancel").click();
    await expect(page).toHaveURL(/\/settings\/?$/);
    await expect(page.getByText("never-saved")).toHaveCount(0);
  });

  test("saving a parameter returns to settings", async ({
    authenticatedPage: page,
  }) => {
    await seedParameter(page);
    await openFirstParameter(page);
    const value = await page.getByTestId("parameter-value").inputValue();
    await page.getByTestId("parameter-value").fill(value);
    await page.getByTestId("parameter-submit").click();
    await expect(page.getByText("Parameter updated")).toBeVisible();
    await expect(page).toHaveURL(/\/settings\/?$/);
  });

  test("an unknown parameter shows the not-found state", async ({
    authenticatedPage: page,
  }) => {
    await page.goto("settings/parameters/nope:nothing");
    await expect(page.getByTestId("entity-not-found")).toBeVisible();
    await expect(page.getByTestId("parameter-value")).toHaveCount(0);
  });

  test("a viewer is refused on the parameter edit route", async ({ page }) => {
    await loginAs(page, "viewer", "viewer");
    await page.goto("settings/parameters/nope:nothing");
    await expect(page.getByText("Access Denied")).toBeVisible();
  });
});
