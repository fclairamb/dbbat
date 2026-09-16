import { test, expect, loginAs } from "./fixtures";

/**
 * Ending a live session is an admin action on a *live* session, and the button
 * has to be absent on both halves of that: a viewer looking at any session, and
 * anyone looking at one that has already ended.
 *
 * Test mode seeds a closed connection and no live one (nothing is proxying),
 * so the closed half is asserted through the UI here and the live half — the
 * button appearing, the dialog, the session actually dropping — is covered by
 * the PostgreSQL integration suite, which has a real proxied session to end.
 */
test.describe("Terminate session", () => {
  /** The seeded sample connection's uid, read from the connections list. */
  async function firstConnectionUid(page: Parameters<typeof loginAs>[0]) {
    const uid = await page.evaluate(async () => {
      const token = localStorage.getItem("dbbat_session_token");
      const res = await fetch("/api/v1/connections?limit=1", {
        headers: { Authorization: `Bearer ${token}` },
      });
      const body = await res.json();
      return body.connections?.[0]?.uid ?? null;
    });

    expect(uid, "test mode should seed at least one connection").toBeTruthy();

    return uid as string;
  }

  test("the button is absent on a closed session, even for an admin", async ({
    authenticatedPage,
  }) => {
    const uid = await firstConnectionUid(authenticatedPage);

    await authenticatedPage.goto(`connections/${uid}`);
    await authenticatedPage.waitForLoadState("networkidle");

    // The page loaded, and the session is closed.
    await expect(
      authenticatedPage.getByText("Connection Information"),
    ).toBeVisible();

    await expect(
      authenticatedPage.getByTestId("terminate-session-button"),
    ).toHaveCount(0);
  });

  test("the button is absent for a viewer", async ({ page }) => {
    await loginAs(page, "viewer", "viewer");

    const uid = await firstConnectionUid(page);

    await page.goto(`connections/${uid}`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByText("Connection Information")).toBeVisible();
    await expect(page.getByTestId("terminate-session-button")).toHaveCount(0);
  });

  test("the endpoint refuses a viewer outright", async ({ page }) => {
    // The UI hiding the button is not the boundary — the API is. A viewer who
    // calls it by hand is answered 403, not 202.
    await loginAs(page, "viewer", "viewer");

    const uid = await firstConnectionUid(page);

    const status = await page.evaluate(async (connectionUid) => {
      const token = localStorage.getItem("dbbat_session_token");
      const res = await fetch(`/api/v1/connections/${connectionUid}/terminate`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${token}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ reason: "e2e" }),
      });
      return res.status;
    }, uid);

    expect(status).toBe(403);
  });

  test("an admin terminating an already-closed session gets a conflict", async ({
    authenticatedPage,
  }) => {
    const uid = await firstConnectionUid(authenticatedPage);

    const status = await authenticatedPage.evaluate(async (connectionUid) => {
      const token = localStorage.getItem("dbbat_session_token");
      const res = await fetch(`/api/v1/connections/${connectionUid}/terminate`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${token}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ reason: "e2e" }),
      });
      return res.status;
    }, uid);

    // 409, not 404 and not 202: the session exists, there is just nothing left
    // to end.
    expect(status).toBe(409);
  });
});
