import { useCallback, useMemo, useState } from "react";
import { Link } from "@tanstack/react-router";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  DataTable,
  type Column,
  type RowGroup,
} from "@/components/shared/DataTable";
import {
  ApprovalStatusBadge,
  HeldFor,
} from "@/components/shared/ApprovalStatusBadge";
import {
  useApproveQuery,
  useDenyQuery,
  usePendingApprovals,
  useQueries,
  useUsers,
  type Query,
} from "@/api";
import {
  useEventStream,
  type StreamEvent,
  type StreamEventData,
} from "@/hooks/use-event-stream";
import { formatDistanceToNow } from "date-fns";

/** How many queries the REST seed asks for. */
const RECENT_QUERIES_LIMIT = 50;

/**
 * Ceiling on the rows kept once the stream has been running for a while.
 * Held statements are never subject to it — a hold that scrolled off the end
 * of a busy feed would be a hold nobody answers.
 */
const FEED_LIMIT = 100;

/** One row of the unified feed, whatever source it came from. */
interface FeedRow {
  /** Query uid where we have one; otherwise a synthetic per-event key. */
  key: string;
  queryUid?: string;
  sqlText: string;
  executedAt: string;
  durationMs?: number;
  rowsAffected?: number;
  error?: string;
  approvalStatus?: string;
  approvalPattern?: string;
  resolvedBy?: string;
  resolutionReason?: string;
}

/**
 * Keep what the incoming record actually says, fall back to what we already
 * knew.
 *
 * An absent field is "this record has nothing to say about that", never "reset
 * it". The server sends several events per query — held, resolved, completed —
 * and each carries only the fields it knows: the completion event of a
 * released statement has no approver on it, and must not erase the one the
 * resolution event delivered a moment earlier. The same rule makes a REST
 * refetch safe to fold onto a row the stream already built.
 */
function keep<T>(
  incoming: T | undefined,
  previous: T | undefined,
): T | undefined {
  return incoming === undefined || incoming === "" ? previous : incoming;
}

/**
 * Fold an incoming record onto the row already showing the same query.
 *
 * The feed is keyed by query uid, so the second (and third) event for one
 * statement updates its row instead of adding another — and "last non-empty
 * wins" makes that independent of the order they arrive in. It is also what
 * stops a REST refetch from duplicating a row the stream already delivered.
 */
function mergeFeedItem(
  previous: FeedRow | undefined,
  incoming: FeedRow,
): FeedRow {
  if (!previous) return incoming;

  return {
    key: incoming.key,
    queryUid: keep(incoming.queryUid, previous.queryUid),
    sqlText: keep(incoming.sqlText, previous.sqlText) ?? "",
    executedAt: keep(incoming.executedAt, previous.executedAt) ?? incoming.executedAt,
    durationMs: keep(incoming.durationMs, previous.durationMs),
    rowsAffected: keep(incoming.rowsAffected, previous.rowsAffected),
    error: keep(incoming.error, previous.error),
    approvalStatus: keep(incoming.approvalStatus, previous.approvalStatus),
    approvalPattern: keep(incoming.approvalPattern, previous.approvalPattern),
    resolvedBy: keep(incoming.resolvedBy, previous.resolvedBy),
    resolutionReason: keep(
      incoming.resolutionReason,
      previous.resolutionReason,
    ),
  };
}

/**
 * REST carries the approver as a bare uid; the stream carries their display
 * name. `resolveUser` bridges the two so a resolved hold reads the same
 * whether the page just loaded it or watched it happen.
 */
function fromQuery(q: Query, resolveUser: (uid: string) => string): FeedRow {
  return {
    key: q.uid,
    queryUid: q.uid,
    sqlText: q.sql_text,
    executedAt: q.executed_at,
    durationMs: q.duration_ms ?? undefined,
    rowsAffected: q.rows_affected ?? undefined,
    error: q.error ?? undefined,
    approvalStatus: q.approval_status ?? undefined,
    approvalPattern: q.approval_pattern ?? undefined,
    resolvedBy: q.resolved_by ? resolveUser(q.resolved_by) : undefined,
    resolutionReason: q.resolution_reason ?? undefined,
  };
}

function fromEvent(ev: StreamEvent): FeedRow {
  const d: StreamEventData = ev.data;

  return {
    key: d.query_uid ?? `${ev.topic}:${ev.seq}`,
    queryUid: d.query_uid,
    sqlText: d.sql_text ?? "",
    executedAt: d.executed_at ?? ev.at,
    durationMs: d.duration_ms,
    rowsAffected: d.rows_affected,
    error: d.error,
    approvalStatus: d.approval_status,
    approvalPattern: d.approval_pattern,
    resolvedBy: d.resolved_by?.display_name ?? d.resolved_by?.username,
    resolutionReason: d.resolution_reason,
  };
}

function isHeld(row: FeedRow): boolean {
  return row.approvalStatus === "pending";
}

function millis(at: string): number {
  const t = new Date(at).getTime();
  return Number.isNaN(t) ? 0 : t;
}

/**
 * Every query on one connection, in one table: history from REST, live rows
 * from `connection/<uid>/queries`, and any statement currently held for
 * approval — all keyed by query uid so the three never render the same
 * statement twice.
 *
 * On an **active** connection this is live from mount: no toggle to find, and
 * the pending set is fetched independently of the stream so a hold that
 * predates the page load is visible and actionable straight away. The toggle
 * only pauses it. A disconnected connection is plain history — no socket.
 *
 * The stream is best-effort for history: every gap (`lagged`, a reconnect)
 * refetches *both* REST sources, which are the authoritative answer to "what
 * ran" and "what is pending right now".
 */
export function ConnectionQueryFeed({
  connectionUid,
  active,
}: {
  connectionUid: string;
  /** True while the connection is open (`disconnected_at` is null). */
  active: boolean;
}) {
  // Live by default on an active connection; the toggle below only pauses.
  const [live, setLive] = useState(active);
  const [streamed, setStreamed] = useState<Record<string, FeedRow>>({});
  const [gap, setGap] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const {
    data: queries,
    isLoading: isLoadingQueries,
    refetch: refetchQueries,
  } = useQueries({ connection_id: connectionUid, limit: RECENT_QUERIES_LIMIT });

  // Independent of the stream on purpose: a statement held before this page
  // was opened has to be actionable without the visitor knowing to press
  // anything.
  const { data: pending, refetch: refetchPending } = usePendingApprovals({
    enabled: active,
  });

  const watching = active && live;

  const topics = useMemo(
    () => (watching ? [`connection/${connectionUid}/queries`] : []),
    [connectionUid, watching],
  );

  const handleEvent = useCallback((ev: StreamEvent) => {
    setStreamed((prev) => {
      const item = fromEvent(ev);
      const next = { ...prev, [item.key]: mergeFeedItem(prev[item.key], item) };

      const keys = Object.keys(next);
      if (keys.length <= FEED_LIMIT * 2) return next;

      // Bounded: a page left open for a day must not accumulate for a day.
      // Held statements are kept whatever their age.
      const trimmed: Record<string, FeedRow> = {};
      for (const row of Object.values(next)
        .sort((a, b) => millis(b.executedAt) - millis(a.executedAt))
        .slice(0, FEED_LIMIT)) {
        trimmed[row.key] = row;
      }
      for (const row of Object.values(next)) {
        if (isHeld(row)) trimmed[row.key] = row;
      }
      return trimmed;
    });
  }, []);

  const handleGap = useCallback(
    (reason: "reconnect" | "lagged", dropped?: number) => {
      setGap(
        reason === "lagged"
          ? `Stream fell behind and dropped ${dropped ?? 0} event(s) — the list below may have holes.`
          : "Stream reconnected — the list below may have holes.",
      );
      // REST is authoritative for both halves: what ran, and what is pending.
      void refetchQueries();
      void refetchPending();
    },
    [refetchPending, refetchQueries],
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
    void refetchQueries();
  }, [refetchPending, refetchQueries]);

  const approve = useApproveQuery({ onSuccess: onResolved });
  const deny = useDenyQuery({ onSuccess: onResolved });

  const { data: users } = useUsers();
  const resolveUser = useCallback(
    (uid: string) => users?.find((u) => u.uid === uid)?.username ?? uid,
    [users],
  );

  const { held, rest, all } = useMemo(() => {
    const byKey = new Map<string, FeedRow>();
    const fold = (row: FeedRow) =>
      byKey.set(row.key, mergeFeedItem(byKey.get(row.key), row));

    // Order matters: REST history first, then the pending set, then the
    // stream — each layer only overwrites the fields it actually carries.
    for (const q of queries ?? []) fold(fromQuery(q, resolveUser));
    for (const q of pending ?? []) {
      if (q.connection_id === connectionUid) fold(fromQuery(q, resolveUser));
    }
    for (const row of Object.values(streamed)) fold(row);

    const sorted = [...byKey.values()].sort((a, b) => {
      const byTime = millis(b.executedAt) - millis(a.executedAt);
      return byTime !== 0 ? byTime : b.key.localeCompare(a.key);
    });

    const heldRows = sorted.filter(isHeld);
    const restRows = sorted.filter((r) => !isHeld(r)).slice(0, FEED_LIMIT);

    return { held: heldRows, rest: restRows, all: [...heldRows, ...restRows] };
  }, [connectionUid, pending, queries, resolveUser, streamed]);

  const columns: Column<FeedRow>[] = [
    {
      key: "sql_text",
      header: "Query",
      cell: (row) => (
        <div className="max-w-md">
          <span className="font-mono text-xs break-all line-clamp-2">
            {row.sqlText}
          </span>
          {isHeld(row) && row.approvalPattern && (
            <span
              className="mt-1 block truncate text-xs text-amber-700 dark:text-amber-400"
              title={row.approvalPattern}
            >
              matched <code className="font-mono">{row.approvalPattern}</code>
            </span>
          )}
        </div>
      ),
    },
    {
      key: "executed_at",
      header: "Executed",
      cell: (row) =>
        isHeld(row) ? (
          <HeldFor since={row.executedAt} />
        ) : (
          <span className="text-sm text-muted-foreground whitespace-nowrap">
            {formatDistanceToNow(new Date(row.executedAt), { addSuffix: true })}
          </span>
        ),
    },
    {
      key: "duration_ms",
      header: "Duration",
      cell: (row) => (
        <span className="text-sm whitespace-nowrap">
          {row.durationMs != null ? `${row.durationMs.toFixed(1)}ms` : "-"}
        </span>
      ),
    },
    {
      key: "rows_affected",
      header: "Rows",
      cell: (row) => <span className="text-sm">{row.rowsAffected ?? "-"}</span>,
    },
    {
      key: "status",
      header: "Status",
      cell: (row) => (
        <span className="flex flex-wrap items-center gap-2">
          {row.approvalStatus ? (
            <ApprovalStatusBadge status={row.approvalStatus} />
          ) : row.error ? (
            <Badge variant="destructive">Error</Badge>
          ) : (
            <Badge variant="secondary">OK</Badge>
          )}
          {row.resolvedBy && (
            <span className="text-xs text-muted-foreground whitespace-nowrap">
              by {row.resolvedBy}
            </span>
          )}
        </span>
      ),
    },
  ];

  // The actions column only exists while something is held: an empty column on
  // every historical row would be pure noise.
  if (held.length > 0) {
    columns.push({
      key: "actions",
      header: "Actions",
      cell: (row) =>
        isHeld(row) && row.queryUid ? (
          // Above rowHref's full-row overlay link, or the buttons would be
          // unclickable and every press would navigate instead.
          <span className="relative z-10 flex gap-2">
            <Button
              size="sm"
              data-testid={`approve-query-${row.queryUid}`}
              disabled={approve.isPending}
              onClick={() =>
                approve.mutate(row.queryUid as string, {
                  onError: (e: Error) => setActionError(e.message),
                })
              }
            >
              Approve
            </Button>
            <Button
              size="sm"
              variant="destructive"
              data-testid={`deny-query-${row.queryUid}`}
              disabled={deny.isPending}
              onClick={() =>
                deny.mutate(
                  {
                    uid: row.queryUid as string,
                    reason: "denied from the connection feed",
                  },
                  { onError: (e: Error) => setActionError(e.message) },
                )
              }
            >
              Deny
            </Button>
          </span>
        ) : null,
    });
  }

  // Held statements get a `<tbody>` of their own so they stay grouped and
  // loud. They are already the newest rows — a parked statement blocks its
  // connection, so nothing newer can run on it — this only makes that
  // structural instead of incidental.
  const rowGroups: RowGroup<FeedRow>[] | undefined =
    held.length > 0
      ? [
          { id: "held", testId: "pending-approvals", rows: held },
          { id: "rows", rows: rest },
        ]
      : undefined;

  return (
    <Card data-testid="connection-watch-panel">
      <CardHeader className="flex flex-row items-center justify-between space-y-0">
        <CardTitle className="flex items-center gap-2">
          Queries
          {watching && (
            <Badge
              variant={status === "open" ? "default" : "outline"}
              data-testid="watch-stream-status"
            >
              {status === "open" ? "live" : status}
            </Badge>
          )}
        </CardTitle>
        {active && (
          <Button
            variant={live ? "secondary" : "default"}
            size="sm"
            data-testid="watch-toggle"
            title={
              live
                ? "Pause the live stream. The queries below stay; they just stop updating."
                : "Resume streaming this connection's queries as they run."
            }
            onClick={() => {
              setLive((w) => !w);
              setGap(null);
            }}
          >
            {live ? "Stop watching" : "Watch live"}
          </Button>
        )}
      </CardHeader>

      <CardContent className="space-y-4">
        {active && !live && (
          <p className="text-sm text-muted-foreground">
            Live updates are paused — this connection is still open, so new
            queries and approval holds will not appear until you resume.
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

        <div data-testid="watch-feed">
          <DataTable
            columns={columns}
            data={all}
            rowGroups={rowGroups}
            isLoading={isLoadingQueries && all.length === 0}
            rowKey={(row) => row.key}
            rowTestId={(row) =>
              isHeld(row) && row.queryUid
                ? `pending-approval-${row.queryUid}`
                : "watch-feed-item"
            }
            rowClassName={(row) =>
              isHeld(row)
                ? "border-l-4 border-l-amber-500 bg-amber-500/10 hover:bg-amber-500/15"
                : undefined
            }
            rowHref={(row) =>
              row.queryUid ? `/queries/${row.queryUid}` : undefined
            }
            emptyMessage={
              watching
                ? "No queries yet — waiting for this connection to run one"
                : "No queries recorded for this connection"
            }
          />
        </div>

        {queries && queries.length >= RECENT_QUERIES_LIMIT && (
          <div className="text-sm text-muted-foreground">
            Showing the most recent {RECENT_QUERIES_LIMIT} queries.{" "}
            <Link
              to="/queries"
              search={{
                connection_id: connectionUid,
                before: undefined,
                size: 100,
              }}
              className="underline hover:text-foreground"
            >
              View all queries for this connection
            </Link>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
