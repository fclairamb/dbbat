import { useRef, useCallback } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { useQueries, type Query } from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { AdaptiveRefresh } from "@/components/shared/AdaptiveRefresh";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { X, ChevronLeft, ChevronRight } from "lucide-react";
import { formatDistanceToNow } from "date-fns";
import { useAuth } from "@/contexts/AuthContext";
import { canViewQueries } from "@/lib/permissions";
import { AccessDenied } from "@/components/shared/AccessDenied";

const DEFAULT_PAGE_SIZE = 50;
const PAGE_SIZE_OPTIONS = [25, 50, 100];

export const Route = createFileRoute("/_authenticated/queries/")({
  validateSearch: (search: Record<string, unknown>) => ({
    connection_id: search.connection_id as string | undefined,
    before: search.before as string | undefined,
    size: search.size ? Number(search.size) : DEFAULT_PAGE_SIZE,
  }),
  component: QueriesPage,
});

function QueriesPage() {
  const { user } = useAuth();
  const { connection_id, before, size } = Route.useSearch();
  const { data: queries, isLoading, refetch } = useQueries({
    connection_id,
    before,
    limit: size,
  });

  const isFirstPage = !before;

  const previousFirstUid = useRef<string | null>(null);

  const handleRefresh = useCallback(async () => {
    const result = await refetch();
    const newData = result.data;

    let hasNewData = false;
    if (newData && newData.length > 0) {
      const firstUid = newData[0].uid;
      hasNewData = previousFirstUid.current !== null &&
                   firstUid !== previousFirstUid.current;
      previousFirstUid.current = firstUid;
    }

    return { hasNewData };
  }, [refetch]);

  if (!canViewQueries(user?.roles)) {
    return <AccessDenied requiredRole="viewer" />;
  }

  const columns: Column<Query>[] = [
    {
      key: "sql_text",
      header: "Query",
      cell: (q) => (
        <div className="max-w-md">
          <span className="font-mono text-xs break-all line-clamp-2">
            {q.sql_text}
          </span>
        </div>
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
      header: "Duration",
      cell: (q) => (
        <span className="text-sm whitespace-nowrap">
          {q.duration_ms != null ? `${q.duration_ms.toFixed(1)}ms` : "-"}
        </span>
      ),
    },
    {
      key: "rows_affected",
      header: "Rows",
      cell: (q) => <span className="text-sm">{q.rows_affected ?? "-"}</span>,
    },
    {
      key: "status",
      header: "Status",
      cell: (q) =>
        q.error ? (
          <Badge variant="destructive">Error</Badge>
        ) : (
          <Badge variant="secondary">OK</Badge>
        ),
    },
  ];

  const lastUid = queries && queries.length > 0 ? queries[queries.length - 1].uid : undefined;
  const firstUid = queries && queries.length > 0 ? queries[0].uid : undefined;
  const hasMore = queries && queries.length >= size;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Queries"
        description="View executed query history"
        actions={
          <div className="flex items-center gap-4">
            {isFirstPage && (
              <AdaptiveRefresh
                onRefresh={handleRefresh}
                storageKey="dbbat.autoRefresh.queries"
              />
            )}
            {connection_id && (
              <Badge variant="secondary" className="gap-1">
                Filtered by connection
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-4 w-4 p-0 hover:bg-transparent"
                  asChild
                >
                  <Link to="/queries" search={{ before: undefined, size, connection_id: undefined }}>
                    <X className="h-3 w-3" />
                  </Link>
                </Button>
              </Badge>
            )}
          </div>
        }
      />

      <DataTable
        columns={columns}
        data={queries ?? []}
        isLoading={isLoading}
        rowKey={(q) => q.uid}
        emptyMessage="No queries recorded"
        rowHref={(q) => `/queries/${q.uid}`}
      />

      {/* Pagination */}
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <span>Rows per page:</span>
          {PAGE_SIZE_OPTIONS.map((opt) => (
            <Button
              key={opt}
              variant={opt === size ? "secondary" : "ghost"}
              size="sm"
              className="h-7 px-2"
              asChild
            >
              <Link
                to="/queries"
                search={{ connection_id, before: undefined, size: opt }}
              >
                {opt}
              </Link>
            </Button>
          ))}
        </div>

        <div className="flex items-center gap-2">
          {!isFirstPage && (
            <Button variant="outline" size="sm" asChild>
              <Link
                to="/queries"
                search={{ connection_id, before: undefined, size }}
              >
                <ChevronLeft className="h-4 w-4 mr-1" />
                Newer
              </Link>
            </Button>
          )}
          {hasMore && lastUid && (
            <Button variant="outline" size="sm" asChild>
              <Link
                to="/queries"
                search={{ connection_id, before: lastUid, size }}
              >
                Older
                <ChevronRight className="h-4 w-4 ml-1" />
              </Link>
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
