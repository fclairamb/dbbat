/**
 * A drawn mouse pointer.
 *
 * Playwright's video recorder captures the page, not the compositor, so the
 * real cursor is never in the frame — a click in a recorded run looks like the
 * UI reacting to nothing. This injects a DOM pointer that follows the synthetic
 * mousemove events Playwright dispatches, plus a click ripple.
 */
import type { Locator, Page } from "@playwright/test";

const CURSOR_SCRIPT = `
(() => {
  if (window.__dbbatShowcaseCursor) return;
  window.__dbbatShowcaseCursor = true;

  const install = () => {
    if (!document.body || document.getElementById("dbbat-showcase-cursor")) return;

    const style = document.createElement("style");
    style.textContent = \`
      #dbbat-showcase-cursor {
        position: fixed;
        top: 0; left: 0;
        width: 18px; height: 18px;
        margin: -9px 0 0 -9px;
        border-radius: 50%;
        background: rgba(15, 23, 42, 0.75);
        box-shadow: 0 0 0 2px rgba(255, 255, 255, 0.9), 0 2px 8px rgba(0, 0, 0, 0.35);
        pointer-events: none;
        z-index: 2147483647;
        transition: transform 60ms linear;
        will-change: transform;
      }
      #dbbat-showcase-cursor.is-down { background: rgba(37, 99, 235, 0.9); }
      .dbbat-showcase-ripple {
        position: fixed;
        width: 12px; height: 12px;
        margin: -6px 0 0 -6px;
        border-radius: 50%;
        border: 2px solid rgba(37, 99, 235, 0.9);
        pointer-events: none;
        z-index: 2147483646;
        animation: dbbat-showcase-ripple 420ms ease-out forwards;
      }
      @keyframes dbbat-showcase-ripple {
        to { transform: scale(4); opacity: 0; }
      }
    \`;
    document.head.appendChild(style);

    const dot = document.createElement("div");
    dot.id = "dbbat-showcase-cursor";
    dot.style.transform = "translate(-100px, -100px)";
    document.body.appendChild(dot);

    document.addEventListener("mousemove", (ev) => {
      dot.style.transform = "translate(" + ev.clientX + "px, " + ev.clientY + "px)";
    }, true);

    document.addEventListener("mousedown", (ev) => {
      dot.classList.add("is-down");
      const ripple = document.createElement("div");
      ripple.className = "dbbat-showcase-ripple";
      ripple.style.left = ev.clientX + "px";
      ripple.style.top = ev.clientY + "px";
      document.body.appendChild(ripple);
      setTimeout(() => ripple.remove(), 500);
    }, true);

    document.addEventListener("mouseup", () => {
      dot.classList.remove("is-down");
    }, true);
  };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", install);
  } else {
    install();
  }
})();
`;

/** Draw the pointer on this page and on every page it navigates to. */
export async function installFakeCursor(page: Page): Promise<void> {
  await page.addInitScript(CURSOR_SCRIPT);
}

/**
 * Glide the pointer to the centre of `target`. `steps` is what makes the move
 * readable on video — a single jump reads as a teleport.
 */
export async function moveCursorTo(
  page: Page,
  target: Locator,
  steps = 24,
): Promise<void> {
  // `block: "center"` rather than scrollIntoViewIfNeeded: the fake terminal
  // pane occupies the bottom of the viewport, and "if needed" happily leaves
  // an element sitting underneath it.
  await target.evaluate((el) =>
    el.scrollIntoView({ block: "center", inline: "nearest" }),
  );
  const box = await target.boundingBox();
  if (!box) {
    throw new Error("showcase: cannot move the cursor to an invisible element");
  }
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, {
    steps,
  });
}

/**
 * Glide to `target`, pause a beat so the viewer registers it, then click.
 *
 * The click itself goes through the locator rather than through raw mouse
 * events: a raw `mouse.down()` has no actionability check, so an overlay
 * sitting on top of the button (the terminal pane, a toast) would silently
 * swallow it and the video would show a click that did nothing.
 */
export async function cursorClick(
  page: Page,
  target: Locator,
  settleMs = 350,
): Promise<void> {
  await moveCursorTo(page, target);
  await page.waitForTimeout(settleMs);
  await target.click({ timeout: 15_000 });
}
