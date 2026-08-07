import { useCallback, useMemo, useRef, useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  useGrants,
  useUsers,
  useDatabases,
  useGrantDefinitions,
  useAssignGrant,
  useRevokeGrant,
  type AccessGrant,
  type GrantDefinition,
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
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { Plus, Ban } from "lucide-react";
import { toast } from "sonner";
import { formatDateTimeLocal, formatDateTime } from "@/lib/date-utils";
import { UsageMeter } from "@/components/shared/UsageMeter";
import { formatBytes } from "@/lib/utils";

// Helper to format control names for display
function formatControlName(control: string): string {
  return control
    .replace(/_/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

// Helper to format duration in human-readable format
function formatDuration(ms: number): string {
  if (ms <= 0) return "0 minutes";

  const seconds = Math.floor(ms / 1000);
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);
  const days = Math.floor(hours / 24);

  const parts: string[] = [];

  if (days > 0) {
    parts.push(`${days} day${days !== 1 ? "s" : ""}`);
  }
  if (hours % 24 > 0) {
    parts.push(`${hours % 24} hour${hours % 24 !== 1 ? "s" : ""}`);
  }
  if (minutes % 60 > 0 && days === 0) {
    parts.push(`${minutes % 60} minute${minutes % 60 !== 1 ? "s" : ""}`);
  }

  return parts.length > 0 ? parts.join(", ") : "less than a minute";
}

export const Route = createFileRoute("/_authenticated/grants/")({
  component: GrantsPage,
});

function GrantsPage() {
  const { user } = useAuth();
  const [activeOnly, setActiveOnly] = useState(true);
  const { data: grants, isLoading, refetch } = useGrants({ active_only: activeOnly });
  const { data: users } = useUsers();
  const { data: databases } = useDatabases();
  const [isAssignOpen, setIsAssignOpen] = useState(false);
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
  const canRevoke = canRevokeGrant(user?.roles);

  const getUserName = (uid: string) =>
    users?.find((u) => u.uid === uid)?.username ?? uid;
  const getDbName = (uid: string) =>
    databases?.find((d) => d.uid === uid)?.name ?? uid;

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
      cell: (g) => (
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
            disabled={!canRevoke}
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
            <Dialog open={isAssignOpen} onOpenChange={setIsAssignOpen}>
              <DialogTrigger asChild>
                <PermissionButton
                  disabled={!canCreate}
                  disabledReason={getDisabledReason("create-grant", user?.roles)}
                  enabledTooltip={getActionTooltip("create-grant")}
                  data-testid="assign-grant-button"
                >
                  <Plus className="mr-2 h-4 w-4" />
                  Assign Grant
                </PermissionButton>
              </DialogTrigger>
              {isAssignOpen && (
                <AssignGrantDialog
                  users={users ?? []}
                  databases={databases ?? []}
                  onClose={() => setIsAssignOpen(false)}
                />
              )}
            </Dialog>
          </div>
        }
      />

      <DataTable
        columns={columns}
        data={grants ?? []}
        isLoading={isLoading}
        rowKey={(g) => g.uid}
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

/**
 * AssignGrantDialog replaced the old "Create Grant" form. An admin no longer
 * invents a grant's shape here: they pick a definition, and the controls,
 * quotas, approval gating and duration all come from it. That is what makes a
 * definition trustworthy as the policy source of truth — no grant can be an
 * unauditable one-off.
 */
function AssignGrantDialog({
  users,
  databases,
  onClose,
}: {
  users: { uid: string; username: string }[];
  databases: { uid: string; name: string }[];
  onClose: () => void;
}) {
  const { data: definitions } = useGrantDefinitions({ active_only: true });
  const [definitionUid, setDefinitionUid] = useState("");
  const [userId, setUserId] = useState("");
  const [databaseId, setDatabaseId] = useState("");
  const [startsAt, setStartsAt] = useState(() => {
    const now = new Date();
    now.setSeconds(0, 0);
    return formatDateTimeLocal(now);
  });

  const definition: GrantDefinition | undefined = useMemo(
    () => definitions?.find((d) => d.uid === definitionUid),
    [definitions, definitionUid],
  );

  // A definition scoped to specific databases can only be assigned against
  // those — the server enforces it, so the picker shouldn't offer the rest.
  const selectableDatabases = useMemo(() => {
    const scope = definition?.database_uids ?? [];
    if (scope.length === 0) return databases;
    return databases.filter((d) => scope.includes(d.uid));
  }, [databases, definition]);

  const durationMs = (definition?.duration_seconds ?? 0) * 1000;

  const assignGrant = useAssignGrant({
    onSuccess: () => {
      toast.success("Grant assigned successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    assignGrant.mutate({
      grant_definition_id: definitionUid,
      user_id: userId,
      database_id: databaseId,
      starts_at: new Date(startsAt).toISOString(),
    });
  };

  return (
    <DialogContent data-testid="assign-grant-dialog">
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Assign Grant</DialogTitle>
          <DialogDescription>
            Give a user access to a database by issuing a grant definition. The
            definition decides what the access can do and how long it lasts.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label htmlFor="definition">Grant definition</Label>
            <Select
              value={definitionUid}
              onValueChange={(value) => {
                setDefinitionUid(value);
                setDatabaseId("");
              }}
              required
            >
              <SelectTrigger data-testid="assign-grant-definition">
                <SelectValue placeholder="Select a grant definition" />
              </SelectTrigger>
              <SelectContent>
                {(definitions ?? []).map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {definitions?.length === 0 && (
              <p className="text-sm text-muted-foreground">
                No active grant definitions yet — create one first; grants can
                only be issued from a definition.
              </p>
            )}
          </div>

          {definition && (
            <div
              className="rounded-md border p-3 space-y-2 text-sm"
              data-testid="assign-grant-shape"
            >
              <div className="flex flex-wrap items-center gap-1">
                <span className="text-muted-foreground">Controls:</span>
                {(definition.controls ?? []).length === 0 ? (
                  <Badge variant="default">Full Access</Badge>
                ) : (
                  definition.controls.map((control) => (
                    <Badge key={control} variant="secondary">
                      {formatControlName(control)}
                    </Badge>
                  ))
                )}
              </div>
              <div className="text-muted-foreground">
                Duration: {formatDuration(durationMs)}
              </div>
              <div className="text-muted-foreground">
                Quotas:{" "}
                {definition.max_query_counts
                  ? `${definition.max_query_counts} queries`
                  : "unlimited queries"}
                {", "}
                {definition.max_bytes_transferred
                  ? formatBytes(definition.max_bytes_transferred)
                  : "unlimited transfer"}
              </div>
              {(definition.approval_patterns?.length ?? 0) > 0 && (
                <div className="text-muted-foreground">
                  Approval holds: {definition.approval_patterns?.length} pattern
                  {definition.approval_patterns?.length !== 1 ? "s" : ""}
                </div>
              )}
            </div>
          )}

          <div className="space-y-2">
            <Label htmlFor="user">User</Label>
            <Select value={userId} onValueChange={setUserId} required>
              <SelectTrigger data-testid="assign-grant-user">
                <SelectValue placeholder="Select user" />
              </SelectTrigger>
              <SelectContent>
                {users.map((u) => (
                  <SelectItem key={u.uid} value={u.uid}>
                    {u.username}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="database">Database</Label>
            <Select value={databaseId} onValueChange={setDatabaseId} required>
              <SelectTrigger data-testid="assign-grant-database">
                <SelectValue placeholder="Select database" />
              </SelectTrigger>
              <SelectContent>
                {selectableDatabases.map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {definition && (definition.database_uids ?? []).length > 0 && (
              <p className="text-xs text-muted-foreground">
                This definition is scoped to specific databases.
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="startsAt">Start Date &amp; Time</Label>
            <Input
              id="startsAt"
              type="datetime-local"
              value={startsAt}
              onChange={(e) => setStartsAt(e.target.value)}
              required
            />
            <p className="text-xs text-muted-foreground">
              The grant expires {formatDuration(durationMs)} after it starts —
              that length comes from the definition.
            </p>
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={
              assignGrant.isPending || !definitionUid || !userId || !databaseId
            }
            data-testid="assign-grant-submit"
          >
            Assign
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
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
