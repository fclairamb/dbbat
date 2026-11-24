import { type APIRequestContext, type Page } from "@playwright/test";
import { test, expect, loginAs } from "./fixtures";

/**
 * The requester can withdraw their own pending grant request from the grant
 * requests page, behind a confirm dialog. Nobody else (an admin included) gets
 * the button.
 */

const API_BASE = "http://localhost:8080/api/v1";

// Seeds a database and a manual (non-auto-approve) definition through the API,
// so the request below lands pending and the connector has a database to pick.
async function seed(request: APIRequestContext, name: string) {
  const login = await request.post(`${API_BASE}/auth/login`, {
    data: { username: "admin", password: "admintest" },
  });
  expect(login.status()).toBe(200);
  const { token } = await login.json();
  const headers = { Authorization: `Bearer ${token}` };

  const server = await request.post(`${API_BASE}/servers`, {
    headers,
    data: {
      name: `e2e_cancel_db_${Date.now()}`,
      host: "localhost",
      port: 5432,
      database_name: "postgres",
      username: "postgres",
      password: "postgres",
    },
  });
  expect(server.status()).toBeLessThan(300);

  const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-");
  const def = await request.post(`${API_BASE}/grant-definitions`, {
    headers,
    data: {
      name,
      slug,
      duration_seconds: 3600,
      controls: ["read_only"],
    },
  });
  expect(def.status()).toBeLessThan(300);
}

async function submitRequest(page: Page, definitionName: string, why: string) {
  await page.goto("grant-requests");
  await page.waitForLoadState("networkidle");
  await page.getByTestId("request-grant-button").click();
  await page.waitForSelector('[role="dialog"]');
  await page.getByTestId("grant-request-definition").click();
  await page.getByRole("option", { name: definitionName }).click();
  await page.getByTestId("grant-request-database").click();
  await page.getByRole("option").first().click();
  await page.getByTestId("grant-request-justification").fill(why);
  await page.getByTestId("grant-request-submit").click();
  await expect(page.locator('[role="dialog"]')).toBeHidden();
}

test.describe("Grant Requests: requester withdraws", () => {
  test("connector withdraws a pending request after confirming; admin has no Cancel button", async ({
    authenticatedPage: page,
    request,
  }) => {
    const name = `E2E Cancel ${Date.now()}`;
    await seed(request, name);

    // Connector files the request.
    await page.context().clearCookies();
    await page.evaluate(() => localStorage.clear());
    await loginAs(page, "connector", "connector");
    await submitRequest(page, name, "E2E cancel request");

    const connectorRow = page.locator("tr", { hasText: name }).first();
    await expect(connectorRow).toContainText("pending");

    // An admin sees the pending request but cannot withdraw it.
    await page.context().clearCookies();
    await page.evaluate(() => localStorage.clear());
    await loginAs(page, "admin", "admintest");
    await page.goto("grant-requests");
    await page.waitForLoadState("networkidle");
    const adminRow = page.locator("tr", { hasText: name }).first();
    await expect(adminRow).toContainText("pending");
    await expect(adminRow.locator('[data-testid^="cancel-"]')).toHaveCount(0);

    // Back as the connector: the button asks for confirmation first.
    await page.context().clearCookies();
    await page.evaluate(() => localStorage.clear());
    await loginAs(page, "connector", "connector");
    await page.goto("grant-requests");
    await page.waitForLoadState("networkidle");
    const row = page.locator("tr", { hasText: name }).first();
    await row.locator('[data-testid^="cancel-"]').click();
    await expect(page.getByTestId("withdraw-dialog")).toBeVisible();

    // Keeping it leaves the request pending.
    await page.getByTestId("withdraw-keep").click();
    await expect(page.getByTestId("withdraw-dialog")).toBeHidden();
    await expect(row).toContainText("pending");

    // Confirming withdraws it.
    await row.locator('[data-testid^="cancel-"]').click();
    await page.getByTestId("withdraw-confirm").click();
    await expect(page.getByTestId("withdraw-dialog")).toBeHidden();
    await expect(page.locator("tr", { hasText: name }).first()).toContainText(
      "cancelled",
    );
  });
});
