import { useEffect, useState } from "react";
import { Badge } from "@/components/ui/badge";

/**
 * Elapsed time a hold has been parked. Deliberately counts *up*: nothing
 * expires — see docs/approvals.md ("There is no approval timeout").
 */
export function HeldFor({ since }: { since: string }) {
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
  const label = mins > 0 ? `${mins}m ${seconds % 60}s` : `${seconds}s`;

  return (
    <span
      className="font-mono text-xs text-amber-700 dark:text-amber-400"
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
