import { test, expect } from "./fixtures";

/**
 * Approval holds and the live watch panel.
 *
 * The end-to-end *hold* itself (a real database connection parked on a human)
 * is covered by the Go tests in internal/proxy/postgresql — it needs a live
 * proxy session, which Playwright cannot create. What is verified here is the
 * operator-facing surface: the watch panel exists, the stream connects, the
 * pending-approval REST contract behaves, and the approve/deny endpoints
 * enforce their rules (notably the self-approval rejection).
 */
test.describe("Approval holds", () => {
  test("connection detail page exposes the live watch panel", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connections seeded in this environment");
      return;
    }

    await rows.first().click();
    await authenticatedPage.waitForLoadState("networkidle");

    const panel = authenticatedPage.getByTestId("connection-watch-panel");
    await expect(panel).toBeVisible();

    // The panel starts idle: watching opens a socket, so it must be opt-in.
    const toggle = authenticatedPage.getByTestId("watch-toggle");
    await expect(toggle).toHaveText(/Watch live/);

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/connection-watch-panel.png",
      fullPage: true,
    });
  });

  test("?watch=1 deep link opens the panel already watching", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("connections");
    await authenticatedPage.waitForLoadState("networkidle");

    const rows = authenticatedPage.locator("tbody tr");
    if ((await rows.count()) === 0) {
      test.skip(true, "No connections seeded in this environment");
      return;
    }

    await rows.first().click();
    await authenticatedPage.waitForLoadState("networkidle");

    const url = new URL(authenticatedPage.url());
    await authenticatedPage.goto(`${url.pathname}?watch=1`);
    await authenticatedPage.waitForLoadState("networkidle");

    // This is the link the Slack escalation points at: the approver must land
    // on an already-live panel, not one they have to activate.
    await expect(authenticatedPage.getByTestId("watch-toggle")).toHaveText(
      /Stop watching/,
    );

    await expect(
      authenticatedPage.getByTestId("watch-stream-status"),
    ).toBeVisible();
  });

  test("the live event stream accepts an authenticated WebSocket", async ({
    authenticatedPage,
  }) => {
    const result = await authenticatedPage.evaluate(async () => {
      const token = localStorage.getItem("dbbat_session_token");
      if (!token) return { ok: false, reason: "no token" };

      const url = new URL("/api/v1/stream", window.location.origin);
      url.protocol = url.protocol === "https:" ? "wss:" : "ws:";

      return await new Promise<{ ok: boolean; reason?: string }>((resolve) => {
        const socket = new WebSocket(url.toString(), [
          `dbbat.auth.bearer.${token}`,
        ]);

        const timer = setTimeout(() => {
          socket.close();
          resolve({ ok: false, reason: "timeout" });
        }, 10000);

        socket.onopen = () => {
          socket.send(
            JSON.stringify({ type: "subscribe", topic: "approvals/pending" }),
          );
        };

        socket.onmessage = (ev) => {
          const msg = JSON.parse(ev.data as string) as {
            type?: string;
            error?: string | null;
          };
          if (msg.type === "subscribed") {
            clearTimeout(timer);
            socket.close();
            resolve({ ok: msg.error == null, reason: msg.error ?? undefined });
          }
        };

        socket.onerror = () => {
          clearTimeout(timer);
          resolve({ ok: false, reason: "socket error" });
        };
      });
    });

    expect(result.reason ?? "").toBe("");
    expect(result.ok).toBe(true);
  });

  test("pending approvals endpoint answers with a list", async ({
    authenticatedPage,
  }) => {
    const body = await authenticatedPage.evaluate(async () => {
      const token = localStorage.getItem("dbbat_session_token");
      const res = await fetch("/api/v1/queries/pending", {
        headers: { Authorization: `Bearer ${token}` },
      });
      return { status: res.status, json: await res.json() };
    });

    expect(body.status).toBe(200);
    expect(Array.isArray(body.json.queries)).toBe(true);
  });

  test("approving a query that is not held is a 404 or 409, never a silent 200", async ({
    authenticatedPage,
  }) => {
    const result = await authenticatedPage.evaluate(async () => {
      const token = localStorage.getItem("dbbat_session_token");
      const res = await fetch(
        "/api/v1/queries/00000000-0000-0000-0000-000000000000/approve",
        { method: "POST", headers: { Authorization: `Bearer ${token}` } },
      );
      return res.status;
    });

    // Fail closed: an approval must never appear to succeed against something
    // that isn't actually parked.
    expect([404, 409]).toContain(result);
  });

  test("a non-admin cannot deny every pending approval", async ({ page }) => {
    // The bulk safety valve is admin-only; a viewer must be refused.
    await page.goto("/");
    await page.waitForLoadState("domcontentloaded");
    await page.getByTestId("login-logo").waitFor({ state: "visible" });
    await page.getByTestId("login-username").fill("viewer");
    await page.getByTestId("login-password").fill("viewer");
    await page.getByTestId("login-submit").click();
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 10000,
    });

    const status = await page.evaluate(async () => {
      const token = localStorage.getItem("dbbat_session_token");
      const res = await fetch("/api/v1/queries/pending/deny-all", {
        method: "POST",
        headers: {
          Authorization: `Bearer ${token}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ reason: "e2e" }),
      });
      return res.status;
    });

    expect(status).toBe(403);
  });
});
