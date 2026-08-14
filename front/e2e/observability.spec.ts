import { test, expect } from "./fixtures";

test.describe("Observability Features", () => {
  test("should display connections page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/connections-page.png",
      fullPage: true,
    });

    // Verify we're on the connections page
    await expect(authenticatedPage).toHaveURL(/\/connections/);

    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("should display queries page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/queries-page.png",
      fullPage: true,
    });

    // Verify we're on the queries page
    await expect(authenticatedPage).toHaveURL(/\/queries/);

    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("queries list shows user, database, and connection columns", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    // Column headers are present regardless of whether any rows exist.
    const headerRow = authenticatedPage.locator("thead tr");
    await expect(headerRow.getByText("User", { exact: true })).toBeVisible();
    await expect(
      headerRow.getByText("Database", { exact: true }),
    ).toBeVisible();
    await expect(
      headerRow.getByText("Connection", { exact: true }),
    ).toBeVisible();

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No query rows available in this environment");
      return;
    }

    // Test-mode seeds one query executed by the admin user against
    // proxy_target (see seedSampleQuery in main.go) — assert it renders.
    const firstRow = rows.first();
    await expect(firstRow.getByText("admin")).toBeVisible();
    await expect(firstRow.getByText("proxy_target")).toBeVisible();

    // The connection cell is a link scoped to that connection's queries,
    // independently clickable from the row's own link to the query detail.
    const connectionLink = firstRow.locator("a").last();
    const href = await connectionLink.getAttribute("href");
    expect(href).toMatch(/\/queries\?connection_id=/);

    await connectionLink.click();
    await expect(authenticatedPage).toHaveURL(/connection_id=/);
    await authenticatedPage.waitForLoadState("networkidle");

    // Filter badge confirms the connection-scoped view is active.
    await expect(
      authenticatedPage.getByText("Filtered by connection"),
    ).toBeVisible();
  });

  test("query detail breadcrumb shows Queries parent, not 'Details'", async ({
    authenticatedPage,
  }) => {
    // Navigate straight to a detail route with a synthetic uid. Even with no
    // matching query, the breadcrumb is built from the pathname, so it must
    // still show the "Queries" parent crumb and must not collapse the detail
    // segment to the literal "Details".
    await authenticatedPage.goto(
      "queries/00000000-0000-0000-0000-000000000000",
    );
    await authenticatedPage.waitForLoadState("networkidle");

    const breadcrumb = authenticatedPage.locator(
      'nav[aria-label="breadcrumb"]',
    );
    await expect(breadcrumb).toBeVisible();

    // Parent "Queries" crumb links back to the list.
    const queriesCrumb = breadcrumb.getByRole("link", { name: "Queries" });
    await expect(queriesCrumb).toBeVisible();
    await expect(queriesCrumb).toHaveAttribute("href", /\/queries$/);

    // The old behaviour rendered every detail page's crumb as "Details".
    await expect(breadcrumb).not.toContainText("Details");
  });

  test("query detail breadcrumb shows a SQL preview when a query exists", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    // A real query row is a table row containing a navigation link. The empty
    // state renders a single colSpan row with no <a>, so guard on the link
    // count (not the row count) to avoid hanging when there are no queries.
    const rowLinks = authenticatedPage.locator("tbody tr td a");
    if ((await rowLinks.count()) === 0) {
      test.skip(true, "No query rows available in this environment");
      return;
    }

    // Click the first query row to open its detail page.
    await rowLinks.first().click();
    await expect(authenticatedPage).toHaveURL(/\/queries\/[0-9a-f-]{36}$/);
    await authenticatedPage.waitForLoadState("networkidle");

    const breadcrumb = authenticatedPage.locator(
      'nav[aria-label="breadcrumb"]',
    );
    // "Queries" parent + a non-empty preview leaf (the SQL text, not "Details").
    await expect(
      breadcrumb.getByRole("link", { name: "Queries" }),
    ).toBeVisible();
    await expect(breadcrumb).not.toContainText("Details");
    // The leaf (current-page) crumb carries the SQL preview.
    const leaf = breadcrumb.locator('[aria-current="page"]');
    await expect(leaf).toBeVisible();
    await expect(leaf).not.toHaveText("");
  });

  test("query detail breadcrumb shows the owning connection and links to its detail page", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    const rowLinks = authenticatedPage.locator("tbody tr td a");
    if ((await rowLinks.count()) === 0) {
      test.skip(true, "No query rows available in this environment");
      return;
    }

    // Grab the connection id from the list row's own connection link so we
    // can assert the breadcrumb crumb matches it exactly.
    const firstRow = authenticatedPage.locator("tbody tr").first();
    const listConnectionHref = await firstRow
      .locator("a")
      .last()
      .getAttribute("href");
    const connectionId = listConnectionHref?.match(
      /connection_id=([^&]+)/,
    )?.[1];
    expect(connectionId).toBeTruthy();

    await rowLinks.first().click();
    await expect(authenticatedPage).toHaveURL(/\/queries\/[0-9a-f-]{36}$/);
    await authenticatedPage.waitForLoadState("networkidle");

    const breadcrumb = authenticatedPage.locator(
      'nav[aria-label="breadcrumb"]',
    );
    await expect(breadcrumb).toBeVisible();

    // A connection crumb sits between "Queries" and the SQL-preview leaf,
    // linking to the connection's own detail page (not the filtered list).
    const connectionCrumb = breadcrumb.locator(
      `a[href="/app/connections/${connectionId}"]`,
    );
    await expect(connectionCrumb).toBeVisible();

    // Once the connection has been fetched, its label upgrades from the
    // short UID to the resolved "username @ database" form.
    await expect(connectionCrumb).toHaveText(/admin @ proxy_target/);

    await connectionCrumb.click();
    await expect(authenticatedPage).toHaveURL(
      new RegExp(`/connections/${connectionId}$`),
    );
    await authenticatedPage.waitForLoadState("networkidle");

    // The connection detail page's own breadcrumb reads "Connections › <label>".
    const detailBreadcrumb = authenticatedPage.locator(
      'nav[aria-label="breadcrumb"]',
    );
    await expect(
      detailBreadcrumb.getByRole("link", { name: "Connections" }),
    ).toBeVisible();
    const detailLeaf = detailBreadcrumb.locator('[aria-current="page"]');
    await expect(detailLeaf).toHaveText(/admin @ proxy_target/);

    // The metadata card is rendered with the connection's details.
    await expect(
      authenticatedPage.getByText("Connection Information"),
    ).toBeVisible();
    await expect(authenticatedPage.getByText("Source IP")).toBeVisible();
  });

  test("connections list links to the connection detail page", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connection rows available in this environment");
      return;
    }

    const rowLink = rows.first().locator("a").first();
    const href = await rowLink.getAttribute("href");
    expect(href).toMatch(/^\/app\/connections\/[0-9a-f-]{36}$/);

    await rowLink.click();
    await expect(authenticatedPage).toHaveURL(/\/connections\/[0-9a-f-]{36}$/);
    await authenticatedPage.waitForLoadState("networkidle");
    await expect(
      authenticatedPage.getByText("Connection Information"),
    ).toBeVisible();
  });

  test("Active only toggle updates the table without a document reload", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // A hard navigation re-creates the document (and its globals). Stamp a
    // marker on `window` so we can prove the SPA never reloaded.
    await authenticatedPage.evaluate(() => {
      (window as unknown as { __noReloadMarker?: boolean }).__noReloadMarker =
        true;
    });

    const toggle = authenticatedPage.locator("#showActive");
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute("data-state", "unchecked");

    await toggle.click();

    // The URL search param updates client-side...
    await expect(authenticatedPage).toHaveURL(/[?&]active=true/);
    await expect(toggle).toHaveAttribute("data-state", "checked");

    // ...and the marker survives, proving no document reload happened.
    const markerAfterOn = await authenticatedPage.evaluate(
      () =>
        (window as unknown as { __noReloadMarker?: boolean })
          .__noReloadMarker,
    );
    expect(markerAfterOn).toBe(true);

    // Toggling back off removes the param entirely (matching the pre-router
    // behavior) rather than leaving `active=false` in the URL.
    await toggle.click();
    await expect(authenticatedPage).not.toHaveURL(/[?&]active=/);
    await expect(toggle).toHaveAttribute("data-state", "unchecked");

    const markerAfterOff = await authenticatedPage.evaluate(
      () =>
        (window as unknown as { __noReloadMarker?: boolean })
          .__noReloadMarker,
    );
    expect(markerAfterOff).toBe(true);
  });

  test("pagination and page-size links carry the active filter through", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections?active=true");
    await authenticatedPage.waitForLoadState("networkidle");

    const toggle = authenticatedPage.locator("#showActive");
    await expect(toggle).toHaveAttribute("data-state", "checked");

    // Page-size links (25/50/100) must keep active=true.
    const pageSizeLink = authenticatedPage.getByRole("link", { name: "25" });
    await expect(pageSizeLink).toHaveAttribute(
      "href",
      /[?&]active=true/,
    );

    // The "Older" pagination link, when present, must also keep active=true.
    const olderLink = authenticatedPage.getByRole("link", { name: /Older/ });
    if (await olderLink.count()) {
      await expect(olderLink).toHaveAttribute("href", /[?&]active=true/);
    }
  });

  // upstream_tls records an outcome, not a policy: under the opportunistic
  // ssl_mode values dbbat offers TLS and silently falls back to plaintext when
  // the target refuses. Test mode seeds proxy_target with ssl_mode=disable and
  // a sample connection that never negotiated TLS, so every row here is the
  // plaintext case — which is the one the UI must make visible.
  test("connections list flags a plaintext upstream leg", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const headerRow = authenticatedPage.locator("thead tr");
    await expect(headerRow.getByText("Upstream", { exact: true })).toBeVisible();

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connection rows available in this environment");
      return;
    }

    const indicator = rows
      .first()
      .getByTestId("upstream-tls-indicator");
    await expect(indicator).toBeVisible();
    // Admin sees the target's protocol (postgresql), so the plaintext leg is
    // attributable and gets the loud state rather than the muted fallback.
    await expect(indicator).toHaveAttribute("data-state", "plaintext");
    await expect(indicator).toHaveText("Plaintext");
  });

  test("connection detail pairs the TLS outcome with the ssl_mode policy", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connection rows available in this environment");
      return;
    }

    await rows.first().locator("a").first().click();
    await expect(authenticatedPage).toHaveURL(/\/connections\/[0-9a-f-]{36}$/);
    await authenticatedPage.waitForLoadState("networkidle");

    await expect(
      authenticatedPage.getByText("Upstream Encryption"),
    ).toBeVisible();

    const indicator = authenticatedPage.getByTestId("upstream-tls-indicator");
    await expect(indicator).toHaveAttribute("data-state", "plaintext");

    // The policy sits next to the outcome so the two read as one statement.
    // proxy_target is seeded with ssl_mode=disable.
    await expect(
      authenticatedPage.getByTestId("upstream-tls-policy"),
    ).toContainText("disable");
  });

  // Test mode's seeded sample connection (seedSampleQuery in main.go) is
  // stamped with the read-only, quota-bounded grant seedQuotaGrant creates
  // for the admin user just before it — the same grant_uid a real proxy
  // session would carry after authenticating under it.
  test("connection detail shows the grant it ran under", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connection rows available in this environment");
      return;
    }

    // The list itself carries a compact access chip derived from the same
    // stamp, before navigating into the detail page.
    await expect(
      rows.first().getByTestId("grant-access-chip"),
    ).toHaveText("R/O");

    await rows.first().locator("a").first().click();
    await expect(authenticatedPage).toHaveURL(/\/connections\/[0-9a-f-]{36}$/);
    await authenticatedPage.waitForLoadState("networkidle");

    await expect(authenticatedPage.getByText("Grant", { exact: true })).toBeVisible();

    const summary = authenticatedPage.getByTestId("connection-grant-summary");
    await expect(summary).toBeVisible();
    await expect(summary.getByText("Read Only")).toBeVisible();

    // seedQuotaGrant sets no explicit priority, so it auto-computes to the
    // read_only tier (store.PriorityReadOnly = 10).
    await expect(
      authenticatedPage.getByTestId("connection-grant-priority"),
    ).toHaveText("10");

    await expect(
      authenticatedPage.getByTestId("connection-grant-link"),
    ).toHaveAttribute("href", "/app/grants");
  });

  test("viewer sees the plaintext leg without an unattributable warning", async ({
    authenticatedPage,
    browser,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connection rows available in this environment");
      return;
    }

    const href = await rows.first().locator("a").first().getAttribute("href");
    const connectionId = href?.match(/connections\/([0-9a-f-]{36})/)?.[1];
    expect(connectionId).toBeTruthy();

    const viewerContext = await browser.newContext();
    const page = await viewerContext.newPage();
    await page.goto("/");
    await page.waitForLoadState("domcontentloaded");
    await page.getByTestId("login-logo").waitFor({ state: "visible" });
    await page.getByTestId("login-username").fill("viewer");
    await page.getByTestId("login-password").fill("viewer");
    await page.getByTestId("login-submit").click();
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    await page.goto(`connections/${connectionId}`);
    await page.waitForLoadState("networkidle");
    await expect(page.getByText("Connection Information")).toBeVisible();

    // A viewer only gets uid/name/description for a server, so the UI cannot
    // tell a genuine downgrade from an Oracle session (always plaintext by
    // design). It reports the honest outcome without the warning styling, and
    // without a policy it has no business knowing.
    const indicator = page.getByTestId("upstream-tls-indicator");
    await expect(indicator).toHaveAttribute(
      "data-state",
      "plaintext-unattributed",
    );
    await expect(page.getByTestId("upstream-tls-policy")).toHaveCount(0);

    await viewerContext.close();
  });

  test("a connector cannot open another user's connection page", async ({
    authenticatedPage,
    browser,
  }) => {
    // Test mode seeds one query/connection executed by the admin user
    // (seedSampleQuery in main.go). Grab its connection id as the admin.
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    const rowLinks = authenticatedPage.locator("tbody tr td a");
    if ((await rowLinks.count()) === 0) {
      test.skip(true, "No query rows available in this environment");
      return;
    }

    const firstRow = authenticatedPage.locator("tbody tr").first();
    const listConnectionHref = await firstRow
      .locator("a")
      .last()
      .getAttribute("href");
    const connectionId = listConnectionHref?.match(
      /connection_id=([^&]+)/,
    )?.[1];
    expect(connectionId).toBeTruthy();

    // Log in as the connector user (a distinct, non-owning user) and try to
    // open the admin's connection directly. It must be reported as not
    // found — connectors must not be able to distinguish "doesn't exist"
    // from "exists but isn't mine". This needs a genuinely separate browser
    // context: `authenticatedPage`'s fixture is built on top of the base
    // `page` fixture and hands back that same Page, so reusing `page` here
    // would still be signed in as admin and never show the login form.
    const connectorContext = await browser.newContext();
    const page = await connectorContext.newPage();
    await page.goto("/");
    await page.waitForLoadState("domcontentloaded");
    await page.getByTestId("login-logo").waitFor({ state: "visible" });
    await page.getByTestId("login-username").fill("connector");
    await page.getByTestId("login-password").fill("connector");
    await page.getByTestId("login-submit").click();
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    await page.goto(`connections/${connectionId}`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByText("Connection not found")).toBeVisible();
    await connectorContext.close();
  });

  // Capture download: the E2E harness (front/e2e/global-setup.ts) starts the
  // server without DBB_DUMP_DIR, so captures are disabled server-wide and no
  // connection in this environment ever reports dump.available. That is
  // enough to exercise both "no button without a capture" and "no button for
  // a non-admin" — it can't exercise "button visible + successful download",
  // which needs an on-disk capture the harness doesn't produce. That
  // happy-path assertion is intentionally not written here rather than
  // faked; see specs/todos/2026-08-05-03-download-session-capture-from-ui.md.
  test("download capture button is absent when the connection has no capture", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connection rows available in this environment");
      return;
    }

    const rowLink = rows.first().locator("a").first();
    await rowLink.click();
    await expect(authenticatedPage).toHaveURL(/\/connections\/[0-9a-f-]{36}$/);
    await authenticatedPage.waitForLoadState("networkidle");
    await expect(
      authenticatedPage.getByText("Connection Information"),
    ).toBeVisible();

    // Dumps are disabled in this harness, so dump.available is false even
    // for an admin — the button must not render.
    await expect(
      authenticatedPage.getByTestId("download-dump-button"),
    ).toHaveCount(0);
  });

  test("download capture button is absent for the viewer account", async ({
    authenticatedPage,
    browser,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connection rows available in this environment");
      return;
    }

    const href = await rows.first().locator("a").first().getAttribute("href");
    const connectionId = href?.match(/connections\/([0-9a-f-]{36})/)?.[1];
    expect(connectionId).toBeTruthy();

    // A genuinely separate browser context, signed in as viewer: reusing
    // authenticatedPage's Page would stay signed in as admin.
    const viewerContext = await browser.newContext();
    const page = await viewerContext.newPage();
    await page.goto("/");
    await page.waitForLoadState("domcontentloaded");
    await page.getByTestId("login-logo").waitFor({ state: "visible" });
    await page.getByTestId("login-username").fill("viewer");
    await page.getByTestId("login-password").fill("viewer");
    await page.getByTestId("login-submit").click();
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    await page.goto(`connections/${connectionId}`);
    await page.waitForLoadState("networkidle");
    await expect(page.getByText("Connection Information")).toBeVisible();

    // Viewer role must never see the download action, regardless of whether
    // a capture exists — the UI gate mirrors the API's admin-only narrowing.
    await expect(page.getByTestId("download-dump-button")).toHaveCount(0);

    await viewerContext.close();
  });

  test("should display audit log page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/audit-page.png",
      fullPage: true,
    });

    // Verify we're on the audit page
    await expect(authenticatedPage).toHaveURL(/\/audit/);

    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("should verify the audit chains from the audit page", async ({
    authenticatedPage,
  }) => {
    // The store-wide audit chain only reports a head once something has been
    // audited, and a freshly provisioned test store has nothing in
    // `audit_log`. Create and immediately revoke an API key: both are audited,
    // and the key never outlives the test.
    const apiBase = new URL(authenticatedPage.url()).origin + "/api/v1";
    const auth =
      "Basic " + Buffer.from("admin:admintest").toString("base64");
    const created = await authenticatedPage.request.post(`${apiBase}/keys`, {
      headers: { Authorization: auth },
      data: { name: `e2e-chain-verification-${Date.now()}` },
    });
    expect(created.ok()).toBeTruthy();
    const createdKey = (await created.json()) as { id: string };
    const revoked = await authenticatedPage.request.delete(
      `${apiBase}/keys/${createdKey.id}`,
      { headers: { Authorization: auth } },
    );
    expect(revoked.ok()).toBeTruthy();

    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");

    const card = authenticatedPage.getByTestId("chain-verification-card");
    await expect(card).toBeVisible();

    // The caveat is the point of the panel as much as the verdict is: the
    // answer comes from the server being audited, and the CLI is what someone
    // who does not trust it runs.
    await expect(
      authenticatedPage.getByTestId("chain-verification-caveat"),
    ).toContainText("dbbat audit verify");
    await expect(
      authenticatedPage.getByTestId("chain-verification-docs-link"),
    ).toHaveAttribute("href", /audit-chain/);

    // The store-wide audit chain reports a head, and the head is copyable —
    // recording it outside the database is the ritual this panel exists for.
    // A walk's outcome is cached for a minute, so an answer computed before
    // the key above was created can legitimately come back headless: click
    // again until the cache has turned over.
    const headMac = authenticatedPage.getByTestId("audit-head-mac");
    await expect(async () => {
      await authenticatedPage.getByTestId("verify-chains-button").click();
      await expect(headMac).toBeVisible({ timeout: 10000 });
    }).toPass({ timeout: 120000, intervals: [5000] });
    await expect(headMac).toHaveText(/^[0-9a-f]{16,}$/);
    await expect(
      authenticatedPage.getByTestId("audit-head-mac-copy"),
    ).toBeEnabled();

    // All three chains are walked: audit_log, the per-connection query chains
    // and the per-capture result-row chains.
    for (const chain of ["audit", "queries", "rows"]) {
      const row = authenticatedPage.getByTestId(`chain-result-${chain}`);
      await expect(row).toBeVisible();
      await expect(row).toHaveAttribute("data-verified", "true", {
        timeout: 30000,
      });
      await expect(
        authenticatedPage.getByTestId(`chain-result-${chain}-status`),
      ).toHaveText(/Verified/);
      // Cached or fresh, the panel says which — it never presents a
      // remembered walk as one run for this request.
      await expect(
        authenticatedPage.getByTestId(`chain-result-${chain}-freshness`),
      ).toHaveText(/Walked for this request|Cached result/);
      // No chain is broken, so no break block should be rendered.
      await expect(
        authenticatedPage.getByTestId(`chain-result-${chain}-break`),
      ).toHaveCount(0);
    }

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/audit-chain-verification.png",
      fullPage: true,
    });
  });

  test("should hide chain verification from a viewer", async ({
    authenticatedPage,
    browser,
  }) => {
    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");
    await expect(
      authenticatedPage.getByTestId("chain-verification-card"),
    ).toBeVisible();

    // A genuinely separate browser context, signed in as viewer: reusing
    // authenticatedPage's Page would stay signed in as admin.
    const viewerContext = await browser.newContext();
    const page = await viewerContext.newPage();
    await page.goto("/");
    await page.waitForLoadState("domcontentloaded");
    await page.getByTestId("login-logo").waitFor({ state: "visible" });
    await page.getByTestId("login-username").fill("viewer");
    await page.getByTestId("login-password").fill("viewer");
    await page.getByTestId("login-submit").click();
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    await page.goto("audit");
    await page.waitForLoadState("networkidle");

    // A viewer may read the audit list but the verify endpoints answer 403,
    // so the panel must not be offered at all.
    await expect(page).toHaveURL(/\/audit/);
    await expect(page.getByTestId("chain-verification-card")).toHaveCount(0);
    await expect(page.getByTestId("verify-chains-button")).toHaveCount(0);

    await viewerContext.close();
  });

  test("should navigate to queries page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("load");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/queries-navigation.png",
      fullPage: true,
    });

    // Verify we're on the queries page
    await expect(authenticatedPage).toHaveURL(/\/queries/);
  });

  test("should refresh connections without getting stuck", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Find the Refresh button
    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    // Verify button is enabled initially
    await expect(refreshButton).toBeEnabled();

    // Click the refresh button and wait for it to complete
    await refreshButton.click();
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    // Verify icon is not spinning anymore
    const refreshIcon = refreshButton.locator("svg");
    const iconClasses = await refreshIcon.getAttribute("class");
    expect(iconClasses).not.toContain("animate-spin");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/connections-refresh-complete.png",
      fullPage: true,
    });
  });

  test("should refresh queries without getting stuck", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    await expect(refreshButton).toBeEnabled();
    await refreshButton.click();
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    const refreshIcon = refreshButton.locator("svg");
    const iconClasses = await refreshIcon.getAttribute("class");
    expect(iconClasses).not.toContain("animate-spin");
  });

  test("should refresh audit log without getting stuck", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    await expect(refreshButton).toBeEnabled();
    await refreshButton.click();
    // Note: We don't check for disabled state here as the API call may complete
    // too quickly for the disabled state to be caught reliably
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    const refreshIcon = refreshButton.locator("svg");
    const iconClasses = await refreshIcon.getAttribute("class");
    expect(iconClasses).not.toContain("animate-spin");
  });
});

test.describe("Adaptive Auto-Refresh Feature", () => {
  test("should toggle auto-refresh on and off", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Find the auto-refresh badge
    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );
    await expect(autoRefreshBadge).toBeVisible();

    // Initially, auto-refresh should be enabled (showing countdown)
    let badgeText = await autoRefreshBadge.textContent();
    const initiallyEnabled = badgeText?.includes("Next:");

    // Click to toggle
    await autoRefreshBadge.click();

    // Wait a bit for state to update
    await authenticatedPage.waitForTimeout(100);

    // Verify it toggled
    badgeText = await autoRefreshBadge.textContent();
    if (initiallyEnabled) {
      expect(badgeText).toContain("Auto-refresh: OFF");
    } else {
      expect(badgeText).toMatch(/Next: \d+s/);
    }

    // Toggle back
    await autoRefreshBadge.click();
    await authenticatedPage.waitForTimeout(100);

    // Verify it toggled back
    badgeText = await autoRefreshBadge.textContent();
    if (initiallyEnabled) {
      expect(badgeText).toMatch(/Next: \d+s/);
    } else {
      expect(badgeText).toContain("Auto-refresh: OFF");
    }

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-toggle.png",
      fullPage: true,
    });
  });

  test("should display countdown timer when auto-refresh is enabled", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Find the auto-refresh badge
    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );
    await expect(autoRefreshBadge).toBeVisible();

    // Make sure auto-refresh is enabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Verify countdown format
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toMatch(/Next: \d+s/);

    // Extract the current countdown value
    const match = badgeText?.match(/Next: (\d+)s/);
    expect(match).toBeTruthy();
    const initialSeconds = parseInt(match![1]);

    // Wait 2 seconds and verify countdown decreased
    await authenticatedPage.waitForTimeout(2000);
    badgeText = await autoRefreshBadge.textContent();
    const match2 = badgeText?.match(/Next: (\d+)s/);
    expect(match2).toBeTruthy();
    const newSeconds = parseInt(match2![1]);

    // Countdown should have decreased (allowing for some timing variance)
    expect(newSeconds).toBeLessThan(initialSeconds);

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-countdown.png",
      fullPage: true,
    });
  });

  test("should trigger refresh when countdown reaches 0", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Find the auto-refresh badge
    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );
    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    // Make sure auto-refresh is enabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Manually trigger a refresh to reset the timer to a known state
    await refreshButton.click();
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    // Now wait for countdown to reach a low value (checking every second)
    let countdownReachedLow = false;
    for (let i = 0; i < 15; i++) {
      badgeText = await autoRefreshBadge.textContent();
      const match = badgeText?.match(/Next: (\d+)s/);
      if (match) {
        const seconds = parseInt(match[1]);
        if (seconds <= 2) {
          countdownReachedLow = true;
          break;
        }
      }
      await authenticatedPage.waitForTimeout(1000);
    }

    expect(countdownReachedLow).toBeTruthy();

    // Wait for the auto-refresh to trigger (button should become disabled then enabled)
    // The refresh should trigger when countdown hits 0
    await authenticatedPage.waitForTimeout(3000);

    // After auto-refresh, countdown should have reset
    badgeText = await autoRefreshBadge.textContent();
    const matchAfter = badgeText?.match(/Next: (\d+)s/);
    if (matchAfter) {
      const secondsAfter = parseInt(matchAfter[1]);
      // Should be close to the initial interval (10s by default)
      expect(secondsAfter).toBeGreaterThan(2);
    }

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-triggered.png",
      fullPage: true,
    });
  });

  test("should reset interval when manually refreshing", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );
    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });

    // Make sure auto-refresh is enabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Wait for countdown to decrease
    await authenticatedPage.waitForTimeout(3000);

    // Get current countdown
    badgeText = await autoRefreshBadge.textContent();
    let match = badgeText?.match(/Next: (\d+)s/);
    expect(match).toBeTruthy();
    const secondsBeforeRefresh = parseInt(match![1]);

    // Manual refresh
    await refreshButton.click();
    await expect(refreshButton).toBeEnabled({ timeout: 15000 });

    // After manual refresh, countdown should reset to initial interval
    badgeText = await autoRefreshBadge.textContent();
    match = badgeText?.match(/Next: (\d+)s/);
    expect(match).toBeTruthy();
    const secondsAfterRefresh = parseInt(match![1]);

    // Countdown should have reset to a higher value (initial interval is 10s by default)
    expect(secondsAfterRefresh).toBeGreaterThan(secondsBeforeRefresh);
    expect(secondsAfterRefresh).toBeGreaterThanOrEqual(8); // Should be close to 10s

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-manual-reset.png",
      fullPage: true,
    });
  });

  test("should persist auto-refresh state in localStorage", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );

    // Make sure auto-refresh is enabled first
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Verify it's enabled
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toMatch(/Next: \d+s/);

    // Check localStorage
    let storedValue = await authenticatedPage.evaluate(() => {
      return localStorage.getItem("dbbat.autoRefresh.connections");
    });
    expect(storedValue).toBeTruthy();
    let parsed = JSON.parse(storedValue!);
    expect(parsed.enabled).toBe(true);

    // Toggle off
    await autoRefreshBadge.click();
    await authenticatedPage.waitForTimeout(100);

    // Verify badge shows OFF
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toContain("Auto-refresh: OFF");

    // Check localStorage was updated
    storedValue = await authenticatedPage.evaluate(() => {
      return localStorage.getItem("dbbat.autoRefresh.connections");
    });
    parsed = JSON.parse(storedValue!);
    expect(parsed.enabled).toBe(false);

    // Navigate away and back to test persistence
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    // Auto-refresh should still be off after navigation
    const autoRefreshBadgeAfterNav = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );
    const badgeTextAfterNav = await autoRefreshBadgeAfterNav.textContent();
    expect(badgeTextAfterNav).toContain("Auto-refresh: OFF");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-persistence.png",
      fullPage: true,
    });
  });

  test("should show 'Auto-refresh: OFF' when disabled", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );

    // Make sure auto-refresh is disabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Next:")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Verify text shows "Auto-refresh: OFF"
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toBe("Auto-refresh: OFF");

    // Wait and verify text doesn't change (no countdown when disabled)
    await authenticatedPage.waitForTimeout(2000);
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toBe("Auto-refresh: OFF");
  });

  test("should work on queries page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("queries");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );
    await expect(autoRefreshBadge).toBeVisible();

    // Toggle
    await autoRefreshBadge.click();
    await authenticatedPage.waitForTimeout(100);

    const badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toBeTruthy();

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-queries.png",
      fullPage: true,
    });
  });

  test("should work on audit page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");

    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );
    await expect(autoRefreshBadge).toBeVisible();

    // Toggle
    await autoRefreshBadge.click();
    await authenticatedPage.waitForTimeout(100);

    const badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toBeTruthy();

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-audit.png",
      fullPage: true,
    });
  });

  test("should handle multiple manual refresh clicks properly", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });
    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );

    // Verify button is enabled initially
    await expect(refreshButton).toBeEnabled();

    // Click refresh multiple times in succession
    for (let i = 0; i < 3; i++) {
      // Click the refresh button and wait for it to complete
      await refreshButton.click();
      await expect(refreshButton).toBeEnabled({ timeout: 5000 });

      // Verify icon is not spinning after completion
      const refreshIcon = refreshButton.locator("svg");
      const iconClasses = await refreshIcon.getAttribute("class");
      expect(iconClasses).not.toContain("animate-spin");

      // Verify countdown reset (if auto-refresh is enabled)
      const badgeText = await autoRefreshBadge.textContent();
      if (badgeText?.includes("Next:")) {
        const match = badgeText.match(/Next: (\d+)s/);
        expect(match).toBeTruthy();
        const seconds = parseInt(match![1]);
        expect(seconds).toBeGreaterThanOrEqual(8); // Should be close to initial 10s
      }

      // Small delay between clicks
      await authenticatedPage.waitForTimeout(500);
    }

    // Take screenshot after multiple refreshes
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-multiple-clicks.png",
      fullPage: true,
    });
  });

  test("should refresh button work with auto-refresh enabled", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const refreshButton = authenticatedPage.getByRole("button", {
      name: "Refresh",
    });
    const autoRefreshBadge = authenticatedPage.getByText(
      /Next:|Auto-refresh: OFF/,
    );

    // Ensure auto-refresh is enabled
    let badgeText = await autoRefreshBadge.textContent();
    if (badgeText?.includes("Auto-refresh: OFF")) {
      await autoRefreshBadge.click();
      await authenticatedPage.waitForTimeout(100);
    }

    // Verify countdown is running
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toMatch(/Next: \d+s/);

    // Manual refresh should still work
    await refreshButton.click();
    // Wait for refresh to complete (button re-enabled)
    await expect(refreshButton).toBeEnabled({ timeout: 5000 });

    // Auto-refresh should still be enabled after manual refresh
    badgeText = await autoRefreshBadge.textContent();
    expect(badgeText).toMatch(/Next: \d+s/);

    // Countdown should have reset to initial value
    const match = badgeText?.match(/Next: (\d+)s/);
    expect(match).toBeTruthy();
    const seconds = parseInt(match![1]);
    expect(seconds).toBeGreaterThanOrEqual(8);

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/auto-refresh-button-with-auto.png",
      fullPage: true,
    });
  });
});
