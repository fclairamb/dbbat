import { createFileRoute } from "@tanstack/react-router";
import { useUsers, useConnections, useQueries, useDatabases } from "@/api";
import { StatCard } from "@/components/shared/StatCard";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { Badge } from "@/components/ui/badge";
import { Users, Database, Activity, Search } from "lucide-react";
import { formatDistanceToNow } from "date-fns";
import type { Query } from "@/api";
import { useAuth } from "@/contexts/AuthContext";
import { canViewQueries } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/")({
  component: DashboardPage,
});

function DashboardPage() {
  const { user } = useAuth();
  const { data: users, isLoading: usersLoading } = useUsers();
  const { data: databases, isLoading: databasesLoading } = useDatabases();
  const { data: connections, isLoading: connectionsLoading } = useConnections({
    limit: 100,
  });

  // Only fetch queries if user has viewer or admin role
  const canSeeQueries = canViewQueries(user?.roles);
  const { data: queries, isLoading: queriesLoading } = useQueries(
    { limit: 10 },
    { enabled: canSeeQueries }
  );

  const activeConnections =
    connections?.filter((c) => !c.disconnected_at).length ?? 0;

  const queryColumns: Column<Query>[] = [
    {
      key: "sql_text",
      header: "Query",
      cell: (q) => (
        <span className="font-mono text-xs truncate max-w-xs block">
          {q.sql_text.substring(0, 60)}
          {q.sql_text.length > 60 ? "..." : ""}
        </span>
      ),
    },
    {
      key: "executed_at",
      header: "Executed",
      cell: (q) => (
        <span className="text-sm text-muted-foreground">
          {formatDistanceToNow(new Date(q.executed_at), { addSuffix: true })}
        </span>
      ),
    },
    {
      key: "duration_ms",
      header: "Duration",
      cell: (q) => (
        <span className="text-sm">
          {q.duration_ms ? `${q.duration_ms.toFixed(1)}ms` : "-"}
        </span>
      ),
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

  return (
    <div className="space-y-6">
      <PageHeader
        title="Dashboard"
        description="Overview of your DBBat proxy"
      />

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <StatCard
          title="Active Connections"
          value={activeConnections}
          icon={Activity}
          isLoading={connectionsLoading}
          description="Currently connected"
        />
        {canSeeQueries && (
          <StatCard
            title="Total Queries"
            value={queries?.length ?? 0}
            icon={Search}
            isLoading={queriesLoading}
            description="Recent queries"
          />
        )}
        <StatCard
          title="Users"
          value={users?.length ?? 0}
          icon={Users}
          isLoading={usersLoading}
          description="Registered users"
        />
        <StatCard
          title="Databases"
          value={databases?.length ?? 0}
          icon={Database}
          isLoading={databasesLoading}
          description="Configured targets"
        />
      </div>

      {canSeeQueries && (
        <div className="space-y-4">
          <h2 className="text-lg font-semibold">Recent Queries</h2>
          <DataTable
            columns={queryColumns}
            data={queries ?? []}
            isLoading={queriesLoading}
            rowKey={(q) => q.uid}
            emptyMessage="No queries recorded yet"
          />
        </div>
      )}
    </div>
  );
}
