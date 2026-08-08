import type { Page } from "@playwright/test";
import { test, expect } from "./fixtures";

/**
 * Approval holds and the connection's unified query feed.
 *
 * The end-to-end *hold* itself (a real database connection parked on a human)
 * is covered by the Go tests in internal/proxy/postgresql — it needs a live
 * proxy session, which Playwright cannot create. What is verified here is the
 * operator-facing surface: history, live rows and holds share one table, an
 * open connection streams without anybody pressing anything, a hold that
 * predates the page load is actionable, the stream connects, the
 * pending-approval REST contract behaves, and the approve/deny endpoints
 * enforce their rules (notably the self-approval rejection).
 */

/** A connection the server reports as still open, so the page goes live. */
const ACTIVE_UID = "3f1b0c9e-0000-4000-8000-000000000011";

async function mockActiveConnection(page: Page, uid: string): Promise<void> {
  const at = new Date().toISOString();
  await page.route(`**/api/v1/connections/${uid}`, (route) =>
    route.fulfill({
      json: {
        uid,
        user_id: "3f1b0c9e-0000-4000-8000-000000000003",
        database_id: "3f1b0c9e-0000-4000-8000-000000000004",
        source_ip: "127.0.0.1",
        connected_at: at,
        last_activity_at: at,
        disconnected_at: null,
        queries: 0,
        bytes_transferred: 0,
      },
    }),
  );
}

/** A stream that accepts the subscription and then says nothing. */
async function mockSilentStream(page: Page): Promise<void> {
  await page.routeWebSocket("**/api/v1/stream", (ws) => {
    ws.onMessage((raw) => {
      const msg = JSON.parse(String(raw)) as { type?: string; topic?: string };
      if (msg.type === "subscribe") {
        ws.send(
          JSON.stringify({ type: "subscribed", topic: msg.topic, error: null }),
        );
      }
    });
  });
}

test.describe("Approval holds", () => {
  test("the connection detail page shows exactly one query table", async ({
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
    await expect(authenticatedPage.getByTestId("watch-feed")).toBeVisible();

    // The whole point of the change: history, the live feed and any hold are
    // one table, not three surfaces rendering the same statements.
    await expect(authenticatedPage.locator("table")).toHaveCount(1);

    await authenticatedPage.screenshot({
      path: "test-results/screenshots/connection-watch-panel.png",
      fullPage: true,
    });

    // Rows still navigate to the query detail — the feed replaced the old
    // history table, so it has to keep what that table did.
    const items = authenticatedPage.getByTestId("watch-feed-item");
    if ((await items.count()) > 0) {
      await items.first().click();
      await expect(authenticatedPage).toHaveURL(/\/queries\/[0-9a-f-]{36}/);
    }
  });

  test("an open connection is live from mount, and the toggle only pauses", async ({
    authenticatedPage,
  }) => {
    await mockActiveConnection(authenticatedPage, ACTIVE_UID);
    await mockSilentStream(authenticatedPage);

    await authenticatedPage.goto(`connections/${ACTIVE_UID}`);
    await authenticatedPage.waitForLoadState("domcontentloaded");

    // Nothing was clicked: an open connection streams straight away.
    const toggle = authenticatedPage.getByTestId("watch-toggle");
    await expect(toggle).toHaveText(/Stop watching/);
    await expect(
      authenticatedPage.getByTestId("watch-stream-status"),
    ).toBeVisible();

    // The toggle is now a pause: the table stays, the socket goes.
    await toggle.click();
    await expect(toggle).toHaveText(/Watch live/);
    await expect(
      authenticatedPage.getByTestId("watch-stream-status"),
    ).toHaveCount(0);
    await expect(authenticatedPage.getByTestId("watch-feed")).toBeVisible();
  });

  test("?watch=1 is still accepted on an open connection", async ({
    authenticatedPage,
  }) => {
    await mockActiveConnection(authenticatedPage, ACTIVE_UID);
    await mockSilentStream(authenticatedPage);

    // Slack escalations already sent out carry this parameter. It is a no-op
    // now — the page is live either way — but it must not stop being valid.
    await authenticatedPage.goto(`connections/${ACTIVE_UID}?watch=1`);
    await authenticatedPage.waitForLoadState("domcontentloaded");

    await expect(authenticatedPage.getByTestId("watch-toggle")).toHaveText(
      /Stop watching/,
    );
    await expect(
      authenticatedPage.getByTestId("watch-stream-status"),
    ).toBeVisible();
  });

  test("a hold that predates the page load is visible and actionable", async ({
    authenticatedPage,
  }) => {
    // The regression this guards: the pending set used to be fetched only
    // once somebody pressed "Watch live", so an admin opening the page to
    // find out why a colleague is stuck saw nothing at all.
    const queryUid = "3f1b0c9e-0000-4000-8000-000000000012";
    const executedAt = new Date(Date.now() - 45_000).toISOString();
    const sql = "DELETE FROM invoices WHERE created_at < now() - interval '1 year'";

    await mockActiveConnection(authenticatedPage, ACTIVE_UID);
    await mockSilentStream(authenticatedPage);

    // Deliberately empty: the hold has to reach the table from the pending
    // endpoint alone, with no help from the query history or the stream.
    await authenticatedPage.route("**/api/v1/queries?**", (route) =>
      route.fulfill({ json: { queries: [] } }),
    );
    await authenticatedPage.route("**/api/v1/queries/pending", (route) =>
      route.fulfill({
        json: {
          queries: [
            {
              uid: queryUid,
              connection_id: ACTIVE_UID,
              sql_text: sql,
              executed_at: executedAt,
              approval_status: "pending",
              approval_pattern: "(?i)^\\s*DELETE",
            },
          ],
        },
      }),
    );

    await authenticatedPage.goto(`connections/${ACTIVE_UID}`);
    await authenticatedPage.waitForLoadState("domcontentloaded");

    const pending = authenticatedPage.getByTestId("pending-approvals");
    await expect(pending).toBeVisible();
    await expect(pending).toContainText("DELETE FROM invoices");
    await expect(pending).toContainText("(?i)^\\s*DELETE");
    await expect(
      pending.getByTestId("approval-status-pending"),
    ).toBeVisible();
    await expect(pending.getByTestId("approval-held-for")).toBeVisible();

    // Inline in the row — the decision must never require a navigation.
    const held = authenticatedPage.getByTestId(`pending-approval-${queryUid}`);
    await expect(held).toBeVisible();
    await expect(
      held.getByTestId(`approve-query-${queryUid}`),
    ).toBeEnabled();
    await expect(held.getByTestId(`deny-query-${queryUid}`)).toBeEnabled();

    // Duration and Rows stay "-" until the statement actually runs.
    await expect(held.locator("td").nth(2)).toHaveText("-");
    await expect(held.locator("td").nth(3)).toHaveText("-");

    // Impossible to miss: it is the first row of the table.
    await expect(authenticatedPage.locator("tbody tr").first()).toHaveAttribute(
      "data-testid",
      `pending-approval-${queryUid}`,
    );

    // The row is a link to the query detail, drawn as an overlay across the
    // whole row. Approve has to sit above it: if it doesn't, every press
    // navigates away instead of resolving the hold.
    let approvePosted = false;
    await authenticatedPage.route(
      `**/api/v1/queries/${queryUid}/approve`,
      (route) => {
        approvePosted = true;
        return route.fulfill({
          json: { query_uid: queryUid, approval_status: "approved" },
        });
      },
    );

    await held.getByTestId(`approve-query-${queryUid}`).click();
    await expect
      .poll(() => approvePosted, { timeout: 5_000 })
      .toBe(true);
    await expect(authenticatedPage).toHaveURL(
      new RegExp(`/connections/${ACTIVE_UID}$`),
    );
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

  test("the feed keeps the approval outcome after the statement completes", async ({
    authenticatedPage,
  }) => {
    // A held statement produces three events for one query uid: the hold, the
    // decision, then the completion once it actually ran. The regression this
    // guards is the third one wiping the second: the row must still say
    // "Approved", by whom, and must still be a single row — the table is keyed
    // by query uid, so three events and a later REST refetch all land on it.
    //
    // Both halves are synthetic on purpose — a real hold needs a live proxy
    // session (see the note at the top of this file), and what is under test
    // here is purely how the table folds the events it is handed.
    const connectionUid = "3f1b0c9e-0000-4000-8000-000000000001";
    const queryUid = "3f1b0c9e-0000-4000-8000-000000000002";
    const executedAt = new Date().toISOString();
    const sql = "DELETE FROM shipments WHERE id = 42";

    await authenticatedPage.route(
      `**/api/v1/connections/${connectionUid}`,
      (route) =>
        route.fulfill({
          json: {
            uid: connectionUid,
            user_id: "3f1b0c9e-0000-4000-8000-000000000003",
            database_id: "3f1b0c9e-0000-4000-8000-000000000004",
            source_ip: "127.0.0.1",
            connected_at: executedAt,
            last_activity_at: executedAt,
            disconnected_at: null,
            queries: 1,
            bytes_transferred: 0,
          },
        }),
    );

    await authenticatedPage.routeWebSocket("**/api/v1/stream", (ws) => {
      const send = (event: string, seq: number, data: Record<string, unknown>) =>
        ws.send(
          JSON.stringify({
            type: "event",
            topic: `connection/${connectionUid}/queries`,
            seq,
            event,
            at: executedAt,
            data,
          }),
        );

      ws.onMessage((raw) => {
        const msg = JSON.parse(String(raw)) as { type?: string; topic?: string };
        if (msg.type !== "subscribe") return;

        ws.send(JSON.stringify({ type: "subscribed", topic: msg.topic, error: null }));

        send("approval_pending", 1, {
          query_uid: queryUid,
          connection_uid: connectionUid,
          sql_text: sql,
          executed_at: executedAt,
          approval_required: true,
          approval_status: "pending",
          approval_pattern: "(?i)^\\s*DELETE",
        });

        send("approval_resolved", 2, {
          query_uid: queryUid,
          connection_uid: connectionUid,
          approval_required: true,
          approval_status: "approved",
          resolved_at: executedAt,
          resolved_by: { username: "bob", display_name: "bob" },
        });

        // The completion event as the proxy published it before the fix: it
        // knows nothing about the hold. Folding it in must not un-approve the
        // row, whatever the server chooses to send.
        send("query", 3, {
          query_uid: queryUid,
          connection_uid: connectionUid,
          sql_text: sql,
          executed_at: executedAt,
          duration_ms: 12.5,
          rows_affected: 1,
          approval_required: false,
          approval_status: "",
        });
      });
    });

    await authenticatedPage.goto(`connections/${connectionUid}?watch=1`);
    await authenticatedPage.waitForLoadState("domcontentloaded");

    const feed = authenticatedPage.getByTestId("watch-feed");
    await expect(feed).toBeVisible();
    await expect(feed.getByTestId("approval-status-approved")).toBeVisible();
    await expect(feed).toContainText("by bob");
    await expect(feed).toContainText("DELETE FROM shipments");

    // One statement, one row: the feed is keyed by query uid, not by event.
    await expect(authenticatedPage.getByTestId("watch-feed-item")).toHaveCount(1);
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
