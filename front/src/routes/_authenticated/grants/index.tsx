import {
  useCallback,
  useMemo,
  useRef,
  useState,
} from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  useGrants,
  useUsers,
  useDatabases,
  useServerGroups,
  useRevokeGrant,
  type AccessGrant,
} from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { AdaptiveRefresh } from "@/components/shared/AdaptiveRefresh";
import { Button } from "@/components/ui/button";
import { PermissionButton } from "@/components/shared/PermissionButton";
import { useAuth } from "@/contexts/AuthContext";
import {
  canCreateGrant,
  canRevokeGrant,
  getDisabledReason,
  getActionTooltip,
} from "@/lib/permissions";
import { Badge } from "@/components/ui/badge";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { Plus, Ban } from "lucide-react";
import { formatControlName } from "@/lib/grant-format";
import { toast } from "sonner";
import { formatDateTime } from "@/lib/date-utils";
import { UsageMeter } from "@/components/shared/UsageMeter";
import { formatBytes } from "@/lib/utils";

export const Route = createFileRoute("/_authenticated/grants/")({
  // `?uid=` narrows the list to one grant: it is what Slack notices link to.
  validateSearch: (search: Record<string, unknown>): { uid?: string } => ({
    uid: typeof search.uid === "string" && search.uid ? search.uid : undefined,
  }),
  component: GrantsPage,
});

function GrantsPage() {
  const { user } = useAuth();
  const [activeOnly, setActiveOnly] = useState(true);
  const { uid: focusUid } = Route.useSearch();
  // A linked grant may be expired or revoked, so the active filter is lifted.
  const { data: allGrants, isLoading, refetch } = useGrants({
    active_only: focusUid ? false : activeOnly,
  });
  const grants = useMemo(
    () => (focusUid ? allGrants?.filter((g) => g.uid === focusUid) : allGrants),
    [allGrants, focusUid],
  );
  const { data: users } = useUsers();
  const { data: databases } = useDatabases();
  // Admin-only listing; non-admins simply get no group name to render.
  const { data: serverGroups = [] } = useServerGroups({
    enabled: canCreateGrant(user?.roles),
  });
  const [revokeGrant, setRevokeGrant] = useState<AccessGrant | null>(null);

  const previousSignatureRef = useRef<string | null>(null);

  const handleRefresh = useCallback(async () => {
    const result = await refetch();
    const newData = result.data ?? [];
    const signature = newData
      .map(
        (g) =>
          `${g.uid}:${g.revoked_at ?? ""}:${g.query_count ?? 0}:${g.bytes_transferred ?? 0}`,
      )
      .sort()
      .join("|");

    const hasNewData =
      previousSignatureRef.current !== null &&
      signature !== previousSignatureRef.current;
    previousSignatureRef.current = signature;

    return { hasNewData };
  }, [refetch]);

  const canCreate = canCreateGrant(user?.roles);

  const getUserName = (uid: string) =>
    users?.find((u) => u.uid === uid)?.username ?? uid;
  const getDbName = (uid: string) =>
    databases?.find((d) => d.uid === uid)?.name ?? uid;
  const getServerGroupName = (uid: string) =>
    serverGroups.find((g) => g.uid === uid)?.name ?? uid.slice(0, 8);

  const getStatus = (grant: AccessGrant) => {
    if (grant.revoked_at) return "revoked";
    const now = new Date();
    if (new Date(grant.starts_at) > now) return "pending";
    if (new Date(grant.expires_at) < now) return "expired";
    return "active";
  };

  const columns: Column<AccessGrant>[] = [
    {
      key: "user",
      header: "User",
      cell: (g) => (
        <span className="font-medium">{getUserName(g.user_id)}</span>
      ),
    },
    {
      key: "database",
      header: "Database",
      // A group-bound grant covers its group's *current* membership instead of
      // the database it was issued for, so naming that database alone would
      // misstate the grant's reach — and naming it alongside the group would
      // suggest the two add up, which they don't.
      cell: (g) =>
        g.server_group_uid ? (
          <div className="flex flex-col gap-0.5">
            <span className="font-mono text-sm">
              {getServerGroupName(g.server_group_uid)}
            </span>
            <span className="text-xs text-muted-foreground">
              server group — whichever servers it holds right now
            </span>
          </div>
        ) : (
          <span className="font-mono text-sm">{getDbName(g.database_id)}</span>
        ),
    },
    {
      key: "definition",
      header: "Definition",
      cell: (g) => (
        <div className="space-y-1" data-testid={`grant-definition-${g.uid}`}>
          <Link
            to="/grant-definitions"
            className="font-medium hover:underline"
            onClick={(e) => e.stopPropagation()}
          >
            {g.definition?.name ?? "—"}
          </Link>
          {g.definition && !g.definition.is_active && (
            <Badge variant="destructive">Deactivated</Badge>
          )}
        </div>
      ),
    },
    {
      key: "controls",
      header: "Controls",
      cell: (g) => {
        // The shape lives on the definition the grant was issued from; the
        // grant row itself carries none of it. An absent definition is not
        // reachable today (the FK is NOT NULL and the store attaches it
        // unfiltered), but if it ever happened, rendering "no controls"
        // would read as unrestricted — the opposite of the backend's
        // fail-closed convention (AccessGrant.Controls() in models.go
        // returns *every* control when the definition is missing). Render
        // an explicit unknown state instead of guessing.
        if (!g.definition) {
          return (
            <Tooltip>
              <TooltipTrigger asChild>
                <Badge
                  variant="destructive"
                  data-testid={`grant-controls-unknown-${g.uid}`}
                >
                  Unknown
                </Badge>
              </TooltipTrigger>
              <TooltipContent>
                This grant&apos;s definition could not be loaded, so its
                controls are unknown.
              </TooltipContent>
            </Tooltip>
          );
        }

        const controls = g.definition.controls;
        if (controls.length === 0) {
          return <Badge variant="default">Full Access</Badge>;
        }
        return (
          <div className="flex flex-wrap gap-1">
            {controls.map((control) => (
              <Badge key={control} variant="secondary">
                {formatControlName(control)}
              </Badge>
            ))}
          </div>
        );
      },
    },
    {
      key: "priority",
      header: "Priority",
      cell: (g) => (
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              className="font-mono text-sm tabular-nums"
              data-testid={`grant-priority-${g.uid}`}
            >
              {g.priority}
            </span>
          </TooltipTrigger>
          <TooltipContent>
            Among this user's overlapping active grants on this database, the
            highest priority is the one a new session is admitted under.
          </TooltipContent>
        </Tooltip>
      ),
    },
    {
      key: "time_window",
      header: "Time Window",
      cell: (g) => (
        <div className="text-sm">
          <div>{formatDateTime(g.starts_at)}</div>
          <div className="text-muted-foreground">
            to {formatDateTime(g.expires_at)}
          </div>
        </div>
      ),
    },
    {
      key: "status",
      header: "Status",
      cell: (g) => {
        const status = getStatus(g);
        const variants: Record<string, "default" | "secondary" | "destructive" | "outline"> = {
          active: "default",
          pending: "outline",
          expired: "secondary",
          revoked: "destructive",
        };
        return <Badge variant={variants[status]}>{status}</Badge>;
      },
    },
    {
      key: "usage",
      header: "Usage",
      cell: (g) => (
        <div className="space-y-2" data-testid={`grant-usage-${g.uid}`}>
          <UsageMeter
            used={g.query_count ?? 0}
            limit={g.definition?.max_query_counts}
            unit="queries"
          />
          <UsageMeter
            used={g.bytes_transferred ?? 0}
            limit={g.definition?.max_bytes_transferred}
            format={formatBytes}
          />
        </div>
      ),
    },
    {
      key: "actions",
      header: "",
      cell: (g) =>
        getStatus(g) === "active" && (
          <PermissionButton
            variant="ghost"
            size="icon"
            disabled={!canRevokeGrant(user, g)}
            data-testid={`revoke-grant-${g.uid}`}
            disabledReason={getDisabledReason("revoke-grant", user?.roles)}
            enabledTooltip={getActionTooltip("revoke-grant")}
            onClick={(e) => {
              e.stopPropagation();
              setRevokeGrant(g);
            }}
          >
            <Ban className="h-4 w-4" />
          </PermissionButton>
        ),
      className: "w-10",
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Grants"
        description="Database access, issued from grant definitions"
        actions={
          <div className="flex items-center gap-4">
            <AdaptiveRefresh
              onRefresh={handleRefresh}
              storageKey="dbbat.autoRefresh.grants"
            />
            <div className="flex items-center gap-2">
              <Switch
                id="activeOnly"
                checked={activeOnly}
                onCheckedChange={setActiveOnly}
              />
              <Label htmlFor="activeOnly">Active only</Label>
            </div>
            {canCreate ? (
              <Button asChild data-testid="assign-grant-button">
                <Link to="/grants/new">
                  <Plus className="mr-2 h-4 w-4" />
                  Assign Grant
                </Link>
              </Button>
            ) : (
              <PermissionButton
                disabled
                disabledReason={getDisabledReason("create-grant", user?.roles)}
                data-testid="assign-grant-button"
              >
                <Plus className="mr-2 h-4 w-4" />
                Assign Grant
              </PermissionButton>
            )}
          </div>
        }
      />

      {focusUid && (
        <div
          className="flex items-center gap-2 text-sm"
          data-testid="grant-focus-banner"
        >
          <span>
            Showing grant <code>{focusUid}</code> only.
          </span>
          <Link
            to="/grants"
            search={{ uid: undefined }}
            className="underline"
            data-testid="grant-focus-clear"
          >
            Show all grants
          </Link>
        </div>
      )}

      <DataTable
        columns={columns}
        data={grants ?? []}
        isLoading={isLoading}
        rowKey={(g) => g.uid}
        rowTestId={(g) => `grant-row-${g.uid}`}
        emptyMessage="No grants found"
      />

      <RevokeGrantDialog
        grant={revokeGrant}
        getUserName={getUserName}
        getDbName={getDbName}
        onClose={() => setRevokeGrant(null)}
      />
    </div>
  );
}

function RevokeGrantDialog({
  grant,
  getUserName,
  getDbName,
  onClose,
}: {
  grant: AccessGrant | null;
  getUserName: (uid: string) => string;
  getDbName: (uid: string) => string;
  onClose: () => void;
}) {
  const revokeGrant = useRevokeGrant({
    onSuccess: () => {
      toast.success("Grant revoked successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  return (
    <AlertDialog open={!!grant} onOpenChange={() => onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Revoke Grant</AlertDialogTitle>
          <AlertDialogDescription>
            Are you sure you want to revoke {getUserName(grant?.user_id ?? "")}'s
            access to {getDbName(grant?.database_id ?? "")}?
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            data-testid="revoke-grant-confirm"
            onClick={() => grant && revokeGrant.mutate(grant.uid)}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            Revoke
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
