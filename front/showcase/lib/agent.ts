/**
 * A drawn AI-agent pane, driven by a real MCP client.
 *
 * The sibling of ./terminal.ts, and drawn for the same reason: the MCP clip has
 * to show both halves of a hold — the operator's browser *and* the agent whose
 * statement is parked on them — and no agent client renders reproducibly in CI.
 * So the transcript is DOM injected into the dbbat page and typed into by the
 * script, while every call it depicts is genuinely made against
 * `POST /api/v1/mcp` by ./mcp.ts.
 *
 * The rule is ./terminal.ts's rule: **nothing about a result is faked**. Every
 * `execution_id`, `query_uid`, approval pattern, message, row and duration the
 * pane prints was read off the real MCP response, in the order shown.
 *
 * ## Why this one survives navigation
 *
 * ./terminal.ts is mounted with `page.evaluate` after the page it belongs to
 * has loaded, because the `psql` scenario never leaves that page. The MCP
 * scenario has to: an agent's statement runs on its own loopback connection, so
 * the connection the operator watches *does not exist* until the call is in
 * flight, and the clip would otherwise have to show the hold arriving before
 * the call that caused it. Installing through `addInitScript` and replaying the
 * transcript out of `sessionStorage` lets the call be typed first, on the
 * connections list, and the session be opened afterwards — which is the real
 * order of events.
 */
import type { Page } from "@playwright/test";

/** Line styles the pane understands. */
export type AgentClass =
  | "call" // an outgoing tool call's heading and body
  | "json" // a line of a response body
  | "muted"
  | "ok" // a finished result's heading
  | "warn"; // an approval_pending heading

const AGENT_SCRIPT = `
(() => {
  if (window.__dbbatAgentInstalled) return;
  window.__dbbatAgentInstalled = true;

  const LINES_KEY = "dbbat.showcase.agent.lines";
  const TITLE_KEY = "dbbat.showcase.agent.title";

  const install = () => {
    if (!document.body || document.getElementById("dbbat-showcase-agent")) return;

    const el = document.createElement("div");
    el.id = "dbbat-showcase-agent";
    el.innerHTML =
      '<div class="dbbat-agent-bar">' +
        '<span class="dbbat-agent-badge">MCP</span>' +
        '<span class="dbbat-agent-title"></span>' +
        '<span class="dbbat-agent-live"></span>' +
      '</div>' +
      '<pre class="dbbat-agent-body"></pre>';

    const style = document.createElement("style");
    style.textContent = \`
      #dbbat-showcase-agent {
        position: fixed;
        left: 0; right: 0; bottom: 0;
        height: 272px;
        z-index: 2147483000;
        display: flex;
        flex-direction: column;
        background: #0b1020;
        border-top: 1px solid rgba(148, 163, 184, 0.35);
        box-shadow: 0 -12px 32px rgba(2, 6, 23, 0.45);
        font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
      }
      #dbbat-showcase-agent .dbbat-agent-bar {
        display: flex; align-items: center; gap: 10px;
        padding: 7px 14px;
        background: #111931;
        border-bottom: 1px solid rgba(148, 163, 184, 0.2);
      }
      #dbbat-showcase-agent .dbbat-agent-badge {
        font-size: 10px; font-weight: 700; letter-spacing: 0.08em;
        color: #0b1020; background: #a78bfa;
        padding: 2px 6px; border-radius: 4px;
      }
      #dbbat-showcase-agent .dbbat-agent-title {
        color: #cbd5e1; font-size: 12px; letter-spacing: 0.02em;
      }
      #dbbat-showcase-agent .dbbat-agent-live {
        margin-left: auto; width: 8px; height: 8px; border-radius: 50%;
        background: #4ade80; box-shadow: 0 0 0 3px rgba(74, 222, 128, 0.18);
      }
      #dbbat-showcase-agent .dbbat-agent-body {
        margin: 0; flex: 1; overflow: hidden;
        padding: 10px 16px 14px;
        color: #e2e8f0; font-size: 13px; line-height: 1.45;
        white-space: pre-wrap; word-break: break-word;
      }
      #dbbat-showcase-agent .call { color: #a78bfa; }
      #dbbat-showcase-agent .json { color: #cbd5e1; }
      #dbbat-showcase-agent .muted { color: #94a3b8; }
      #dbbat-showcase-agent .ok { color: #4ade80; font-weight: 600; }
      #dbbat-showcase-agent .warn { color: #fbbf24; font-weight: 600; }
      #dbbat-showcase-agent .caret {
        display: inline-block; width: 8px; height: 1.05em;
        background: #e2e8f0; vertical-align: text-bottom;
        animation: dbbat-agent-blink 1s steps(1) infinite;
      }
      @keyframes dbbat-agent-blink { 50% { opacity: 0; } }
    \`;

    document.head.appendChild(style);
    document.body.appendChild(el);

    // The pane is fixed to the bottom of the viewport, so without this the
    // app's last 272px sit underneath it — including the Approve button the
    // clip has to click.
    document.body.style.paddingBottom = "292px";
    document.documentElement.style.scrollPaddingBottom = "292px";

    const body = el.querySelector(".dbbat-agent-body");
    const title = el.querySelector(".dbbat-agent-title");
    title.textContent = sessionStorage.getItem(TITLE_KEY) || "";

    let current = null;

    const trim = () => {
      // Keep the transcript inside its box: drop the oldest lines, the way a
      // chat pane scrolls. Only the DOM is trimmed — the stored transcript
      // keeps everything, so a re-mount replays the whole exchange.
      while (body.scrollHeight > body.clientHeight && body.firstChild) {
        body.removeChild(body.firstChild);
      }
    };

    const append = (text, cls) => {
      const line = document.createElement("div");
      line.className = cls || "json";
      line.textContent = text;
      body.appendChild(line);
      trim();
    };

    const stored = () => {
      try {
        return JSON.parse(sessionStorage.getItem(LINES_KEY) || "[]");
      } catch (err) {
        return [];
      }
    };

    const remember = (text, cls) => {
      const lines = stored();
      lines.push({ text: text, cls: cls || "json" });
      sessionStorage.setItem(LINES_KEY, JSON.stringify(lines));
    };

    for (const line of stored()) {
      append(line.text, line.cls);
    }

    window.__dbbatAgent = {
      setTitle(text) {
        sessionStorage.setItem(TITLE_KEY, text);
        title.textContent = text;
      },
      write(text, cls) {
        append(text, cls);
        remember(text, cls);
      },
      open(cls) {
        const line = document.createElement("div");
        line.className = cls || "json";
        const typed = document.createElement("span");
        const caret = document.createElement("span");
        caret.className = "caret";
        line.appendChild(typed);
        line.appendChild(caret);
        body.appendChild(line);
        current = { typed: typed, caret: caret, cls: cls || "json" };
        trim();
      },
      type(chunk) {
        if (!current) return;
        current.typed.textContent += chunk;
        trim();
      },
      close() {
        if (!current) return;
        current.caret.remove();
        remember(current.typed.textContent, current.cls);
        current = null;
      },
    };
  };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", install);
  } else {
    install();
  }
})();
`;

interface AgentBridge {
  setTitle(s: string): void;
  write(s: string, cls: string): void;
  open(cls: string): void;
  type(s: string): void;
  close(): void;
}

/**
 * Install the pane on this page and on every page it navigates to.
 *
 * Call it before the first `goto`. The transcript lives in `sessionStorage`, so
 * a navigation re-renders what has already been said instead of losing it.
 */
export async function installAgentPane(
  page: Page,
  title: string,
): Promise<void> {
  await page.addInitScript(AGENT_SCRIPT);
  await page.addInitScript(
    ([key, value]) => sessionStorage.setItem(key, value),
    ["dbbat.showcase.agent.title", title] as const,
  );
}

/** Append a finished line. */
export async function writeAgentLine(
  page: Page,
  text: string,
  cls: AgentClass = "json",
): Promise<void> {
  await page.evaluate(
    ({ t, c }) =>
      (window as unknown as { __dbbatAgent: AgentBridge }).__dbbatAgent.write(
        t,
        c,
      ),
    { t: text, c: cls },
  );
}

/** Append several finished lines. */
export async function writeAgentLines(
  page: Page,
  lines: string[],
  cls: AgentClass = "json",
): Promise<void> {
  for (const line of lines) {
    await writeAgentLine(page, line, cls);
  }
}

/**
 * Type `lines` out one character at a time, carrying a caret.
 *
 * This is the outgoing half of the transcript — the request an agent composes.
 * At `msPerChar = 0` it lands instantly, which is what the poster wants.
 */
export async function typeAgentLines(
  page: Page,
  lines: string[],
  msPerChar: number,
  cls: AgentClass = "call",
): Promise<void> {
  for (const line of lines) {
    await page.evaluate(
      (c) =>
        (window as unknown as { __dbbatAgent: AgentBridge }).__dbbatAgent.open(
          c,
        ),
      cls,
    );

    if (msPerChar <= 0) {
      await page.evaluate(
        (t) =>
          (window as unknown as { __dbbatAgent: AgentBridge }).__dbbatAgent.type(
            t,
          ),
        line,
      );
    } else {
      for (const ch of line) {
        await page.evaluate(
          (c) =>
            (
              window as unknown as { __dbbatAgent: AgentBridge }
            ).__dbbatAgent.type(c),
          ch,
        );
        await page.waitForTimeout(msPerChar);
      }
    }

    await page.evaluate(() =>
      (window as unknown as { __dbbatAgent: AgentBridge }).__dbbatAgent.close(),
    );
  }
}
