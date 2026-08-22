import { useRef, useCallback } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  useQueries,
  useUsers,
  useDatabases,
  useServerGroups,
  useGrantDefinitions,
} from "@/api";
import {
  ObservabilityFilterBar,
  type ObservabilityFilters,
} from "@/components/shared/ObservabilityFilterBar";
import { DataTable } from "@/components/shared/DataTable";
import { buildQueryColumns } from "@/components/shared/queryColumns";
import { PageHeader } from "@/components/shared/PageHeader";
import { AdaptiveRefresh } from "@/components/shared/AdaptiveRefresh";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { X, ChevronLeft, ChevronRight } from "lucide-react";
import { useAuth } from "@/contexts/AuthContext";
import { canViewQueries } from "@/lib/permissions";
import { AccessDenied } from "@/components/shared/AccessDenied";

const DEFAULT_PAGE_SIZE = 50;
const PAGE_SIZE_OPTIONS = [25, 50, 100];

type QuerySearch = ObservabilityFilters & {
  connection_id?: string;
  before?: string;
  size: number;
};

const asString = (value: unknown) =>
  typeof value === "string" && value !== "" ? value : undefined;

export const Route = createFileRoute("/_authenticated/queries/")({
  // Filters ride in the URL alongside `connection_id`, `before` and `size`, so
  // a filtered view is shareable and the back button works.
  validateSearch: (search: Record<string, unknown>): QuerySearch => ({
    connection_id: asString(search.connection_id),
    before: asString(search.before),
    size: search.size ? Number(search.size) : DEFAULT_PAGE_SIZE,
    user_id: asString(search.user_id),
    database_id: asString(search.database_id),
    server_group_uid: asString(search.server_group_uid),
    grant_definition_uid: asString(search.grant_definition_uid),
    grant_uid: asString(search.grant_uid),
    grant_provenance: asString(search.grant_provenance),
    approval_status: asString(search.approval_status),
  }),
  component: QueriesPage,
});

function QueriesPage() {
  const { user } = useAuth();
  const search = Route.useSearch();
  const { connection_id, before, size } = search;
  const navigate = Route.useNavigate();
  // Server-side, all of it — see the note on the connections listing.
  const { data: queries, isLoading, refetch } = useQueries({
    connection_id,
    before,
    limit: size,
    user_id: search.user_id,
    database_id: search.database_id,
    server_group_uid: search.server_group_uid,
    grant_definition_uid: search.grant_definition_uid,
    grant_uid: search.grant_uid,
    grant_provenance: search.grant_provenance,
    approval_status: search.approval_status,
  });
  const { data: users } = useUsers();
  const { data: databases } = useDatabases();
  const { data: serverGroups } = useServerGroups();
  const { data: grantDefinitions } = useGrantDefinitions();

  // Any filter change drops the cursor: a `before` from the previous result
  // set silently hides the head of the new one.
  const applyFilters = (patch: ObservabilityFilters) => {
    navigate({
      search: (prev) => ({ ...prev, ...patch, before: undefined }),
      replace: true,
    });
  };

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

  const columns = buildQueryColumns({ users, databases, size });

  const lastUid = queries && queries.length > 0 ? queries[queries.length - 1].uid : undefined;
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
                  <Link
                    to="/queries"
                    search={(prev) => ({
                      ...prev,
                      size,
                      before: undefined,
                      connection_id: undefined,
                    })}
                  >
                    <X className="h-3 w-3" />
                  </Link>
                </Button>
              </Badge>
            )}
          </div>
        }
      />

      <ObservabilityFilterBar
        filters={search}
        onChange={applyFilters}
        users={users}
        databases={databases}
        serverGroups={serverGroups}
        grantDefinitions={grantDefinitions}
        showApprovalStatus
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
                search={(prev) => ({ ...prev, before: undefined, size: opt })}
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
                search={(prev) => ({ ...prev, before: undefined, size })}
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
                search={(prev) => ({ ...prev, before: lastUid, size })}
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
