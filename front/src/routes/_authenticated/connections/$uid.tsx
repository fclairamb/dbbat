import { createFileRoute, Link } from "@tanstack/react-router";
import {
  useConnection,
  useUsers,
  useDatabases,
  useQueries,
  type Query,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { format, formatDistanceToNow } from "date-fns";
import { useBreadcrumbTitle } from "@/contexts/BreadcrumbContext";

const RECENT_QUERIES_LIMIT = 50;

export const Route = createFileRoute("/_authenticated/connections/$uid")({
  component: ConnectionDetailPage,
});

function ConnectionDetailPage() {
  const { uid } = Route.useParams();
  const { data: connection, isLoading: isLoadingConnection } =
    useConnection(uid);
  const { data: users } = useUsers();
  const { data: databases } = useDatabases();

  const getUserName = (userId: string) =>
    users?.find((u) => u.uid === userId)?.username ?? userId;
  const getDbName = (databaseId: string) =>
    databases?.find((d) => d.uid === databaseId)?.name ?? databaseId;

  // Publish a "Connections › username @ database" breadcrumb once the
  // connection (and the users/databases needed to resolve it) has loaded.
  useBreadcrumbTitle(
    `/connections/${uid}`,
    connection
      ? `${getUserName(connection.user_id)} @ ${getDbName(connection.database_id)}`
      : undefined,
  );

  const { data: queries, isLoading: isLoadingQueries } = useQueries(
    { connection_id: uid, limit: RECENT_QUERIES_LIMIT },
    { enabled: !!connection },
  );

  if (isLoadingConnection) {
    return <PageLoader />;
  }

  if (!connection) {
    return (
      <div className="text-center text-muted-foreground py-12">
        Connection not found
      </div>
    );
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

  const end = connection.disconnected_at
    ? new Date(connection.disconnected_at)
    : new Date();
  const start = new Date(connection.connected_at);
  const durationMs = end.getTime() - start.getTime();
  const durationLabel = formatDuration(durationMs);

  return (
    <div className="space-y-6">
      <PageHeader
        title={`${getUserName(connection.user_id)} @ ${getDbName(connection.database_id)}`}
        description={`Connected ${format(new Date(connection.connected_at), "PPpp")}`}
        actions={
          connection.disconnected_at ? (
            <Badge variant="secondary">Disconnected</Badge>
          ) : (
            <Badge variant="default">Active</Badge>
          )
        }
      />

      <Card>
        <CardHeader>
          <CardTitle>Connection Information</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid gap-4 sm:grid-cols-2 md:grid-cols-3">
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                User
              </dt>
              <dd className="font-medium">{getUserName(connection.user_id)}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Database
              </dt>
              <dd className="font-mono text-sm">
                {getDbName(connection.database_id)}
              </dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Source IP
              </dt>
              <dd className="font-mono text-sm">{connection.source_ip}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Connected
              </dt>
              <dd>{format(new Date(connection.connected_at), "PPpp")}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Disconnected
              </dt>
              <dd>
                {connection.disconnected_at
                  ? format(new Date(connection.disconnected_at), "PPpp")
                  : "-"}
              </dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Duration
              </dt>
              <dd>{durationLabel}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Queries
              </dt>
              <dd>{connection.queries}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Data Transferred
              </dt>
              <dd>{formatBytes(connection.bytes_transferred)}</dd>
            </div>
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Queries</CardTitle>
        </CardHeader>
        <CardContent>
          <DataTable
            columns={columns}
            data={queries ?? []}
            isLoading={isLoadingQueries}
            rowKey={(q) => q.uid}
            emptyMessage="No queries recorded for this connection"
            rowHref={(q) => `/queries/${q.uid}`}
          />
          {queries && queries.length >= RECENT_QUERIES_LIMIT && (
            <div className="mt-4 text-sm text-muted-foreground">
              Showing the most recent {RECENT_QUERIES_LIMIT} queries.{" "}
              <Link
                to="/queries"
                search={{ connection_id: uid, before: undefined, size: 100 }}
                className="underline hover:text-foreground"
              >
                View all queries for this connection
              </Link>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function formatDuration(durationMs: number): string {
  const seconds = Math.floor(durationMs / 1000);
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);

  if (hours > 0) {
    return `${hours}h ${minutes % 60}m`;
  }
  if (minutes > 0) {
    return `${minutes}m ${seconds % 60}s`;
  }
  return `${seconds}s`;
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(1))} ${sizes[i]}`;
}
