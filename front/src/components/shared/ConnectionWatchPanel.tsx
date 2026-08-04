import { useCallback, useEffect, useMemo, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  useApproveQuery,
  useDenyQuery,
  usePendingApprovals,
  type Query,
} from "@/api";
import {
  useEventStream,
  type StreamEvent,
  type StreamEventData,
} from "@/hooks/use-event-stream";

const LIVE_FEED_LIMIT = 100;

/** One row in the live feed. Keyed by query uid where we have one. */
interface FeedItem {
  key: string;
  queryUid?: string;
  sqlText: string;
  at: string;
  approvalStatus?: string;
  approvalPattern?: string;
  resolvedBy?: string;
  resolutionReason?: string;
  durationMs?: number;
  error?: string;
}

/** Elapsed time a hold has been parked. Deliberately counts *up*: nothing expires. */
function HeldFor({ since }: { since: string }) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  const seconds = Math.max(
    0,
    Math.floor((now - new Date(since).getTime()) / 1000),
  );
  const mins = Math.floor(seconds / 60);
  const label =
    mins > 0 ? `${mins}m ${seconds % 60}s` : `${seconds}s`;

  return (
    <span
      className="font-mono text-xs text-muted-foreground"
      data-testid="approval-held-for"
      title="Time this statement has been held. Nothing expires — it waits until someone decides or the client gives up."
    >
      held {label}
    </span>
  );
}

/** Renders an approval status with `abandoned` clearly distinct from `denied`. */
export function ApprovalStatusBadge({ status }: { status?: string }) {
  if (!status) return null;

  switch (status) {
    case "pending":
      return (
        <Badge variant="default" data-testid="approval-status-pending">
          Awaiting approval
        </Badge>
      );
    case "approved":
      return (
        <Badge variant="secondary" data-testid="approval-status-approved">
          Approved
        </Badge>
      );
    case "denied":
      return (
        <Badge variant="destructive" data-testid="approval-status-denied">
          Denied
        </Badge>
      );
    case "abandoned":
      // Not a rejection: the client gave up before anyone decided, and
      // nothing ran. Styled as a neutral outline so it never reads as
      // "somebody said no".
      return (
        <Badge variant="outline" data-testid="approval-status-abandoned">
          Abandoned (client gave up)
        </Badge>
      );
    default:
      return <Badge variant="outline">{status}</Badge>;
  }
}

function toFeedItem(ev: StreamEvent): FeedItem {
  const d: StreamEventData = ev.data;

  return {
    key: d.query_uid ?? `${ev.topic}:${ev.seq}`,
    queryUid: d.query_uid,
    sqlText: d.sql_text ?? "",
    at: d.executed_at ?? ev.at,
    approvalStatus: d.approval_status,
    approvalPattern: d.approval_pattern,
    resolvedBy: d.resolved_by?.display_name ?? d.resolved_by?.username,
    resolutionReason: d.resolution_reason,
    durationMs: d.duration_ms,
    error: d.error,
  };
}

/**
 * Live watch panel for one connection: queries as they arrive, with any
 * approval holds pinned to the top and actionable.
 *
 * The stream is treated as best-effort for history — every gap (`lagged`, a
 * reconnect) refetches the pending set from REST, which is the only
 * authoritative answer to "what is pending right now".
 */
export function ConnectionWatchPanel({
  connectionUid,
  defaultWatching = false,
}: {
  connectionUid: string;
  defaultWatching?: boolean;
}) {
  const [watching, setWatching] = useState(defaultWatching);
  const [feed, setFeed] = useState<FeedItem[]>([]);
  const [gap, setGap] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const {
    data: pending,
    refetch: refetchPending,
  } = usePendingApprovals({ enabled: watching });

  const topics = useMemo(
    () => (watching ? [`connection/${connectionUid}/queries`] : []),
    [connectionUid, watching],
  );

  const handleEvent = useCallback((ev: StreamEvent) => {
    setFeed((prev) => {
      const item = toFeedItem(ev);
      const without = prev.filter((p) => p.key !== item.key);
      const merged = prev.find((p) => p.key === item.key);

      // Resolution events carry only the decision, so fold them onto the
      // pending row rather than replacing its SQL text with an empty string.
      const next = merged
        ? { ...merged, ...item, sqlText: item.sqlText || merged.sqlText }
        : item;

      return [next, ...without].slice(0, LIVE_FEED_LIMIT);
    });
  }, []);

  const handleGap = useCallback(
    (reason: "reconnect" | "lagged", dropped?: number) => {
      setGap(
        reason === "lagged"
          ? `Stream fell behind and dropped ${dropped ?? 0} event(s) — the list below may have holes.`
          : "Stream reconnected — the list below may have holes.",
      );
      void refetchPending();
    },
    [refetchPending],
  );

  const { status } = useEventStream({
    topics,
    onEvent: handleEvent,
    onGap: handleGap,
    enabled: watching,
  });

  const onResolved = useCallback(() => {
    setActionError(null);
    void refetchPending();
  }, [refetchPending]);

  const approve = useApproveQuery({ onSuccess: onResolved });
  const deny = useDenyQuery({ onSuccess: onResolved });

  const pendingHere = (pending ?? []).filter(
    (q: Query) => q.connection_id === connectionUid,
  );

  return (
    <Card data-testid="connection-watch-panel">
      <CardHeader className="flex flex-row items-center justify-between space-y-0">
        <CardTitle className="flex items-center gap-2">
          Live watch
          {watching && (
            <Badge
              variant={status === "open" ? "default" : "outline"}
              data-testid="watch-stream-status"
            >
              {status === "open" ? "live" : status}
            </Badge>
          )}
        </CardTitle>
        <Button
          variant={watching ? "secondary" : "default"}
          size="sm"
          data-testid="watch-toggle"
          onClick={() => {
            setWatching((w) => !w);
            setGap(null);
          }}
        >
          {watching ? "Stop watching" : "Watch live"}
        </Button>
      </CardHeader>

      <CardContent className="space-y-4">
        {!watching && (
          <p className="text-sm text-muted-foreground">
            Start watching to stream this connection's queries as they run and
            to act on statements held for approval.
          </p>
        )}

        {watching && gap && (
          <Alert data-testid="watch-gap-alert">
            <AlertDescription>{gap}</AlertDescription>
          </Alert>
        )}

        {actionError && (
          <Alert variant="destructive" data-testid="watch-action-error">
            <AlertDescription>{actionError}</AlertDescription>
          </Alert>
        )}

        {watching && pendingHere.length > 0 && (
          <div className="space-y-2" data-testid="pending-approvals">
            <h4 className="text-sm font-semibold">
              Held for approval ({pendingHere.length})
            </h4>
            {pendingHere.map((q: Query) => (
              <div
                key={q.uid}
                className="rounded-md border border-amber-500/50 bg-amber-500/5 p-3 space-y-2"
                data-testid={`pending-approval-${q.uid}`}
              >
                <div className="flex items-center justify-between gap-2">
                  <ApprovalStatusBadge status={q.approval_status ?? "pending"} />
                  <HeldFor since={q.executed_at} />
                </div>
                <pre className="font-mono text-xs whitespace-pre-wrap break-all">
                  {q.sql_text}
                </pre>
                {q.approval_pattern && (
                  <p className="text-xs text-muted-foreground">
                    matched <code>{q.approval_pattern}</code>
                  </p>
                )}
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    data-testid={`approve-query-${q.uid}`}
                    disabled={approve.isPending}
                    onClick={() =>
                      approve.mutate(q.uid, {
                        onError: (e: Error) => setActionError(e.message),
                      })
                    }
                  >
                    Approve
                  </Button>
                  <Button
                    size="sm"
                    variant="destructive"
                    data-testid={`deny-query-${q.uid}`}
                    disabled={deny.isPending}
                    onClick={() =>
                      deny.mutate(
                        { uid: q.uid, reason: "denied from the watch panel" },
                        { onError: (e: Error) => setActionError(e.message) },
                      )
                    }
                  >
                    Deny
                  </Button>
                </div>
              </div>
            ))}
          </div>
        )}

        {watching && (
          <div className="space-y-1" data-testid="watch-feed">
            <h4 className="text-sm font-semibold">Live queries</h4>
            {feed.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                Waiting for queries…
              </p>
            ) : (
              <ul className="space-y-1">
                {feed.map((item) => (
                  <li
                    key={item.key}
                    className="rounded border px-2 py-1 text-xs font-mono break-all flex items-start justify-between gap-2"
                  >
                    <span className="flex-1">{item.sqlText}</span>
                    <span className="flex items-center gap-2 shrink-0">
                      {item.approvalStatus && (
                        <ApprovalStatusBadge status={item.approvalStatus} />
                      )}
                      {item.resolvedBy && (
                        <span className="text-muted-foreground">
                          by {item.resolvedBy}
                        </span>
                      )}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
