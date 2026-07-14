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
