import { useRef, useCallback } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { useConnections, useUsers, useDatabases, type Connection } from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { AdaptiveRefresh } from "@/components/shared/AdaptiveRefresh";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { Label } from "@/components/ui/label";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { formatDistanceToNow } from "date-fns";

const DEFAULT_PAGE_SIZE = 50;
const PAGE_SIZE_OPTIONS = [25, 50, 100];

export const Route = createFileRoute("/_authenticated/connections/")({
  validateSearch: (search: Record<string, unknown>) => ({
    before: search.before as string | undefined,
    size: search.size ? Number(search.size) : DEFAULT_PAGE_SIZE,
    active: search.active === true || search.active === "true" ? true : undefined,
  }),
  component: ConnectionsPage,
});

function ConnectionsPage() {
  const { before, size, active } = Route.useSearch();
  const { data: connections, isLoading, refetch } = useConnections({
    before,
    limit: size,
  });
  const { data: users } = useUsers();
  const { data: databases } = useDatabases();

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

  const getUserName = (uid: string) =>
    users?.find((u) => u.uid === uid)?.username ?? uid;
  const getDbName = (uid: string) =>
    databases?.find((d) => d.uid === uid)?.name ?? uid;

  const filteredConnections = active
    ? connections?.filter((c) => !c.disconnected_at)
    : connections;

  const columns: Column<Connection>[] = [
    {
      key: "user",
      header: "User",
      cell: (c) => (
        <span className="font-medium">{getUserName(c.user_id)}</span>
      ),
    },
    {
      key: "database",
      header: "Database",
      cell: (c) => (
        <span className="font-mono text-sm">{getDbName(c.database_id)}</span>
      ),
    },
    {
      key: "source_ip",
      header: "Source IP",
      cell: (c) => <span className="font-mono text-sm">{c.source_ip}</span>,
    },
    {
      key: "connected_at",
      header: "Connected",
      cell: (c) => (
        <span className="text-sm text-muted-foreground">
          {formatDistanceToNow(new Date(c.connected_at), { addSuffix: true })}
        </span>
      ),
    },
    {
      key: "status",
      header: "Status",
      cell: (c) =>
        c.disconnected_at ? (
          <Badge variant="secondary">Disconnected</Badge>
        ) : (
          <Badge variant="default">Active</Badge>
        ),
    },
    {
      key: "duration",
      header: "Duration",
      cell: (c) => {
        const end = c.disconnected_at
          ? new Date(c.disconnected_at)
          : new Date();
        const start = new Date(c.connected_at);
        const durationMs = end.getTime() - start.getTime();
        const seconds = Math.floor(durationMs / 1000);
        const minutes = Math.floor(seconds / 60);
        const hours = Math.floor(minutes / 60);

        if (hours > 0) {
          return <span>{hours}h {minutes % 60}m</span>;
        }
        if (minutes > 0) {
          return <span>{minutes}m {seconds % 60}s</span>;
        }
        return <span>{seconds}s</span>;
      },
    },
    {
      key: "queries",
      header: "Queries",
      cell: (c) => <span>{c.queries}</span>,
    },
    {
      key: "bytes",
      header: "Data",
      cell: (c) => <span>{formatBytes(c.bytes_transferred)}</span>,
    },
  ];

  const lastUid = connections && connections.length > 0 ? connections[connections.length - 1].uid : undefined;
  const hasMore = connections && connections.length >= size;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Connections"
        description="View proxy connection history"
        actions={
          <div className="flex items-center gap-4">
            {isFirstPage && (
              <AdaptiveRefresh
                onRefresh={handleRefresh}
                storageKey="dbbat.autoRefresh.connections"
              />
            )}
            <div className="flex items-center gap-2">
              <Switch
                id="showActive"
                checked={!!active}
                onCheckedChange={(checked) => {
                  // Navigate to update the search param
                  window.location.search = new URLSearchParams({
                    ...(before ? { before } : {}),
                    size: String(size),
                    ...(checked ? { active: "true" } : {}),
                  }).toString();
                }}
              />
              <Label htmlFor="showActive">Active only</Label>
            </div>
          </div>
        }
      />

      <DataTable
        columns={columns}
        data={filteredConnections ?? []}
        isLoading={isLoading}
        rowKey={(c) => c.uid}
        emptyMessage="No connections found"
        rowHref={(c) => `/queries?connection_id=${c.uid}`}
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
                to="/connections"
                search={{ before: undefined, size: opt, active }}
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
                to="/connections"
                search={{ before: undefined, size, active }}
              >
                <ChevronLeft className="h-4 w-4 mr-1" />
                Newer
              </Link>
            </Button>
          )}
          {hasMore && lastUid && (
            <Button variant="outline" size="sm" asChild>
              <Link
                to="/connections"
                search={{ before: lastUid, size, active }}
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

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(1))} ${sizes[i]}`;
}
