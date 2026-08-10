/**
 * Showcase global setup.
 *
 * Unlike the e2e suite's global-setup, this one does NOT build or start
 * anything: ../../scripts/showcase.sh owns the lifecycle of the throwaway
 * upstream container and of the demo-mode dbbat instance, precisely so that
 * nothing here can reach for `docker compose` and take a shared development
 * database down with it.
 *
 * What it does own is the scenario: a server row, a grant definition, a grant
 * carrying the approval pattern, a seeded upstream table, and enough real
 * proxy traffic that the observability pages have something to show.
 */
import { mkdirSync } from "node:fs";
import {
  API_URL,
  ADMIN,
  APPROVAL_PATTERN,
  BASE_URL,
  CONNECTOR,
  DEFINITION_NAME,
  DEMO_TARGET,
  OUT_DIR,
  SERVER_NAME,
  UPSTREAM_PORT,
  WORK_DIR,
  fixedTime,
} from "./config";
import {
  ShowcaseApi,
  waitForServer,
  type Identified,
  type ServerRow,
  type UserRow,
} from "./lib/api";
import { normaliseTimeline } from "./lib/normalise";
import { generateTraffic, seedUpstream } from "./lib/traffic";
import { writeState } from "./state";

async function ensureServer(api: ShowcaseApi): Promise<string> {
  const existing = await api.get<{ databases?: ServerRow[] }>("/servers");
  const match = (existing.databases ?? []).find((s) => s.name === SERVER_NAME);
  if (match) {
    return match.uid;
  }

  // Demo mode only accepts one upstream (Config.ValidateDemoTarget), so the
  // credentials are fixed; the port is not validated, which is what lets the
  // showcase point at its own container instead of :5432.
  const created = await api.post<ServerRow>("/servers", {
    name: SERVER_NAME,
    description: "Customer analytics replica",
    protocol: "postgresql",
    host: DEMO_TARGET.host,
    port: UPSTREAM_PORT,
    database_name: DEMO_TARGET.database,
    username: DEMO_TARGET.username,
    password: DEMO_TARGET.password,
    ssl_mode: "disable",
    listable: true,
  });
  return created.uid;
}

async function ensureDefinition(api: ShowcaseApi): Promise<string> {
  const slug = "production-read-write-4h";
  const existing = await api.get<{
    grant_definitions?: (Identified & { slug: string })[];
  }>("/grant-definitions");
  const match = (existing.grant_definitions ?? []).find((d) => d.slug === slug);
  if (match) {
    return match.uid;
  }

  const created = await api.post<Identified>("/grant-definitions", {
    name: DEFINITION_NAME,
    slug,
    description:
      "Time-boxed write access to a production replica. UPDATE and DELETE are held for a second pair of eyes.",
    duration_seconds: 4 * 3600,
    controls: [],
    auto_approve: false,
    approval_patterns: [APPROVAL_PATTERN],
  });
  return created.uid;
}

async function connectorUid(api: ShowcaseApi): Promise<string> {
  const users = await api.get<{ users?: UserRow[] }>("/users");
  const match = (users.users ?? []).find(
    (u) => u.username === CONNECTOR.username,
  );
  if (!match) {
    throw new Error(
      `showcase: no '${CONNECTOR.username}' user — is the instance really in DBB_RUN_MODE=demo?`,
    );
  }
  return match.uid;
}

/**
 * Issues the grant by instantiating the definition above. A grant carries no
 * shape of its own — the controls, the approval patterns and the window's
 * length all come from the definition.
 */
async function ensureGrant(
  api: ShowcaseApi,
  definitionUid: string,
  userUid: string,
  serverUid: string,
): Promise<string> {
  const now = new Date();
  const created = await api.post<Identified>("/grants", {
    grant_definition_id: definitionUid,
    user_id: userUid,
    database_id: serverUid,
    starts_at: new Date(now.getTime() - 60_000).toISOString(),
  });
  return created.uid;
}

/**
 * Mint a `dbb_` API key owned by the connector, for the MCP scenario.
 *
 * `POST /keys` always creates a key for the *caller*, and it is gated on
 * requireWebSessionOrBasicAuth — an API key cannot mint another. So this logs
 * in as the connector rather than reusing the admin client above: the agent has
 * to act as the user whose grant carries the approval pattern, or its statement
 * would never be held.
 *
 * The plaintext key is shown exactly once, at creation. Demo mode drops every
 * table on startup and the instance is torn down with the run, so a fresh key
 * per run is both necessary and free.
 */
async function mintConnectorKey(): Promise<string> {
  const asConnector = new ShowcaseApi(API_URL);
  await asConnector.login(CONNECTOR.username, CONNECTOR.password);

  const created = await asConnector.post<{ key: string }>("/keys", {
    name: "claude-code (showcase)",
  });
  if (!created.key?.startsWith("dbb_")) {
    throw new Error("showcase: POST /keys did not return a dbb_ key");
  }

  return created.key;
}

export default async function globalSetup(): Promise<void> {
  mkdirSync(OUT_DIR, { recursive: true });
  mkdirSync(WORK_DIR, { recursive: true });

  console.log(`[showcase] waiting for ${BASE_URL}`);
  await waitForServer(BASE_URL);

  const api = new ShowcaseApi(API_URL);
  await api.login(ADMIN.username, ADMIN.password);

  console.log("[showcase] seeding the upstream database");
  await seedUpstream();

  console.log("[showcase] creating the scenario (server, definition, grant)");
  const serverUid = await ensureServer(api);
  const definitionUid = await ensureDefinition(api);
  const userUid = await connectorUid(api);
  const grantUid = await ensureGrant(api, definitionUid, userUid, serverUid);

  console.log("[showcase] minting the connector's MCP API key");
  const connectorApiKey = await mintConnectorKey();

  console.log("[showcase] generating proxy traffic");
  await generateTraffic();

  // The traffic just ran at the real clock, and the demo history is dated from
  // whatever UTC day this process started on. Restate both onto the fixed
  // showcase epoch, so the pin below — a constant — is read against rows that
  // are constants too, and a regenerated capture diffs cleanly.
  console.log("[showcase] normalising the observability timeline");
  await normaliseTimeline(serverUid);

  writeState({
    serverUid,
    definitionUid,
    connectorUid: userUid,
    grantUid,
    connectorApiKey,
    fixedTime: fixedTime().toISOString(),
  });

  console.log("[showcase] scenario ready");
}
