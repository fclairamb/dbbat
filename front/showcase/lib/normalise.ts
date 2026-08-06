/**
 * Move everything a capture renders onto a fixed timeline.
 *
 * The scenario is produced for real, at the real clock: global-setup creates
 * the server, the definition and the grant through the API, and lib/traffic.ts
 * runs actual statements through the proxy. That is what makes the stills
 * honest, and it is also what made them churn — every regeneration carried a
 * new `executed_at`, a new measured `duration_ms` and a new UUIDv7 connection
 * id, so the committed PNGs differed byte-for-byte even when nothing about the
 * UI had moved.
 *
 * So once the traffic has been generated and its session is closed, this
 * reaches into the showcase instance's *storage* database and restates the
 * timeline:
 *
 * - the run's own connection and statements are pinned to fixed offsets from
 *   SHOWCASE_EPOCH, with fixed durations;
 * - the demo-seeded history (dated from demoEpoch() in main.go, which is the
 *   start of whatever UTC day the process started on) is shifted wholesale
 *   onto the same epoch, so its relative spread — "4 hours ago", "3 days ago",
 *   "5 days ago" — is preserved exactly;
 * - every connection uid is reissued as a UUIDv7 derived from its own, now
 *   fixed, `connected_at`, because the query list renders its first eight hex
 *   characters and those are a millisecond timestamp.
 *
 * What is *not* touched: `access_grants`. The grant has to stay valid at the
 * real clock — the video project opens a fresh proxy session through it after
 * the stills are done, and a grant backdated into the past would refuse it.
 * No still renders a grant date.
 *
 * Reaching behind a running dbbat is not something to imitate elsewhere. It is
 * acceptable here for the same reason the whole suite is: the instance is a
 * throwaway that scripts/showcase.sh started thirty seconds earlier, holds
 * nothing but demo data, and is torn down when the run ends.
 */
import { Client } from "pg";
import { SHOWCASE_EPOCH, STORAGE_DSN } from "../config";

/**
 * Durations the run's own statements are recorded with, in execution order.
 *
 * Real measurements against a container on the same host come back as 0.0ms
 * and 1.0ms — true, useless to look at, and different every run. These are of
 * the order the same statements take against a real database, and they are the
 * only invented values in the capture: the SQL, the row counts and the
 * captured result rows all stay exactly as they came back.
 */
const DURATIONS_MS = [0.82, 3.41, 2.14, 1.63, 1.27];

/** How many statements lib/traffic.ts issues; the wait below expects them all. */
const EXPECTED_QUERIES = 5;

/** Seconds after the epoch the run's own session is recorded as opening. */
const SESSION_OPEN_S = 0;

/** Seconds after the epoch it is recorded as closing. */
const SESSION_CLOSE_S = EXPECTED_QUERIES + 1;

function storageClient(): Client {
  return new Client({ connectionString: STORAGE_DSN });
}

/**
 * Wait until the proxy has finished writing the session out.
 *
 * `client.end()` returns as soon as the client is done; dbbat still has to log
 * the last statement and stamp `disconnected_at`. Rewriting the row while that
 * is in flight would simply be overwritten again.
 */
async function waitForSessionClosed(
  client: Client,
  serverUid: string,
): Promise<void> {
  const deadline = Date.now() + 30_000;
  for (;;) {
    const { rows } = await client.query<{ open: string; queries: string }>(
      `SELECT count(*) FILTER (WHERE c.disconnected_at IS NULL) AS open,
              (SELECT count(*) FROM queries q
                 JOIN connections c2 ON q.connection_id = c2.uid
                WHERE c2.database_id = $1) AS queries
         FROM connections c
        WHERE c.database_id = $1`,
      [serverUid],
    );
    const open = Number(rows[0]?.open ?? 0);
    const queries = Number(rows[0]?.queries ?? 0);
    if (open === 0 && queries >= EXPECTED_QUERIES) {
      return;
    }
    if (Date.now() > deadline) {
      throw new Error(
        `showcase: the traffic session never settled (${open} still open, ` +
          `${queries}/${EXPECTED_QUERIES} statements logged)`,
      );
    }
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
}

/**
 * Shift the demo-seeded rows onto the showcase epoch.
 *
 * They are already absolute (main.go dates every one of them from demoEpoch()),
 * but demoEpoch() is the start of the current UTC day, so they move by a day
 * each midnight. Translating the whole set by one delta keeps every interval
 * between them — and between them and the run's own session — identical.
 */
async function shiftDemoHistory(
  client: Client,
  serverUid: string,
): Promise<void> {
  // demoEpoch() in main.go, expressed in SQL: the start of the current UTC day.
  const delta = `($1::timestamptz
      - (date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'))`;

  await client.query(
    `UPDATE queries q
        SET executed_at = q.executed_at + ${delta}
       FROM connections c
      WHERE q.connection_id = c.uid
        AND c.database_id <> $2`,
    [SHOWCASE_EPOCH.toISOString(), serverUid],
  );

  await client.query(
    `UPDATE connections
        SET connected_at     = connected_at + ${delta},
            last_activity_at = last_activity_at + ${delta},
            disconnected_at  = disconnected_at + ${delta}
      WHERE database_id <> $2`,
    [SHOWCASE_EPOCH.toISOString(), serverUid],
  );
}

/** Pin the session this run produced, and its statements, to the epoch. */
async function pinOwnSession(client: Client, serverUid: string): Promise<void> {
  await client.query(
    `UPDATE connections
        SET connected_at     = $1::timestamptz + make_interval(secs => $3),
            last_activity_at = $1::timestamptz + make_interval(secs => $4),
            disconnected_at  = $1::timestamptz + make_interval(secs => $4)
      WHERE database_id = $2`,
    [
      SHOWCASE_EPOCH.toISOString(),
      serverUid,
      SESSION_OPEN_S,
      SESSION_CLOSE_S,
    ],
  );

  // One statement per second, in the order they were issued (queries.uid is a
  // UUIDv7, so ascending uid *is* execution order — and it is what the query
  // list sorts on, so the rendered order does not move either).
  await client.query(
    `UPDATE queries q
        SET executed_at = $1::timestamptz + make_interval(secs => o.n),
            duration_ms = ($3::numeric[])[((o.n - 1) % array_length($3::numeric[], 1)) + 1]
       FROM (
         SELECT q2.uid, row_number() OVER (ORDER BY q2.uid) AS n
           FROM queries q2
           JOIN connections c ON q2.connection_id = c.uid
          WHERE c.database_id = $2
       ) o
      WHERE q.uid = o.uid`,
    [SHOWCASE_EPOCH.toISOString(), serverUid, DURATIONS_MS],
  );
}

/**
 * Reissue every connection uid as a UUIDv7 built from its `connected_at`.
 *
 * The query list renders `connection_id.slice(0, 8)` — the top 32 bits of a
 * UUIDv7's 48-bit millisecond timestamp. Left alone it is a fresh value on
 * every run, so the column churned even once the dates were pinned. Deriving
 * it from the (now fixed) connect time is both reproducible and truthful about
 * what a v7 uid encodes, and it keeps the sessions visually distinct: they are
 * hours or days apart, so their prefixes are too.
 *
 * `session_replication_role = replica` is what makes the primary key movable:
 * queries.connection_id references it with ON UPDATE NO ACTION, so the two
 * tables have to be restated with the constraint asleep. Transaction-local,
 * and this connection owns nothing else.
 */
async function reissueConnectionUids(client: Client): Promise<void> {
  await client.query("SET LOCAL session_replication_role = replica");

  await client.query(
    `CREATE TEMP TABLE showcase_conn_remap ON COMMIT DROP AS
       SELECT uid AS old_uid,
              (substr(stamp, 1, 8) || '-' || substr(stamp, 9, 4)
                || '-7' || lpad(to_hex(n), 3, '0')
                || '-8' || lpad(to_hex(n), 3, '0')
                || '-' || lpad(to_hex(n), 12, '0'))::uuid AS new_uid
         FROM (
           SELECT uid,
                  row_number() OVER (ORDER BY connected_at, uid) AS n,
                  lpad(to_hex((extract(epoch FROM connected_at) * 1000)::bigint),
                       12, '0') AS stamp
             FROM connections
         ) t`,
  );

  await client.query(
    `UPDATE queries q SET connection_id = m.new_uid
       FROM showcase_conn_remap m
      WHERE q.connection_id = m.old_uid`,
  );
  await client.query(
    `UPDATE connections c SET uid = m.new_uid
       FROM showcase_conn_remap m
      WHERE c.uid = m.old_uid`,
  );
}

/**
 * Restate the observability timeline so a regenerated capture is byte-identical.
 *
 * Called by global-setup once the traffic has run, before the clock pin is
 * written out — the pin is read against exactly these rows.
 */
export async function normaliseTimeline(serverUid: string): Promise<void> {
  const client = storageClient();
  await client.connect();
  try {
    await waitForSessionClosed(client, serverUid);

    await client.query("BEGIN");
    try {
      await shiftDemoHistory(client, serverUid);
      await pinOwnSession(client, serverUid);
      await reissueConnectionUids(client);
      await client.query("COMMIT");
    } catch (err) {
      await client.query("ROLLBACK");
      throw err;
    }
  } finally {
    await client.end();
  }
}
