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

  test("create dialog rejects a hyphenated name before it reaches the server", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("servers");
    await authenticatedPage.waitForLoadState("networkidle");

    await authenticatedPage.getByTestId("add-database-button").click();

    const nameInput = authenticatedPage.getByTestId("database-name-input");
    await nameInput.fill("bad-name");
    await authenticatedPage.locator("#host").fill("db.example.com");
    await authenticatedPage.locator("#username").fill("postgres");
    await authenticatedPage.locator("#password").fill("secret");

    // The server name is a slug (^[a-z0-9_]{1,63}$) — no hyphens. The input's
    // native HTML5 pattern must catch this before any request is made.
    const isValid = await nameInput.evaluate((el: HTMLInputElement) =>
      el.checkValidity()
    );
    expect(isValid).toBe(false);

    await authenticatedPage.getByTestId("database-create-submit").click();

    // The browser blocks the submit, so the dialog stays open with the
    // rejected value still in the field rather than a round-trip 400.
    await expect(nameInput).toBeVisible();
    await expect(nameInput).toHaveValue("bad-name");
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
      row.locator('[data-testid^="tunnel-insecure-badge-"]')
    ).toHaveCount(0);
    await expect(
      row.locator('[data-testid^="tunnel-ca-pinned-badge-"]')
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
      authenticatedPage.waitForResponse(
        (r) => /\/api\/v1\/servers\/[^/]+\/test$/.test(r.url())
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
        authenticatedPage.locator("tr", { hasText: name }).first()
      ).toBeVisible({ timeout: 10000 });
    };

    await createOracleRow(first, `oracle-${stamp}.db.example.com`);
    // Before the second row exists there is nothing to disagree with.
    const firstRow = authenticatedPage.locator("tr", { hasText: first }).first();
    await expect(
      firstRow.locator('[data-testid^="database-oracle-conflict-"]')
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
      authenticatedPage.getByTestId("server-rename-warning")
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
      authenticatedPage.locator("tr", { hasText: after }).first()
    ).toBeVisible({ timeout: 10000 });
    await expect(
      authenticatedPage.locator("tr", { hasText: before })
    ).toHaveCount(0);
  });

  test("the rename dialog rejects a hyphenated name before it reaches the server", async ({
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
      authenticatedPage.getByTestId("database-rename-dialog")
    ).toBeVisible();

    const input = authenticatedPage.getByTestId("database-rename-input");
    await input.fill(`${name}-renamed`);
    await authenticatedPage.getByTestId("database-rename-submit").click();

    // The slug gate the create dialog enforces applies to the rename too: the
    // form's own pattern refuses to submit, so the dialog stays open.
    const isValid = await input.evaluate((el: HTMLInputElement) =>
      el.checkValidity()
    );
    expect(isValid).toBe(false);
    await expect(
      authenticatedPage.getByTestId("database-rename-dialog")
    ).toBeVisible();
    await expect(
      authenticatedPage.locator("tr", { hasText: name }).first()
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
});
