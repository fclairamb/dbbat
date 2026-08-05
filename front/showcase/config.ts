/**
 * Every knob the showcase runner reads, in one place.
 *
 * The showcase suite deliberately runs against *its own* dbbat instance and
 * *its own* throwaway PostgreSQL container (see ../../scripts/showcase.sh):
 * demo mode drops every table on startup, so pointing it at a shared
 * development database would destroy it. That is why none of these ports
 * collide with the documented defaults (4200/5433/5001) or with the e2e
 * suite's (8080/5433/5001).
 */
import { join } from "node:path";

const REPO_ROOT = join(import.meta.dirname, "../..");

function env(name: string, fallback: string): string {
  const value = process.env[name];
  return value === undefined || value === "" ? fallback : value;
}

/** Host port the showcase dbbat instance serves the API and the UI on. */
export const API_PORT = Number(env("SHOWCASE_API_PORT", "8099"));

/** Host port the showcase dbbat instance accepts PostgreSQL clients on. */
export const PROXY_PORT = Number(env("SHOWCASE_PROXY_PORT", "5499"));

/** Host port of the throwaway upstream PostgreSQL container. */
export const UPSTREAM_PORT = Number(env("SHOWCASE_PG_PORT", "5099"));

export const BASE_URL = env(
  "SHOWCASE_BASE_URL",
  `http://localhost:${API_PORT}/app/`,
);

export const API_URL = env(
  "SHOWCASE_API_URL",
  `http://localhost:${API_PORT}/api/v1`,
);

/** Where the finished, website-ready assets land. */
export const OUT_DIR = env(
  "SHOWCASE_OUT",
  join(REPO_ROOT, "website/static/img/showcase"),
);

/** Scratch space for raw Playwright output (WebM videos, traces). */
export const WORK_DIR = env(
  "SHOWCASE_WORK",
  join(import.meta.dirname, ".artifacts"),
);

/** Demo-mode credentials — see provisionDemoData() in main.go. */
export const ADMIN = { username: "admin", password: "admin" };
export const CONNECTOR = { username: "connector", password: "connector" };

/**
 * The only upstream demo mode accepts. Config.ValidateDemoTarget rejects any
 * other user/password/host/database combination on POST /servers, so the
 * showcase upstream container has to expose exactly this.
 */
export const DEMO_TARGET = {
  host: "localhost",
  database: "demo",
  username: "demo",
  password: "demo",
};

/** Name of the server row the showcase creates and then drives traffic to. */
export const SERVER_NAME = env("SHOWCASE_SERVER_NAME", "analytics-prod");

/** Name of the grant definition backing the grant-request screenshot. */
export const DEFINITION_NAME = env(
  "SHOWCASE_DEFINITION_NAME",
  "Production read-write (4h)",
);

/**
 * RE2 pattern carried by the showcase grant. It matches the UPDATE the video
 * scenario issues, which is what parks that statement on a human.
 */
export const APPROVAL_PATTERN = "(?i)^\\s*UPDATE\\b";

/** Fixed capture geometry — see the spec: 1280x800 everywhere. */
export const VIEWPORT = { width: 1280, height: 800 };

/**
 * The wall clock the browser sees.
 *
 * Demo data is seeded at process start, so its timestamps are relative to the
 * run. Pinning Date.now() to the run's start (truncated to the minute) is what
 * makes the "x minutes ago" labels reproducible from one regeneration to the
 * next; set SHOWCASE_FIXED_TIME to pin it somewhere else.
 */
export function fixedTime(): Date {
  const explicit = process.env.SHOWCASE_FIXED_TIME;
  if (explicit) {
    return new Date(explicit);
  }
  const now = new Date();
  now.setSeconds(0, 0);
  return now;
}

/**
 * Whether to freeze the browser clock for a given capture.
 *
 * Screenshots always want it. The video does *not*: a frozen Date.now() makes
 * the "held for 3s" counter on a live approval hold render as a constant, and
 * worse, can render negative for a statement that arrived after the pin.
 * SHOWCASE_FREEZE_CLOCK=1 forces it on anyway.
 */
export function shouldFreezeClock(kind: "screenshot" | "video"): boolean {
  const explicit = process.env.SHOWCASE_FREEZE_CLOCK;
  if (explicit !== undefined && explicit !== "") {
    return explicit === "1" || explicit === "true";
  }
  return kind === "screenshot";
}
