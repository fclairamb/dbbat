/**
 * A tiny REST client for the showcase runner.
 *
 * The showcase seeds its scenario state (a server row, a grant definition, a
 * grant carrying an approval pattern) over the API rather than by clicking
 * through the UI: the screenshots should show the *finished* product, and the
 * seeding steps have nothing to do with what we are capturing.
 */
import { API_URL } from "../config";

export interface Identified {
  uid: string;
}

export interface ServerRow extends Identified {
  name: string;
  host: string;
  port: number;
}

export interface UserRow extends Identified {
  username: string;
}

export interface ConnectionRow extends Identified {
  user_id: string;
  database_id: string;
  /** Null/absent while the session is still live. */
  disconnected_at?: string | null;
}

/** Authenticated REST client bound to one dbbat user. */
export class ShowcaseApi {
  private token = "";

  constructor(private readonly baseUrl: string = API_URL) {}

  async login(username: string, password: string): Promise<void> {
    const res = await fetch(`${this.baseUrl}/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    if (!res.ok) {
      throw new Error(
        `showcase: login as ${username} failed: ${res.status} ${await res.text()}`,
      );
    }
    const body = (await res.json()) as { token: string };
    this.token = body.token;
  }

  /** The bearer token, for seeding it into the browser's localStorage. */
  get sessionToken(): string {
    return this.token;
  }

  async request<T>(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<T> {
    const res = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers: {
        "Content-Type": "application/json",
        ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    if (!res.ok) {
      throw new Error(
        `showcase: ${method} ${path} failed: ${res.status} ${await res.text()}`,
      );
    }
    if (res.status === 204) {
      return undefined as T;
    }
    return (await res.json()) as T;
  }

  get<T>(path: string): Promise<T> {
    return this.request<T>("GET", path);
  }

  post<T>(path: string, body: unknown): Promise<T> {
    return this.request<T>("POST", path, body);
  }
}

/**
 * Poll for the proxy session a `pg` client has just opened.
 *
 * Scoped to the showcase's own server row and user, and to a session that has
 * not disconnected — global-setup's traffic run left closed connections behind,
 * and picking one of those up would produce a capture of a dead page.
 */
export async function waitForLiveConnection(
  api: ShowcaseApi,
  serverUid: string,
  userUid: string,
): Promise<string> {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const body = await api.get<{ connections?: ConnectionRow[] }>(
      `/connections?database_id=${serverUid}&user_id=${userUid}`,
    );
    const live = (body.connections ?? []).find(
      (c) => c.disconnected_at === null || c.disconnected_at === undefined,
    );
    if (live) {
      return live.uid;
    }
    await new Promise((resolve) => setTimeout(resolve, 300));
  }
  throw new Error("showcase: the proxy session never showed up in /connections");
}

/**
 * Wait until the showcase dbbat answers. The orchestrating shell script waits
 * too, but the suite must be runnable on its own against an instance someone
 * started by hand.
 */
export async function waitForServer(
  url: string,
  timeoutMs = 60_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastError = "never reached";
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok || res.status === 404) {
        return;
      }
      lastError = `status ${res.status}`;
    } catch (err) {
      lastError = String(err);
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error(`showcase: ${url} never became ready (${lastError})`);
}
