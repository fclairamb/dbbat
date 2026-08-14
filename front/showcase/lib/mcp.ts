/**
 * A minimal MCP client — plain `fetch`, no SDK.
 *
 * The showcase needs exactly two tool calls (`query`, then `await_approval`),
 * and dbbat's endpoint is stateless Streamable HTTP: no `initialize`
 * handshake to complete, no `Mcp-Session-Id` to carry, one POST per call (see
 * internal/mcp/server.go and docs/mcp.md). Pulling in an MCP client library to
 * send two JSON-RPC envelopes would add a dependency to the *website's build
 * tooling* for no fidelity gained — the wire format is what an agent sends, and
 * that is what this sends.
 *
 * Authentication is a `dbb_` API key, minted for the connector by
 * ../global-setup.ts. The endpoint refuses Basic Auth and session tokens: the
 * key is also the password the loopback protocol client authenticates to the
 * proxy with.
 */
import { API_URL } from "../config";

/** The `query` / `await_approval` result — internal/mcp.QueryOutput. */
export interface McpQueryOutput {
  /** `ok`, `approval_pending` or `still_running`. */
  status: string;
  database: string;
  columns?: string[];
  rows?: Record<string, unknown>[];
  row_count: number;
  truncated: boolean;
  max_rows: number;
  rows_affected?: number;
  duration_ms: number;
  execution_id?: string;
  /** The dbbat query a human is being shown — the row in the watch panel. */
  query_uid?: string;
  approval_pattern?: string;
  message?: string;
}

interface JsonRpcResponse {
  error?: { code: number; message: string };
  result?: {
    isError?: boolean;
    structuredContent?: unknown;
    content?: { type: string; text?: string }[];
  };
}

/**
 * Pull the single JSON-RPC envelope out of an SSE body.
 *
 * The handler answers `text/event-stream` (the SDK's default — `JSONResponse`
 * is not set), so even a one-shot call arrives as one `data:` frame.
 */
function parseSSE(body: string): JsonRpcResponse {
  for (const line of body.split("\n")) {
    if (line.startsWith("data:")) {
      return JSON.parse(line.slice(5).trim()) as JsonRpcResponse;
    }
  }
  throw new Error(`showcase: no SSE data frame in the MCP response: ${body}`);
}

/** One `dbb_` key's view of the MCP endpoint. */
export class McpClient {
  constructor(
    private readonly apiKey: string,
    private readonly endpoint = `${API_URL}/mcp`,
  ) {}

  /**
   * Call a tool and return its structured result.
   *
   * `signal` is what lets the clip fire a call and get on with the browser
   * half; nothing here polls or retries, because the tools do not need it —
   * `await_approval` blocks server-side and always answers.
   */
  async call<T>(
    name: string,
    args: Record<string, unknown>,
    signal?: AbortSignal,
  ): Promise<T> {
    const res = await fetch(this.endpoint, {
      method: "POST",
      signal,
      headers: {
        Authorization: `Bearer ${this.apiKey}`,
        "Content-Type": "application/json",
        // The spec requires both, and the SDK enforces it.
        Accept: "application/json, text/event-stream",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: Date.now(),
        method: "tools/call",
        params: { name, arguments: args },
      }),
    });

    if (!res.ok) {
      throw new Error(
        `showcase: MCP ${name} failed: ${res.status} ${await res.text()}`,
      );
    }

    const envelope = parseSSE(await res.text());
    if (envelope.error) {
      throw new Error(
        `showcase: MCP ${name} returned an error: ${envelope.error.message}`,
      );
    }

    const result = envelope.result;
    if (!result || result.structuredContent === undefined) {
      // A tool that fails answers with isError and a text block — a denied
      // statement, an expired grant, an unsupported protocol.
      const text = result?.content?.map((c) => c.text ?? "").join("\n") ?? "";
      throw new Error(`showcase: MCP ${name} returned no result: ${text}`);
    }

    return result.structuredContent as T;
  }

  query(
    database: string,
    sql: string,
    signal?: AbortSignal,
  ): Promise<McpQueryOutput> {
    return this.call<McpQueryOutput>("query", { database, sql }, signal);
  }

  awaitApproval(
    executionId: string,
    timeoutSeconds: number,
    signal?: AbortSignal,
  ): Promise<McpQueryOutput> {
    return this.call<McpQueryOutput>(
      "await_approval",
      { execution_id: executionId, timeout_seconds: timeoutSeconds },
      signal,
    );
  }
}
