import { useRef, useCallback } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useQueries, type Query } from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { AdaptiveRefresh } from "@/components/shared/AdaptiveRefresh";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { X } from "lucide-react";
import { formatDistanceToNow } from "date-fns";
import { useAuth } from "@/contexts/AuthContext";
import { canViewQueries } from "@/lib/permissions";
import { AccessDenied } from "@/components/shared/AccessDenied";

export const Route = createFileRoute("/_authenticated/queries/")({
  validateSearch: (search: Record<string, unknown>) => ({
    connection_id: search.connection_id as string | undefined,
  }),
  component: QueriesPage,
});

function QueriesPage() {
  const navigate = useNavigate();
  const { user } = useAuth();
  const { connection_id } = Route.useSearch();
  const { data: queries, isLoading, refetch } = useQueries({
    connection_id,
    limit: 100,
  });

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

  // Check if user has viewer role
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

  const clearFilter = () => {
    navigate({ to: "/queries", search: {} });
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="Queries"
        description="View executed query history"
        actions={
          <div className="flex items-center gap-4">
            <AdaptiveRefresh
              onRefresh={handleRefresh}
              storageKey="dbbat.autoRefresh.queries"
            />
            {connection_id && (
              <Badge variant="secondary" className="gap-1">
                Filtered by connection
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-4 w-4 p-0 hover:bg-transparent"
                  onClick={clearFilter}
                >
                  <X className="h-3 w-3" />
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
        onRowClick={(q) => navigate({ to: "/queries/$uid", params: { uid: q.uid } })}
      />
    </div>
  );
}
