import { execFileSync } from "node:child_process";
import { test, expect } from "./fixtures";

// The same database the E2E server runs against (see global-setup.ts).
const DSN =
  process.env.E2E_DSN ??
  "postgres://postgres:postgres@localhost:5001/dbbat?sslmode=disable";

function psql(sql: string): void {
  execFileSync("psql", [DSN, "-v", "ON_ERROR_STOP=1", "-c", sql], {
    stdio: "pipe",
  });
}

test.describe("Unverified badge on unchained rows", () => {
  const forgedAuditUID = "ffffffff-ffff-7fff-bfff-00000000a001";

  test.beforeAll(() => {
    // A row carrying no chain position, as a forged or backdated one would.
    psql(
      `INSERT INTO audit_log (uid, event_type, details, created_at, chain_seq)
       VALUES ('${forgedAuditUID}', 'e2e.unchained', '{}', now(), NULL)
       ON CONFLICT DO NOTHING`,
    );
  });

  test.afterAll(() => {
    psql(`DELETE FROM audit_log WHERE uid = '${forgedAuditUID}'`);
  });

  test("audit page flags a row with no chain position", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("audit");
    await authenticatedPage.waitForLoadState("networkidle");

    const badge = authenticatedPage.getByTestId(
      `audit-unverified-badge-${forgedAuditUID}`,
    );
    await expect(badge).toBeVisible();
    await expect(badge).toHaveText("Unverified");
    await expect(badge).toHaveAttribute("title", /dbbat audit verify/);

    // Chained rows (the login events written by this very run) carry none.
    await expect(
      authenticatedPage.locator('[data-testid^="audit-unverified-badge-"]'),
    ).toHaveCount(1);
  });
});
