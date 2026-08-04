import { useCallback, useEffect, useRef, useState } from "react";
import { apiBaseUrl, getStoredToken } from "@/api/client";

/**
 * Live event stream client.
 *
 * The transport is a WebSocket today. Nothing in this module's public surface
 * says so: consumers get `subscribe(topic)` plus an `onEvent` callback, and the
 * envelope below is transport-agnostic. Swapping to SSE later is a change to
 * this file's internals only — see docs/approvals.md for why that trade-off was
 * recorded rather than acted on.
 *
 * The stream is *best-effort for history and authoritative only for "what is
 * pending now"*. Any gap (a `lagged` frame, a reconnect) fires `onGap` so the
 * consumer refetches from REST rather than assuming continuity.
 */

/** Server → client event envelope. */
export interface StreamEvent {
  topic: string;
  seq: number;
  /** Event kind: query | approval_pending | approval_resolved | connection. */
  event: string;
  at: string;
  data: StreamEventData;
}

/** Payload common to every query-shaped event. */
export interface StreamEventData {
  query_uid?: string;
  connection_uid?: string;
  user_uid?: string;
  username?: string;
  database_name?: string;
  sql_text?: string;
  executed_at?: string;
  duration_ms?: number;
  rows_affected?: number;
  error?: string;
  approval_required?: boolean;
  approval_status?: string;
  approval_pattern?: string;
  resolution_reason?: string;
  resolved_at?: string;
  resolved_by?: { uid?: string; username?: string; display_name?: string };
  state?: string;
}

export type StreamStatus = "connecting" | "open" | "closed";

interface UseEventStreamOptions {
  /** Topics to subscribe to. Changing the array resubscribes. */
  topics: string[];
  /** Called for every authorized event on a subscribed topic. */
  onEvent?: (event: StreamEvent) => void;
  /**
   * Called when continuity was lost — a reconnect, or a `lagged` frame telling
   * us the server dropped events for this subscriber. Refetch from REST here.
   */
  onGap?: (reason: "reconnect" | "lagged", dropped?: number) => void;
  /** Set false to keep the socket closed (e.g. the panel is collapsed). */
  enabled?: boolean;
}

/** Reconnect backoff: quick first retry, capped so a dead server isn't hammered. */
const BACKOFF_BASE_MS = 500;
const BACKOFF_MAX_MS = 15_000;

/** Builds the ws(s):// URL for /stream from the configured API base. */
function streamUrl(): string {
  const base = new URL(apiBaseUrl, window.location.origin);
  base.pathname = `${base.pathname.replace(/\/$/, "")}/stream`;
  base.protocol = base.protocol === "https:" ? "wss:" : "ws:";
  return base.toString();
}

export function useEventStream({
  topics,
  onEvent,
  onGap,
  enabled = true,
}: UseEventStreamOptions) {
  const [status, setStatus] = useState<StreamStatus>("closed");

  const socketRef = useRef<WebSocket | null>(null);
  const retryRef = useRef(0);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const closedByUsRef = useRef(false);

  // Keep the latest callbacks/topics in refs so the reconnect logic doesn't
  // tear the socket down every render just because a closure identity changed.
  const onEventRef = useRef(onEvent);
  const onGapRef = useRef(onGap);
  const topicsRef = useRef(topics);

  useEffect(() => {
    onEventRef.current = onEvent;
    onGapRef.current = onGap;
    topicsRef.current = topics;
  });

  const topicKey = topics.join("|");

  const send = useCallback((payload: unknown) => {
    const socket = socketRef.current;
    if (socket && socket.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify(payload));
    }
  }, []);

  useEffect(() => {
    if (!enabled || topicKey === "") {
      return;
    }

    let disposed = false;

    const connect = () => {
      if (disposed) return;

      const token = getStoredToken();
      if (!token) {
        setStatus("closed");
        return;
      }

      setStatus("connecting");
      closedByUsRef.current = false;

      // Browsers can't set an Authorization header on a WebSocket handshake,
      // and a token in the query string leaks into access logs. The
      // subprotocol is a real request header that never appears in the URL.
      const socket = new WebSocket(streamUrl(), [
        `dbbat.auth.bearer.${token}`,
      ]);
      socketRef.current = socket;

      socket.onopen = () => {
        if (disposed) return;
        setStatus("open");

        const isReconnect = retryRef.current > 0;
        retryRef.current = 0;

        for (const topic of topicsRef.current) {
          socket.send(JSON.stringify({ type: "subscribe", topic }));
        }

        // Everything that happened while we were away is a gap.
        if (isReconnect) {
          onGapRef.current?.("reconnect");
        }
      };

      socket.onmessage = (raw) => {
        if (disposed) return;

        let msg: Record<string, unknown>;
        try {
          msg = JSON.parse(raw.data as string) as Record<string, unknown>;
        } catch {
          return;
        }

        if (msg.type === "lagged") {
          onGapRef.current?.("lagged", Number(msg.dropped) || 0);
          return;
        }

        if (msg.type === "event") {
          onEventRef.current?.({
            topic: String(msg.topic ?? ""),
            seq: Number(msg.seq ?? 0),
            event: String(msg.event ?? ""),
            at: String(msg.at ?? ""),
            data: (msg.data ?? {}) as StreamEventData,
          });
        }
      };

      socket.onclose = () => {
        if (disposed) return;
        setStatus("closed");

        if (closedByUsRef.current) return;

        const delay = Math.min(
          BACKOFF_MAX_MS,
          BACKOFF_BASE_MS * 2 ** retryRef.current,
        );
        retryRef.current += 1;
        timerRef.current = setTimeout(connect, delay);
      };

      socket.onerror = () => {
        // onclose always follows; the backoff lives there.
      };
    };

    connect();

    return () => {
      disposed = true;
      closedByUsRef.current = true;

      if (timerRef.current) clearTimeout(timerRef.current);
      socketRef.current?.close();
      socketRef.current = null;
      setStatus("closed");
    };
  }, [enabled, topicKey]);

  return { status, send };
}
