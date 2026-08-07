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

/**
 * The showcase dbbat instance's *storage* database.
 *
 * The same throwaway container as the upstream (scripts/showcase.sh points
 * DBB_DSN at it), on the `dbbat` database rather than `demo`. lib/normalise.ts
 * is the only thing that opens it, and only to rewrite the timestamps of rows
 * this run just produced — see normaliseTimeline().
 */
export const STORAGE_DSN = env(
  "SHOWCASE_STORAGE_DSN",
  `postgres://postgres:postgres@localhost:${UPSTREAM_PORT}/dbbat`,
);

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
 * The instant every row a capture renders is dated from.
 *
 * A constant, not "the start of today" like demoEpoch() in main.go: that one
 * rolls so a long-lived public demo never ages into telling an implausible
 * story, whereas these stills are committed files whose whole point is to diff
 * cleanly. A rolling epoch would rewrite `query-results.png`'s absolute
 * "Executed …" line every midnight for no reason.
 *
 * Nothing in the run happens at this instant — the scenario is created at the
 * real clock. lib/normalise.ts rewrites it there afterwards: the run's own
 * rows are pinned to fixed offsets from this epoch, and the demo-seeded rows
 * (dated from demoEpoch(), which moves) are shifted onto it wholesale so their
 * relative spacing — "4 hours ago", "3 days ago" — survives untouched.
 *
 * Bumping it is a deliberate one-line change that regenerates both query
 * stills. Nothing forces it; the dates simply age.
 */
export const SHOWCASE_EPOCH = new Date(
  env("SHOWCASE_EPOCH", "2026-08-06T09:12:00.000Z"),
);

/**
 * How far after the epoch the browser clock is pinned.
 *
 * Under a minute, so the run's own statements render "less than a minute ago"
 * in the query list (date-fns formatDistanceToNow) — a session someone is
 * still looking at, which is the story query-list.png tells.
 */
const FIXED_TIME_OFFSET_MS = 30_000;

/**
 * The clock the browser will see, read once by global-setup.
 *
 * Pinning matters because a capture run spans a minute or two: without it, one
 * screenshot says "less than a minute ago" and the next says "2 minutes ago"
 * for the same row, and the diff churns on every regeneration.
 *
 * It is a constant because every row it is read against is one too — see
 * SHOWCASE_EPOCH and lib/normalise.ts. Override with SHOWCASE_FIXED_TIME.
 */
export function fixedTime(): Date {
  const explicit = process.env.SHOWCASE_FIXED_TIME;
  if (explicit) {
    return new Date(explicit);
  }
  return new Date(SHOWCASE_EPOCH.getTime() + FIXED_TIME_OFFSET_MS);
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
