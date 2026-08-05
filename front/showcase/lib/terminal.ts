/**
 * A fake terminal pane, driven by a real client.
 *
 * The approval video has to show both halves of a hold: the operator's browser
 * *and* the database client that is blocked on them. Recording an actual
 * terminal emulator is not reproducible in CI (fonts, window chrome, prompt
 * theming, shell version), so the pane is DOM injected into the dbbat page and
 * typed into by the script — while the statement it displays is genuinely
 * executed through the proxy by the `pg` client in ../lib/traffic.ts.
 *
 * Nothing here fakes a *result*: every line of output the pane prints comes
 * back from the real connection.
 */
import type { Page } from "@playwright/test";

const TERMINAL_SCRIPT = `
(() => {
  if (window.__dbbatTerm) return;

  const el = document.createElement("div");
  el.id = "dbbat-showcase-term";
  el.innerHTML =
    '<div class="dbbat-term-bar">' +
      '<span class="dbbat-term-dot dbbat-term-dot-r"></span>' +
      '<span class="dbbat-term-dot dbbat-term-dot-y"></span>' +
      '<span class="dbbat-term-dot dbbat-term-dot-g"></span>' +
      '<span class="dbbat-term-title"></span>' +
    '</div>' +
    '<pre class="dbbat-term-body"></pre>';

  const style = document.createElement("style");
  style.textContent = \`
    #dbbat-showcase-term {
      position: fixed;
      left: 0; right: 0; bottom: 0;
      height: 250px;
      z-index: 2147483000;
      display: flex;
      flex-direction: column;
      background: #0b1020;
      border-top: 1px solid rgba(148, 163, 184, 0.35);
      box-shadow: 0 -12px 32px rgba(2, 6, 23, 0.45);
      font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
    }
    #dbbat-showcase-term .dbbat-term-bar {
      display: flex; align-items: center; gap: 7px;
      padding: 7px 12px;
      background: #111931;
      border-bottom: 1px solid rgba(148, 163, 184, 0.2);
    }
    #dbbat-showcase-term .dbbat-term-dot {
      width: 11px; height: 11px; border-radius: 50%; display: inline-block;
    }
    .dbbat-term-dot-r { background: #ff5f57; }
    .dbbat-term-dot-y { background: #febc2e; }
    .dbbat-term-dot-g { background: #28c840; }
    #dbbat-showcase-term .dbbat-term-title {
      margin-left: 10px; color: #94a3b8; font-size: 12px; letter-spacing: 0.02em;
    }
    #dbbat-showcase-term .dbbat-term-body {
      margin: 0; flex: 1; overflow: hidden;
      padding: 12px 16px 16px;
      color: #e2e8f0; font-size: 13.5px; line-height: 1.55;
      white-space: pre-wrap; word-break: break-word;
    }
    #dbbat-showcase-term .p { color: #38bdf8; }
    #dbbat-showcase-term .out { color: #cbd5e1; }
    #dbbat-showcase-term .muted { color: #94a3b8; }
    #dbbat-showcase-term .ok { color: #4ade80; }
    #dbbat-showcase-term .warn { color: #fbbf24; }
    #dbbat-showcase-term .caret {
      display: inline-block; width: 8px; height: 1.05em;
      background: #e2e8f0; vertical-align: text-bottom;
      animation: dbbat-term-blink 1s steps(1) infinite;
    }
    @keyframes dbbat-term-blink { 50% { opacity: 0; } }
  \`;

  document.head.appendChild(style);
  document.body.appendChild(el);

  // The pane is fixed to the bottom of the viewport, so without this the app's
  // last 250px sit underneath it — including, on the connection page, the
  // Approve button the video has to click.
  document.body.style.paddingBottom = "270px";
  document.documentElement.style.scrollPaddingBottom = "270px";

  const body = el.querySelector(".dbbat-term-body");
  const title = el.querySelector(".dbbat-term-title");
  let current = null;

  const trim = () => {
    // Keep the pane from growing past its box: drop the oldest lines.
    while (body.scrollHeight > body.clientHeight && body.firstChild) {
      body.removeChild(body.firstChild);
    }
  };

  window.__dbbatTerm = {
    setTitle(text) { title.textContent = text; },
    prompt(text) {
      const line = document.createElement("div");
      const p = document.createElement("span");
      p.className = "p";
      p.textContent = text + " ";
      const typed = document.createElement("span");
      const caret = document.createElement("span");
      caret.className = "caret";
      line.appendChild(p);
      line.appendChild(typed);
      line.appendChild(caret);
      body.appendChild(line);
      current = { typed, caret };
      trim();
    },
    type(chunk) {
      if (!current) return;
      current.typed.textContent += chunk;
      trim();
    },
    submit() {
      if (current) { current.caret.remove(); current = null; }
    },
    write(text, cls) {
      const line = document.createElement("div");
      line.className = cls || "out";
      line.textContent = text;
      body.appendChild(line);
      trim();
    },
  };
})();
`;

/** Mount the pane (idempotent) on an already-loaded page. */
export async function mountTerminal(page: Page, title: string): Promise<void> {
  await page.evaluate(TERMINAL_SCRIPT);
  await page.evaluate(
    (t) =>
      (
        window as unknown as { __dbbatTerm: { setTitle(s: string): void } }
      ).__dbbatTerm.setTitle(t),
    title,
  );
}

/** Start a fresh input line with `label` as its prompt. */
export async function prompt(page: Page, label = "demo=>"): Promise<void> {
  await page.evaluate(
    (l) =>
      (
        window as unknown as { __dbbatTerm: { prompt(s: string): void } }
      ).__dbbatTerm.prompt(l),
    label,
  );
}

/** Type `text` into the current input line, one character at a time. */
export async function typeText(
  page: Page,
  text: string,
  msPerChar = 32,
): Promise<void> {
  for (const ch of text) {
    await page.evaluate(
      (c) =>
        (
          window as unknown as { __dbbatTerm: { type(s: string): void } }
        ).__dbbatTerm.type(c),
      ch,
    );
    await page.waitForTimeout(msPerChar);
  }
}

/** Retire the caret on the current line — the "Enter" beat. */
export async function submit(page: Page): Promise<void> {
  await page.evaluate(() =>
    (
      window as unknown as { __dbbatTerm: { submit(): void } }
    ).__dbbatTerm.submit(),
  );
}

/** Append an output line. */
export async function writeLine(
  page: Page,
  text: string,
  cls: "out" | "muted" | "ok" | "warn" = "out",
): Promise<void> {
  await page.evaluate(
    ({ t, c }) =>
      (
        window as unknown as {
          __dbbatTerm: { write(s: string, c: string): void };
        }
      ).__dbbatTerm.write(t, c),
    { t: text, c: cls },
  );
}

/** Append several output lines at once. */
export async function writeLines(
  page: Page,
  lines: string[],
  cls: "out" | "muted" | "ok" | "warn" = "out",
): Promise<void> {
  for (const line of lines) {
    await writeLine(page, line, cls);
  }
}
