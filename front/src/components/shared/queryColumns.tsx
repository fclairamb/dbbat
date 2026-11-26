import { Link } from "@tanstack/react-router";
import type { Query, User } from "@/api";
import type { Column } from "@/components/shared/DataTable";
import { UnverifiedBadge } from "@/components/shared/UnverifiedBadge";
import { Badge } from "@/components/ui/badge";
import { ApprovalStatusBadge } from "@/components/shared/ApprovalStatusBadge";
import { SortHeader } from "@/components/shared/SortHeader";
import type { SortState } from "@/lib/sort";
import { formatDistanceToNow } from "date-fns";

const DEFAULT_PAGE_SIZE = 50;

interface BuildQueryColumnsOptions {
  users: User[] | undefined;
  // Accepts both the full Database shape and the limited one returned to
  // non-admin roles — only uid/name are used here.
  databases: { uid: string; name: string }[] | undefined;
  size?: number;
  /** Active sort; with `onSort` it makes Duration and Rows sortable. */
  sort?: SortState;
  onSort?: (next: SortState) => void;
}

export function buildQueryColumns({
  users,
  databases,
  size = DEFAULT_PAGE_SIZE,
  sort,
  onSort,
}: BuildQueryColumnsOptions): Column<Query>[] {
  const getUserName = (uid: string | null | undefined) =>
    uid ? users?.find((u) => u.uid === uid)?.username ?? uid : "-";
  const getDbName = (uid: string | null | undefined) =>
    uid ? databases?.find((d) => d.uid === uid)?.name ?? uid : "-";

  const title = (label: string, sortKey: string) =>
    sort && onSort ? (
      <SortHeader label={label} sortKey={sortKey} current={sort} onSort={onSort} />
    ) : (
      label
    );

  return [
    {
      key: "sql_text",
      header: "Query",
      cell: (q) => (
        <div className="max-w-md">
          <span className="font-mono text-xs break-all line-clamp-2">
            {q.sql_text}
          </span>
          <UnverifiedBadge
            chained={q.chained}
            testId={`query-unverified-badge-${q.uid}`}
          />
        </div>
      ),
    },
    {
      key: "user",
      header: "User",
      cell: (q) => (
        <span className="font-medium whitespace-nowrap">
          {getUserName(q.user_id)}
        </span>
      ),
    },
    {
      key: "database",
      header: "Database",
      cell: (q) => (
        <span className="font-mono text-sm whitespace-nowrap">
          {getDbName(q.database_id)}
        </span>
      ),
    },
    {
      key: "connection",
      header: "Connection",
      cell: (q) => (
        <Link
          to="/queries"
          search={{ connection_id: q.connection_id, before: undefined, size }}
          className="relative z-10 font-mono text-xs text-muted-foreground hover:text-foreground hover:underline whitespace-nowrap"
        >
          {q.connection_id.slice(0, 8)}
        </Link>
      ),
    },
    {
      key: "executed_at",
      header: "Executed",
      cell: (q) => (
        <span className="text-sm text-muted-foreground whitespace-nowrap">
          {formatDistanceToNow(new Date(q.executed_at), { addSuffix: true })}
        </span>
      ),
    },
    {
      key: "duration_ms",
      header: title("Duration", "duration_ms"),
      cell: (q) => (
        <span className="text-sm whitespace-nowrap">
          {q.duration_ms != null ? `${q.duration_ms.toFixed(1)}ms` : "-"}
        </span>
      ),
    },
    {
      key: "rows_affected",
      header: title("Rows", "rows_affected"),
      cell: (q) => <span className="text-sm">{q.rows_affected ?? "-"}</span>,
    },
    {
      key: "status",
      header: "Status",
      cell: (q) => {
        // The approval state wins when there is one: "pending" means the
        // statement has not run at all, and "abandoned" (the client gave up)
        // must never be mistaken for a plain OK — or for a denial.
        if (q.approval_status) {
          return <ApprovalStatusBadge status={q.approval_status} />;
        }

        return q.error ? (
          <Badge variant="destructive">Error</Badge>
        ) : (
          <Badge variant="secondary">OK</Badge>
        );
      },
    },
  ];
}
