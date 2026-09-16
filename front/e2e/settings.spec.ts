import { test, expect } from "./fixtures";

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

    await expect(authenticatedPage.getByText(/limits saved/i)).toBeVisible({
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

    await expect(authenticatedPage.getByText(/limits saved/i)).toBeVisible({
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
    await authenticatedPage.getByTestId("limits-save-button").click();

    await expect(authenticatedPage.getByText(/limits saved/i)).toBeVisible({
      timeout: 10000,
    });

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
