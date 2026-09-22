import { test, expect } from "./fixtures";

// Regression guard for the HTML `pattern=` attribute going silently dead:
// browsers compile `pattern=` under the `v` (unicodeSets) regex flag, where
// an unescaped `-` in a class position like `[a-z0-9_-]` is a compile error
// — and per the HTML spec, a `pattern` that fails to compile is silently
// IGNORED rather than rejected, so client-side validation disappears with no
// visible error (checkValidity() just returns true for everything). There is
// no vitest/unit-test runner wired up under front/src, so this guard lives
// here instead. Keep this string identical to SERVER_NAME_PATTERN in
// front/src/routes/_authenticated/servers/index.tsx, serverNamePattern in
// internal/store/servers.go, and the `pattern:` fields in
// internal/api/openapi.yml.
const SERVER_NAME_PATTERN_SOURCE =
  "^[a-z0-9_][a-z0-9_\\-]{0,61}[a-z0-9_]$|^[a-z0-9_]$";

test.describe("Server name pattern — v-flag regression guard", () => {
  test("the HTML pattern= source compiles under the v (unicodeSets) regex flag", () => {
    // This is exactly what a Chromium-family browser does when it parses a
    // <input pattern="..."> attribute. Reverting the `\-` escape reproduces
    // the original bug: this assertion throws (SyntaxError: Invalid character
    // in character class).
    expect(() => new RegExp(SERVER_NAME_PATTERN_SOURCE, "v")).not.toThrow();
  });

  test("the v-flag-compiled pattern accepts/rejects the same names as before the escape", () => {
    const re = new RegExp(`^(?:${SERVER_NAME_PATTERN_SOURCE})$`, "v");
    expect(re.test("prod-eu-1")).toBe(true);
    expect(re.test("a")).toBe(true);
    expect(re.test("production_db")).toBe(true);
    expect(re.test("-bad")).toBe(false);
    expect(re.test("trail-")).toBe(false);
    expect(re.test("prod.eu.1")).toBe(false);
    expect(re.test("--")).toBe(false);
  });
});

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
      expect(formContent?.toLowerCase()).toMatch(
        /host|port|database|name|connection/,
      );
    }
  });

  test("create dialog rejects a leading-hyphen name before it reaches the server", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    await authenticatedPage.getByTestId("add-database-button").click();

    const nameInput = authenticatedPage.getByTestId("database-name-input");
    await nameInput.fill("-bad-name");
    await authenticatedPage.locator("#host").fill("db.example.com");
    await authenticatedPage.locator("#username").fill("postgres");
    await authenticatedPage.locator("#password").fill("secret");

    // The server name is a slug (see store.ErrServerNameInvalid) — an
    // interior hyphen is fine (e.g. "prod-eu-1"), but it may not lead or
    // trail. The input's native HTML5 pattern must catch that before any
    // request is made.
    const isValid = await nameInput.evaluate((el: HTMLInputElement) =>
      el.checkValidity(),
    );
    expect(isValid).toBe(false);

    await authenticatedPage.getByTestId("database-create-submit").click();

    // The browser blocks the submit, so the dialog stays open with the
    // rejected value still in the field rather than a round-trip 400.
    await expect(nameInput).toBeVisible();
    await expect(nameInput).toHaveValue("-bad-name");
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

      // The URI protocols select the dbbat entry from the username, as
      // `user%23entry`, leaving the path free to carry the real upstream
      // database name — the form DataGrip and DBeaver can sustain across their
      // per-database reconnects. Oracle and MongoDB carry the selector
      // elsewhere, so only assert it on the URI shapes.
      const value = await connUrl.first().inputValue();
      if (value.startsWith("postgresql://") || value.startsWith("mysql://")) {
        expect(value).toContain("%23");
      }
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

    const name = `e2e_bastion_${Date.now()}`;

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

    const name = `e2e_bastion_edit_${Date.now()}`;
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

  test("a Kubernetes cluster can be created without pasting a CA bundle", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const sshSection = authenticatedPage.getByTestId("ssh-servers-section");
    await expect(sshSection).toBeVisible();

    const name = `e2e_cluster_${Date.now()}`;

    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("protocol-select").click();
    await authenticatedPage.getByTestId("protocol-option-kubernetes").click();

    await authenticatedPage.getByTestId("database-name-input").fill(name);
    await authenticatedPage.locator("#host").fill("api.cluster.example.com");
    await authenticatedPage.locator("#port").fill("6443");
    await authenticatedPage.locator("#username").fill("dbbat");
    await authenticatedPage.locator("#password").fill("sa-token");
    await authenticatedPage.getByTestId("k8s-namespace-input").fill("data");

    // The CA textarea is deliberately left empty: the row takes a
    // trust-on-first-use pin instead, so the form must not block on it.
    const caInput = authenticatedPage.getByTestId("k8s-ca-cert-input");
    await expect(caInput).toBeVisible();
    await expect(caInput).not.toHaveAttribute("required", /.*/);

    await authenticatedPage.getByTestId("database-create-submit").click();

    const row = sshSection.locator("tr", { hasText: name });
    await expect(row).toBeVisible({ timeout: 10000 });

    // Nothing is pinned yet, and this is *not* the insecure escape hatch.
    await expect(
      row.locator('[data-testid^="tunnel-insecure-badge-"]'),
    ).toHaveCount(0);
    await expect(
      row.locator('[data-testid^="tunnel-ca-pinned-badge-"]'),
    ).toHaveCount(0);
  });

  test("testing an unreachable SSH bastion reports the failing stage", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const sshSection = authenticatedPage.getByTestId("ssh-servers-section");
    await expect(sshSection).toBeVisible();

    const name = `e2e_bastion_test_${Date.now()}`;

    // 127.0.0.1:1 has nothing listening: the check must fail loudly rather than
    // report the write-and-hope success this feature exists to eliminate.
    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("protocol-select").click();
    await authenticatedPage.getByTestId("protocol-option-ssh").click();
    await authenticatedPage.getByTestId("database-name-input").fill(name);
    await authenticatedPage.locator("#host").fill("127.0.0.1");
    await authenticatedPage.locator("#port").fill("1");
    await authenticatedPage.locator("#username").fill("bastion-user");
    await authenticatedPage.locator("#password").fill("bastion-password");
    await authenticatedPage.getByTestId("database-create-submit").click();

    const row = sshSection.locator("tr", { hasText: name });
    await expect(row).toBeVisible({ timeout: 10000 });

    const testButton = row.locator('[data-testid^="ssh-server-test-"]');
    await expect(testButton).toBeVisible();

    const [response] = await Promise.all([
      authenticatedPage.waitForResponse((r) =>
        /\/api\/v1\/servers\/[^/]+\/test$/.test(r.url()),
      ),
      testButton.click(),
    ]);

    expect(response.status()).toBe(200);

    const body = await response.json();
    expect(body.ok).toBe(false);
    expect(["bastion_dial", "config"]).toContain(body.stage);
    expect(typeof body.message).toBe("string");
    // The stored credentials must never come back out.
    expect(JSON.stringify(body)).not.toContain("bastion-password");
  });

  test("two Oracle rows spelling one host two ways are flagged on the service name", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    // The reported incident, reproduced: one upstream service name claimed by
    // two rows that spell the host differently. Each row is fine on its own;
    // a client connecting with the shared service name is refused ORA-12514,
    // because the proxy compares candidate upstreams as text. The admin must
    // see that before a user hits it.
    const stamp = Date.now();
    const service = `E2EMUTU${stamp}`;
    const first = `e2e_oracle_cname_${stamp}`;
    const second = `e2e_oracle_arecord_${stamp}`;

    const createOracleRow = async (name: string, host: string) => {
      await authenticatedPage.getByTestId("add-database-button").click();
      await authenticatedPage.getByTestId("protocol-select").click();
      await authenticatedPage.getByTestId("protocol-option-oracle").click();
      await authenticatedPage.getByTestId("database-name-input").fill(name);
      await authenticatedPage.locator("#host").fill(host);
      await authenticatedPage.locator("#oracleServiceName").fill(service);
      await authenticatedPage.locator("#username").fill("system");
      await authenticatedPage.locator("#password").fill("oracle-password");
      await authenticatedPage.getByTestId("database-create-submit").click();
      await expect(
        authenticatedPage.locator("tr", { hasText: name }).first(),
      ).toBeVisible({ timeout: 10000 });
    };

    await createOracleRow(first, `oracle-${stamp}.db.example.com`);
    // Before the second row exists there is nothing to disagree with.
    const firstRow = authenticatedPage
      .locator("tr", { hasText: first })
      .first();
    await expect(
      firstRow.locator('[data-testid^="database-oracle-conflict-"]'),
    ).toHaveCount(0);

    await createOracleRow(second, `${stamp}.eu-west-3.rds.amazonaws.com`);

    // Both rows now carry the warning, and it names the conflict.
    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");

    for (const name of [first, second]) {
      const row = authenticatedPage.locator("tr", { hasText: name }).first();
      const marker = row
        .locator('[data-testid^="database-oracle-conflict-"]')
        .first();
      await expect(marker).toBeVisible({ timeout: 10000 });
      await expect(marker).toHaveAttribute("title", /ORA-12514/);
      await expect(marker).toHaveAttribute("title", new RegExp(service));
    }
  });

  test("the create dialog reopens clean, with nothing left over from the last one", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const stamp = Date.now();
    const name = `e2e_reopen_${stamp}`;

    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("database-name-input").fill(name);
    await authenticatedPage.locator("#host").fill("reopen.example.com");
    await authenticatedPage.locator("#databaseName").fill("postgres");
    await authenticatedPage.locator("#username").fill("reopen_user");
    await authenticatedPage.locator("#password").fill("reopen-secret");
    await authenticatedPage.getByTestId("database-create-submit").click();
    await expect(
      authenticatedPage.locator("tr", { hasText: name }).first(),
    ).toBeVisible({ timeout: 10000 });

    // Reopening immediately is the interesting moment: while the dialog was
    // left mounted, its overlay stayed in the DOM for the whole fade-out — a
    // `fixed inset-0 z-50` sheet over the trigger — so this click could be
    // swallowed and the retry would toggle the dialog straight back shut.
    await authenticatedPage.getByTestId("add-database-button").click();
    await expect(
      authenticatedPage.getByTestId("protocol-select"),
    ).toBeVisible();

    // And it comes back blank rather than pre-filled with the server just
    // created, password included.
    await expect(
      authenticatedPage.getByTestId("database-name-input"),
    ).toHaveValue("");
    await expect(authenticatedPage.locator("#host")).toHaveValue("");
    await expect(authenticatedPage.locator("#username")).toHaveValue("");
    await expect(authenticatedPage.locator("#password")).toHaveValue("");
  });

  test("a database row can be renamed, with the connection-target warning", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const stamp = Date.now();
    const before = `e2e_rename_before_${stamp}`;
    const after = `e2e_rename_after_${stamp}`;

    // A row to rename. Name is the only field that used to be immutable.
    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("database-name-input").fill(before);
    await authenticatedPage.locator("#host").fill("localhost");
    await authenticatedPage.locator("#databaseName").fill("postgres");
    await authenticatedPage.locator("#username").fill("postgres");
    await authenticatedPage.locator("#password").fill("postgres");
    await authenticatedPage.getByTestId("database-create-submit").click();

    const row = authenticatedPage.locator("tr", { hasText: before }).first();
    await expect(row).toBeVisible({ timeout: 10000 });

    await row.locator('[data-testid^="database-rename-"]').click();

    const dialog = authenticatedPage.getByTestId("database-rename-dialog");
    await expect(dialog).toBeVisible();

    // Untouched, there is nothing to warn about yet.
    await expect(
      authenticatedPage.getByTestId("server-rename-warning"),
    ).toHaveCount(0);

    const input = authenticatedPage.getByTestId("database-rename-input");
    await input.fill(after);

    // Changing it says what a rename actually breaks: the connection target.
    const warning = authenticatedPage.getByTestId("server-rename-warning");
    await expect(warning).toBeVisible();
    await expect(warning).toContainText(/connection target/i);

    await authenticatedPage.getByTestId("database-rename-submit").click();

    await expect(dialog).not.toBeVisible({ timeout: 10000 });
    await expect(
      authenticatedPage.locator("tr", { hasText: after }).first(),
    ).toBeVisible({ timeout: 10000 });
    await expect(
      authenticatedPage.locator("tr", { hasText: before }),
    ).toHaveCount(0);
  });

  test("the rename dialog rejects a leading-hyphen name before it reaches the server", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const stamp = Date.now();
    const name = `e2e_rename_slug_${stamp}`;

    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("database-name-input").fill(name);
    await authenticatedPage.locator("#host").fill("localhost");
    await authenticatedPage.locator("#databaseName").fill("postgres");
    await authenticatedPage.locator("#username").fill("postgres");
    await authenticatedPage.locator("#password").fill("postgres");
    await authenticatedPage.getByTestId("database-create-submit").click();

    const row = authenticatedPage.locator("tr", { hasText: name }).first();
    await expect(row).toBeVisible({ timeout: 10000 });

    await row.locator('[data-testid^="database-rename-"]').click();
    await expect(
      authenticatedPage.getByTestId("database-rename-dialog"),
    ).toBeVisible();

    const input = authenticatedPage.getByTestId("database-rename-input");
    // An interior hyphen (e.g. "name-renamed") is valid by design — see
    // specs/done/2026/09. A *leading* hyphen stays rejected (untypeable on
    // the command line: `psql -d -foo` parses as a flag), so it's still a
    // reachable negative case for this gate.
    await input.fill(`-${name}`);
    await authenticatedPage.getByTestId("database-rename-submit").click();

    // The slug gate the create dialog enforces applies to the rename too: the
    // form's own pattern refuses to submit, so the dialog stays open.
    const isValid = await input.evaluate((el: HTMLInputElement) =>
      el.checkValidity(),
    );
    expect(isValid).toBe(false);
    await expect(
      authenticatedPage.getByTestId("database-rename-dialog"),
    ).toBeVisible();
    await expect(
      authenticatedPage.locator("tr", { hasText: name }).first(),
    ).toBeVisible();
  });

  test("a tunnel row can be renamed from its edit dialog", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const sshSection = authenticatedPage.getByTestId("ssh-servers-section");
    await expect(sshSection).toBeVisible();

    const stamp = Date.now();
    const before = `e2e_bastion_rename_${stamp}`;
    const after = `e2e_bastion_renamed_${stamp}`;

    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("protocol-select").click();
    await authenticatedPage.getByTestId("protocol-option-ssh").click();
    await authenticatedPage.getByTestId("database-name-input").fill(before);
    await authenticatedPage.locator("#host").fill("bastion.example.com");
    await authenticatedPage.locator("#username").fill("bastion-user");
    await authenticatedPage.locator("#password").fill("bastion-password");
    await authenticatedPage.getByTestId("database-create-submit").click();

    const row = sshSection.locator("tr", { hasText: before });
    await expect(row).toBeVisible({ timeout: 10000 });

    await row.locator('[data-testid^="ssh-server-edit-"]').click();
    const editDialog = authenticatedPage.getByTestId("ssh-server-edit-dialog");
    await expect(editDialog).toBeVisible();

    await authenticatedPage
      .getByTestId("ssh-server-edit-name-input")
      .fill(after);

    // A tunnel row is dialed by id, not by name — the warning has to say so
    // rather than repeat the database row's "this breaks every client" copy.
    const warning = authenticatedPage.getByTestId("server-rename-warning");
    await expect(warning).toBeVisible();
    await expect(warning).toContainText(/reference it by id/i);

    await authenticatedPage.getByTestId("ssh-server-edit-submit").click();

    await expect(editDialog).not.toBeVisible({ timeout: 10000 });
    await expect(sshSection.getByText(after)).toBeVisible({ timeout: 10000 });
  });

  // The bastion in front of a cluster's API server used to be set-once: the
  // create dialog offered it and the edit dialog did not, so changing it meant
  // deleting the cluster and orphaning every database row dialed through it.
  test("a Kubernetes cluster's bastion can be set and cleared from its edit dialog", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const sshSection = authenticatedPage.getByTestId("ssh-servers-section");
    await expect(sshSection).toBeVisible();

    const stamp = Date.now();
    const bastionName = `e2e_via_bastion_${stamp}`;
    const clusterName = `e2e_via_cluster_${stamp}`;

    // A bastion to dial through…
    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("protocol-select").click();
    await authenticatedPage.getByTestId("protocol-option-ssh").click();
    await authenticatedPage
      .getByTestId("database-name-input")
      .fill(bastionName);
    await authenticatedPage.locator("#host").fill("bastion.example.com");
    await authenticatedPage.locator("#username").fill("bastion-user");
    await authenticatedPage.locator("#password").fill("bastion-password");
    await authenticatedPage.getByTestId("database-create-submit").click();
    await expect(
      sshSection.locator("tr", { hasText: bastionName }),
    ).toBeVisible({ timeout: 10000 });

    // …and a cluster created *without* one, so the edit dialog is what sets it.
    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("protocol-select").click();
    await authenticatedPage.getByTestId("protocol-option-kubernetes").click();
    await authenticatedPage
      .getByTestId("database-name-input")
      .fill(clusterName);
    await authenticatedPage.locator("#host").fill("api.cluster.example.com");
    await authenticatedPage.locator("#port").fill("6443");
    await authenticatedPage.locator("#username").fill("dbbat");
    await authenticatedPage.locator("#password").fill("sa-token");
    await authenticatedPage.getByTestId("k8s-namespace-input").fill("data");
    await authenticatedPage.getByTestId("database-create-submit").click();

    const clusterRow = sshSection.locator("tr", { hasText: clusterName });
    await expect(clusterRow).toBeVisible({ timeout: 10000 });

    const openClusterEdit = async () => {
      await clusterRow.locator('[data-testid^="ssh-server-edit-"]').click();
      const dialog = authenticatedPage.getByTestId("ssh-server-edit-dialog");
      await expect(dialog).toBeVisible();
      return dialog;
    };

    // Seeded from the row: nothing in front of it yet.
    let dialog = await openClusterEdit();
    const viaSelect = authenticatedPage.getByTestId(
      "k8s-server-edit-via-select",
    );
    await expect(viaSelect).toContainText("Direct (no tunnel)");

    await viaSelect.click();
    await authenticatedPage
      .getByRole("option", { name: new RegExp(`^${bastionName}`) })
      .click();
    await expect(viaSelect).toContainText(bastionName);
    await authenticatedPage.getByTestId("ssh-server-edit-submit").click();
    await expect(dialog).not.toBeVisible({ timeout: 10000 });

    // Re-read it from the server, not from the form state we just typed into.
    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");
    await expect(clusterRow).toBeVisible({ timeout: 10000 });

    dialog = await openClusterEdit();
    await expect(
      authenticatedPage.getByTestId("k8s-server-edit-via-select"),
    ).toContainText(bastionName);

    // And back to direct — a cleared selector has to send clear_via_uid, since
    // an omitted via_uid would leave the bastion in place.
    await authenticatedPage.getByTestId("k8s-server-edit-via-select").click();
    await authenticatedPage
      .getByRole("option", { name: "Direct (no tunnel)" })
      .click();
    await authenticatedPage.getByTestId("ssh-server-edit-submit").click();
    await expect(dialog).not.toBeVisible({ timeout: 10000 });

    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");
    await expect(clusterRow).toBeVisible({ timeout: 10000 });

    await openClusterEdit();
    await expect(
      authenticatedPage.getByTestId("k8s-server-edit-via-select"),
    ).toContainText("Direct (no tunnel)");
  });

  // An SSH bastion has no via selector of its own: chaining one behind another
  // is configured on the row that dials, and the create dialog does not offer
  // it either.
  test("an SSH bastion's edit dialog offers no via selector", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const sshSection = authenticatedPage.getByTestId("ssh-servers-section");
    await expect(sshSection).toBeVisible();

    const name = `e2e_novia_bastion_${Date.now()}`;

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

    await row.locator('[data-testid^="ssh-server-edit-"]').click();
    await expect(
      authenticatedPage.getByTestId("ssh-server-edit-dialog"),
    ).toBeVisible();
    await expect(
      authenticatedPage.getByTestId("k8s-server-edit-via-select"),
    ).toHaveCount(0);
  });
});

// A database row's connection details — host, port, the upstream database
// name, credentials, SSL mode, tunnel — used to be editable nowhere in this
// UI, so correcting a typo meant deleting and re-creating the row, which
// drops its grants, its session ledger and its query chains.
test.describe("Database row editing", () => {
  const API_BASE = "http://localhost:8080/api/v1";

  test("editing the host warns about the blast radius and saves", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const name = `e2e_edit_host_${Date.now()}`;

    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("database-name-input").fill(name);
    await authenticatedPage.locator("#host").fill("localhost");
    await authenticatedPage.locator("#databaseName").fill("postgres");
    await authenticatedPage.locator("#username").fill("postgres");
    await authenticatedPage.locator("#password").fill("postgres");
    await authenticatedPage.getByTestId("database-create-submit").click();

    const row = authenticatedPage.locator("tr", { hasText: name }).first();
    await expect(row).toBeVisible({ timeout: 10000 });

    await row.locator('[data-testid^="database-edit-"]').click();

    const dialog = authenticatedPage.getByTestId("database-edit-dialog");
    await expect(dialog).toBeVisible();

    // The protocol is deliberately not a field here: it is the one edit that
    // makes the row a different server, and the API refuses it with a 409
    // once anything references the row.
    await expect(dialog.locator("#edit-db-protocol")).toHaveCount(0);

    // Seeded from the row, and nothing has moved yet.
    await expect(
      authenticatedPage.getByTestId("database-edit-host-input"),
    ).toHaveValue("localhost");
    await expect(
      authenticatedPage.getByTestId("database-edit-target-warning"),
    ).toHaveCount(0);

    await authenticatedPage
      .getByTestId("database-edit-host-input")
      .fill("127.0.0.2");

    // Moving the target says what follows it, and counts what follows it.
    const warning = authenticatedPage.getByTestId(
      "database-edit-target-warning",
    );
    await expect(warning).toBeVisible();
    await expect(warning).toContainText(/moves where the row points/i);
    await expect(
      authenticatedPage.getByTestId("database-edit-target-counts"),
    ).toContainText(/reference this row/i, { timeout: 10000 });

    await authenticatedPage.getByTestId("database-edit-submit").click();
    await expect(dialog).not.toBeVisible({ timeout: 10000 });

    // Re-read the row: the edit landed, and the name is untouched.
    await authenticatedPage.reload();
    await authenticatedPage.waitForLoadState("networkidle");

    const saved = authenticatedPage.locator("tr", { hasText: name }).first();
    await expect(saved).toBeVisible({ timeout: 10000 });
    await expect(saved).toContainText("127.0.0.2");

    await saved.locator('[data-testid^="database-edit-"]').click();
    await expect(
      authenticatedPage.getByTestId("database-edit-host-input"),
    ).toHaveValue("127.0.0.2");
  });

  test("a rename-free edit leaves name out of the audit details", async ({
    authenticatedPage,
    request,
  }) => {
    const login = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    expect(login.status()).toBe(200);
    const { token } = await login.json();
    const auth = { Authorization: `Bearer ${token}` };

    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    const name = `e2e_edit_audit_${Date.now()}`;

    await authenticatedPage.getByTestId("add-database-button").click();
    await authenticatedPage.getByTestId("database-name-input").fill(name);
    await authenticatedPage.locator("#host").fill("localhost");
    await authenticatedPage.locator("#databaseName").fill("postgres");
    await authenticatedPage.locator("#username").fill("postgres");
    await authenticatedPage.locator("#password").fill("postgres");
    await authenticatedPage.getByTestId("database-create-submit").click();

    const row = authenticatedPage.locator("tr", { hasText: name }).first();
    await expect(row).toBeVisible({ timeout: 10000 });

    await row.locator('[data-testid^="database-edit-"]').click();
    await expect(
      authenticatedPage.getByTestId("database-edit-dialog"),
    ).toBeVisible();

    // One field, and a password rotation: the form sends exactly those two.
    await authenticatedPage
      .getByTestId("database-edit-description-input")
      .fill("edited by e2e");
    await authenticatedPage
      .getByTestId("database-edit-password-input")
      .fill("rotated-secret");
    await authenticatedPage.getByTestId("database-edit-submit").click();
    await expect(
      authenticatedPage.getByTestId("database-edit-dialog"),
    ).not.toBeVisible({ timeout: 10000 });

    const servers = await request.get(`${API_BASE}/servers`, {
      headers: auth,
    });
    expect(servers.status()).toBe(200);
    const uid = (await servers.json()).databases.find(
      (db: { name: string; uid: string }) => db.name === name,
    ).uid;

    const audit = await request.get(
      `${API_BASE}/audit?event_type=database.updated&limit=50`,
      { headers: auth },
    );
    expect(audit.status()).toBe(200);
    const entries: { details: Record<string, unknown> }[] = (await audit.json())
      .audit_events;

    const mine = entries.find(
      (entry) =>
        (entry.details as { database_uid?: string }).database_uid === uid,
    );
    expect(mine).toBeTruthy();

    const fields = (
      mine!.details as { updated_fields: Record<string, unknown> }
    ).updated_fields;

    expect(fields.description).toBe("edited by e2e");
    expect(fields.password_changed).toBe(true);
    // The whole point of diffing before the PUT: a rename-free edit says
    // nothing about the name, and an untouched host says nothing about the
    // host.
    expect(fields).not.toHaveProperty("name");
    expect(fields).not.toHaveProperty("host");
    // And the secret is never the value.
    expect(JSON.stringify(mine!.details)).not.toContain("rotated-secret");
  });
});
