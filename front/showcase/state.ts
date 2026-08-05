/**
 * The handful of UIDs global-setup creates, handed to the specs through a file
 * so each Playwright worker (and a re-run of a single spec) can pick them up.
 */
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { WORK_DIR } from "./config";

export interface ShowcaseState {
  /** Server row the showcase drives traffic to. */
  serverUid: string;
  /** Grant definition backing the grant-request screenshot. */
  definitionUid: string;
  /** dbbat user the proxy sessions authenticate as. */
  connectorUid: string;
  /** Grant carrying the approval pattern. */
  grantUid: string;
  /** Wall clock the browser is pinned to, ISO-8601. */
  fixedTime: string;
}

const STATE_FILE = join(WORK_DIR, "state.json");

export function writeState(state: ShowcaseState): void {
  mkdirSync(dirname(STATE_FILE), { recursive: true });
  writeFileSync(STATE_FILE, `${JSON.stringify(state, null, 2)}\n`);
}

export function readState(): ShowcaseState {
  try {
    return JSON.parse(readFileSync(STATE_FILE, "utf8")) as ShowcaseState;
  } catch (err) {
    throw new Error(
      `showcase: no scenario state at ${STATE_FILE} — run the suite through its own config so global-setup executes (${String(err)})`,
    );
  }
}
