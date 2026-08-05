/**
 * Real PostgreSQL clients — one dialling the showcase upstream directly (to
 * seed something worth looking at), one dialling *through* the dbbat proxy (to
 * produce the connections, queries, captured result rows and, for the video,
 * the live approval hold).
 *
 * Everything the showcase displays is produced here for real. Nothing in the
 * captures is a mock.
 */
import { Client } from "pg";
import {
  CONNECTOR,
  DEMO_TARGET,
  PROXY_PORT,
  SERVER_NAME,
  UPSTREAM_PORT,
} from "../config";

/** A direct connection to the throwaway upstream, bypassing dbbat. */
export function upstreamClient(): Client {
  return new Client({
    host: DEMO_TARGET.host,
    port: UPSTREAM_PORT,
    user: DEMO_TARGET.username,
    password: DEMO_TARGET.password,
    database: DEMO_TARGET.database,
    ssl: false,
  });
}

/**
 * A connection through the dbbat proxy.
 *
 * The database name is the *dbbat server row's* name, not the upstream
 * database: that is how the proxy resolves which target a session is for. The
 * credentials are dbbat user credentials (Argon2id-hashed, or a dbb_ API key).
 */
export function proxyClient(
  user = CONNECTOR.username,
  password = CONNECTOR.password,
): Client {
  return new Client({
    host: "localhost",
    port: PROXY_PORT,
    user,
    password,
    database: SERVER_NAME,
    ssl: false,
    // A held statement blocks until a human answers; never time it out.
    query_timeout: 0,
    statement_timeout: 0,
  });
}

/**
 * Give the upstream something photogenic. Demo mode's own seed is three rows
 * in `demo_data`, which makes for a thin "query results" screenshot.
 */
export async function seedUpstream(): Promise<void> {
  const client = upstreamClient();
  await client.connect();
  try {
    await client.query(`
      CREATE TABLE IF NOT EXISTS customers (
        id           SERIAL PRIMARY KEY,
        company      TEXT        NOT NULL,
        plan         TEXT        NOT NULL,
        seats        INTEGER     NOT NULL,
        mrr_eur      NUMERIC(9,2) NOT NULL,
        region       TEXT        NOT NULL,
        signed_up_at DATE        NOT NULL
      )`);
    await client.query("TRUNCATE customers RESTART IDENTITY");
    await client.query(`
      INSERT INTO customers (company, plan, seats, mrr_eur, region, signed_up_at) VALUES
        ('Northwind Logistics', 'enterprise',  420, 18400.00, 'eu-west',   '2023-02-14'),
        ('Helios Energy',       'enterprise',  310, 14250.00, 'eu-central','2023-06-02'),
        ('Meridian Bank',       'enterprise',  980, 41900.00, 'eu-west',   '2022-11-21'),
        ('Aster Health',        'business',    140,  5900.00, 'us-east',   '2024-01-09'),
        ('Blue Harbor Retail',  'business',     95,  3980.00, 'us-west',   '2024-03-27'),
        ('Kestrel Robotics',    'business',     62,  2610.00, 'eu-north',  '2024-05-15'),
        ('Vantage Media',       'starter',      18,   540.00, 'eu-west',   '2025-01-30'),
        ('Orchard Foods',       'starter',      24,   720.00, 'us-east',   '2025-04-11'),
        ('Lumen Analytics',     'business',    130,  5450.00, 'eu-central','2024-09-05'),
        ('Cobalt Freight',      'enterprise',  260, 11800.00, 'us-central','2023-08-18')`);
  } finally {
    await client.end();
  }
}

/**
 * Drive a realistic session through the proxy so the connections list, the
 * query list and the captured result rows all have something in them.
 */
export async function generateTraffic(): Promise<void> {
  const client = proxyClient();
  await client.connect();
  try {
    await client.query("SELECT current_database(), current_user");
    await client.query(
      "SELECT region, count(*) AS customers, sum(mrr_eur) AS mrr FROM customers GROUP BY region ORDER BY mrr DESC",
    );
    await client.query(
      "SELECT id, company, plan, seats, mrr_eur, region FROM customers ORDER BY mrr_eur DESC LIMIT 10",
    );
    await client.query(
      "SELECT plan, avg(seats)::int AS avg_seats FROM customers GROUP BY plan ORDER BY plan",
    );
    await client.query(
      "SELECT company, signed_up_at FROM customers WHERE signed_up_at > $1 ORDER BY signed_up_at",
      ["2024-01-01"],
    );
  } finally {
    await client.end();
  }
}

/**
 * Render a result set the way psql would, so the fake terminal pane shows the
 * real rows rather than a hand-written caption.
 */
export function formatResult(
  fields: { name: string }[],
  rows: Record<string, unknown>[],
  maxRows = 5,
): string[] {
  if (fields.length === 0) {
    return [];
  }
  const names = fields.map((f) => f.name);
  const shown = rows.slice(0, maxRows);
  const widths = names.map((name, i) =>
    Math.max(
      name.length,
      ...shown.map((r) => String(r[names[i]] ?? "").length),
      3,
    ),
  );

  const pad = (value: string, width: number) => value.padEnd(width, " ");
  const lines = [
    names.map((n, i) => ` ${pad(n, widths[i])} `).join("|"),
    widths.map((w) => "-".repeat(w + 2)).join("+"),
    ...shown.map((r) =>
      names.map((n, i) => ` ${pad(String(r[n] ?? ""), widths[i])} `).join("|"),
    ),
  ];
  if (rows.length > shown.length) {
    lines.push(`... ${rows.length - shown.length} more row(s)`);
  }
  lines.push(`(${rows.length} row${rows.length === 1 ? "" : "s"})`);
  return lines;
}
