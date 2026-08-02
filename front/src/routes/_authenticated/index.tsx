import { createFileRoute } from "@tanstack/react-router";
import { useUsers, useConnections, useQueries, useDatabases } from "@/api";
import { StatCard } from "@/components/shared/StatCard";
import { DataTable } from "@/components/shared/DataTable";
import { buildQueryColumns } from "@/components/shared/queryColumns";
import { PageHeader } from "@/components/shared/PageHeader";
import { Users, Database, Activity, Search } from "lucide-react";
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

  const queryColumns = buildQueryColumns({ users, databases });

  return (
    <div className="space-y-6">
      <PageHeader
        title="Dashboard"
        description="Overview of your DBBat proxy"
      />

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        {(connectionsLoading || activeConnections > 0) && (
          <StatCard
            title="Active Connections"
            value={activeConnections}
            icon={Activity}
            isLoading={connectionsLoading}
            description="Currently connected"
          />
        )}
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
            rowHref={(q) => `/queries/${q.uid}`}
            emptyMessage="No queries recorded yet"
          />
        </div>
      )}
    </div>
  );
}
