import { useCallback, useMemo, useState } from "react";
import { Link } from "@tanstack/react-router";
import { Badge } from "@/components/ui/badge";
import { usePendingApprovals } from "@/api";
import { useEventStream } from "@/hooks/use-event-stream";

/**
 * Global "queries are parked on you right now" indicator.
 *
 * Fed by the `approvals/pending` stream topic — the only global topic in the
 * product, and the one exempt from drop-on-overflow, precisely because a
 * missed pending event means a live database connection waits on somebody who
 * never learned about it.
 *
 * The stream only tells us *that* something changed; the count itself always
 * comes from REST, which is authoritative. Users who may not read the topic
 * (the subscribe is refused server-side) simply see nothing.
 */
export function PendingApprovalsIndicator() {
  const [, setSeq] = useState(0);

  const { data: pending, refetch } = usePendingApprovals({
    // A slow poll is the backstop for a stream that never connected (a proxy
    // that strips upgrades, an old browser). Cheap: the query is backed by a
    // partial index over a tiny set.
    refetchInterval: 60_000,
  });

  const topics = useMemo(() => ["approvals/pending"], []);

  const onChange = useCallback(() => {
    setSeq((n) => n + 1);
    void refetch();
  }, [refetch]);

  useEventStream({
    topics,
    onEvent: onChange,
    onGap: onChange,
  });

  const count = pending?.length ?? 0;

  if (count === 0) {
    return null;
  }

  return (
    <Link
      to="/queries"
      search={{ connection_id: undefined, before: undefined, size: 100 }}
      className="ml-auto"
      data-testid="pending-approvals-indicator"
      title="Queries are held waiting for an approver. Nothing expires — they wait until someone decides or the client gives up."
    >
      <Badge variant="destructive">
        {count} awaiting approval
      </Badge>
    </Link>
  );
}
