import { useState, useRef, useCallback } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useConnections, useUsers, useDatabases, type Connection } from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { AdaptiveRefresh } from "@/components/shared/AdaptiveRefresh";
import { Badge } from "@/components/ui/badge";
import { Switch } from "@/components/ui/switch";
import { Label } from "@/components/ui/label";
import { formatDistanceToNow } from "date-fns";

export const Route = createFileRoute("/_authenticated/connections/")({
  component: ConnectionsPage,
});

function ConnectionsPage() {
  const navigate = useNavigate();
  const [showActive, setShowActive] = useState(false);
  const { data: connections, isLoading, refetch } = useConnections({ limit: 100 });
  const { data: users } = useUsers();
  const { data: databases } = useDatabases();

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

  const filteredConnections = showActive
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

  return (
    <div className="space-y-6">
      <PageHeader
        title="Connections"
        description="View proxy connection history"
        actions={
          <div className="flex items-center gap-4">
            <AdaptiveRefresh
              onRefresh={handleRefresh}
              storageKey="dbbat.autoRefresh.connections"
            />
            <div className="flex items-center gap-2">
              <Switch
                id="showActive"
                checked={showActive}
                onCheckedChange={setShowActive}
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
        onRowClick={(c) =>
          navigate({ to: "/queries", search: { connection_id: c.uid } })
        }
      />
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
