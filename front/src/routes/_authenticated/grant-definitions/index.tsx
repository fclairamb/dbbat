import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Plus, Pencil, Trash2 } from "lucide-react";
import { toast } from "sonner";

import {
  useGrantDefinitions,
  useUpdateGrantDefinition,
  useDeactivateGrantDefinition,
  type GrantDefinition,
  type UpdateGrantDefinitionRequest,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
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
import { useAuth } from "@/contexts/AuthContext";
import { canManageGrantDefinitions } from "@/lib/permissions";
import { UsageLimit } from "@/components/shared/UsageMeter";
import { formatBytes } from "@/lib/utils";
import { autoPriority } from "@/lib/grant-priority";

export const Route = createFileRoute("/_authenticated/grant-definitions/")({
  component: GrantDefinitionsPage,
});

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    const rem = minutes % 60;
    return rem ? `${hours}h ${rem}m` : `${hours}h`;
  }
  const days = Math.floor(hours / 24);
  const rem = hours % 24;
  return rem ? `${days}d ${rem}h` : `${days}d`;
}

function GrantDefinitionsPage() {
  const { user } = useAuth();
  const isAdmin = canManageGrantDefinitions(user?.roles);

  // Deactivated definitions are permanent (hard deletion is refused while
  // anything references them), so the default view hides them behind an
  // admin-only toggle instead of letting the list grow unbounded with dead
  // rows. Non-admins never reach this page in the nav, but the API also
  // enforces the active-only filter for them as a defense-in-depth.
  const [showDeactivated, setShowDeactivated] = useState(false);
  const { data: definitions = [], isLoading } = useGrantDefinitions({
    active_only: !isAdmin || !showDeactivated,
  });

  const [deactivating, setDeactivating] = useState<GrantDefinition | null>(
    null
  );
  const [confirmingAutoApprove, setConfirmingAutoApprove] =
    useState<GrantDefinition | null>(null);

  const deactivate = useDeactivateGrantDefinition({
    onSuccess: (affectedGrants) => {
      toast.success(
        affectedGrants > 0
          ? `Definition deactivated — ${affectedGrants} grant${affectedGrants === 1 ? "" : "s"} no longer authorise access`
          : "Definition deactivated"
      );
      setDeactivating(null);
    },
    onError: (e) => toast.error(e.message),
  });

  const updateAutoApprove = useUpdateGrantDefinition({
    onSuccess: (def) => {
      toast.success(
        def.auto_approve
          ? "Auto-approve enabled"
          : "Auto-approve disabled"
      );
      setConfirmingAutoApprove(null);
    },
    onError: (e) => toast.error(e.message),
  });

  const toggleAutoApprove = (d: GrantDefinition, next: boolean) => {
    // PATCH is a genuine partial update: only auto_approve changes here,
    // every other field (scope, approval patterns, approver groups, ...)
    // is left untouched on the server.
    const body: UpdateGrantDefinitionRequest = { auto_approve: next };
    updateAutoApprove.mutate({ uid: d.uid, body });
  };

  const columns: Column<GrantDefinition>[] = [
    {
      key: "name",
      header: "Name",
      cell: (d: GrantDefinition) => (
        <div className="flex flex-col">
          {isAdmin && d.is_active ? (
            <Link
              to="/grant-definitions/$uid"
              params={{ uid: d.uid }}
              className="font-medium hover:underline"
              data-testid={`grant-definition-link-${d.uid}`}
            >
              {d.name}
            </Link>
          ) : (
            <span className="font-medium">{d.name}</span>
          )}
          {d.description && (
            <span className="text-xs text-muted-foreground">
              {d.description}
            </span>
          )}
        </div>
      ),
    },
    {
      key: "slug",
      header: "Slug",
      cell: (d: GrantDefinition) => (
        <code
          className="text-xs bg-secondary px-1.5 py-0.5 rounded"
          data-testid={`grant-definition-slug-${d.uid}`}
        >
          {d.slug}
        </code>
      ),
    },
    {
      key: "duration_seconds",
      header: "Duration",
      cell: (d: GrantDefinition) => formatDuration(d.duration_seconds),
    },
    {
      key: "controls",
      header: "Controls",
      cell: (d: GrantDefinition) =>
        d.controls.length === 0 ? (
          <span className="text-muted-foreground italic">none</span>
        ) : (
          <div className="flex gap-1 flex-wrap">
            {d.controls.map((c) => (
              <span
                key={c}
                className="text-xs bg-secondary px-1.5 py-0.5 rounded"
              >
                {c}
              </span>
            ))}
          </div>
        ),
    },
    {
      key: "priority",
      header: "Priority",
      cell: (d: GrantDefinition) => (
        <span
          className="font-mono text-sm tabular-nums"
          data-testid={`grant-definition-priority-${d.uid}`}
        >
          {d.priority ?? (
            <span className="text-muted-foreground italic font-sans">
              auto ({autoPriority(d.controls)})
            </span>
          )}
        </span>
      ),
    },
    {
      key: "max_query_counts",
      header: "Max Queries",
      cell: (d: GrantDefinition) => (
        <UsageLimit limit={d.max_query_counts} unit="queries" />
      ),
    },
    {
      key: "max_bytes_transferred",
      header: "Max Data",
      cell: (d: GrantDefinition) => (
        <UsageLimit limit={d.max_bytes_transferred} format={formatBytes} />
      ),
    },
    {
      key: "scope",
      header: "Scope",
      cell: (d: GrantDefinition) => (
        <div className="flex flex-col gap-0.5 text-xs">
          <span>
            {d.user_group_uids.length === 0 ? (
              <span className="text-muted-foreground italic">all users</span>
            ) : (
              `${d.user_group_uids.length} user group${
                d.user_group_uids.length > 1 ? "s" : ""
              }`
            )}
          </span>
          <span>
            {d.server_group_uids.length === 0 ? (
              <span className="text-muted-foreground italic">
                all databases
              </span>
            ) : (
              `${d.server_group_uids.length} server group${
                d.server_group_uids.length > 1 ? "s" : ""
              }`
            )}
          </span>
        </div>
      ),
    },
    {
      key: "auto_approve",
      header: "Auto-approve",
      cell: (d: GrantDefinition) =>
        isAdmin && d.is_active ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <div className="flex items-center gap-2">
                <Switch
                  checked={d.auto_approve}
                  disabled={updateAutoApprove.isPending}
                  onCheckedChange={(checked) => {
                    if (checked) {
                      setConfirmingAutoApprove(d);
                    } else {
                      toggleAutoApprove(d, false);
                    }
                  }}
                  data-testid={`grant-definition-auto-approve-${d.uid}`}
                />
                <span className="text-xs text-muted-foreground">
                  {d.auto_approve ? "auto-approved" : "manual"}
                </span>
              </div>
            </TooltipTrigger>
            <TooltipContent>
              Requests against this definition skip admin review and are
              approved instantly when this is on.
            </TooltipContent>
          </Tooltip>
        ) : d.auto_approve ? (
          <span className="text-xs bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400 px-1.5 py-0.5 rounded">
            auto-approved
          </span>
        ) : (
          <span className="text-xs text-muted-foreground italic">manual</span>
        ),
    },
    {
      key: "is_active",
      header: "Status",
      cell: (d: GrantDefinition) =>
        d.is_active ? (
          <span className="text-xs bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400 px-1.5 py-0.5 rounded">
            active
          </span>
        ) : (
          <span className="text-xs bg-muted text-muted-foreground px-1.5 py-0.5 rounded">
            deactivated
          </span>
        ),
    },
    {
      key: "actions",
      header: "",
      cell: (d: GrantDefinition) =>
        isAdmin && d.is_active ? (
          <div className="flex gap-1 justify-end">
            <Button
              asChild
              size="sm"
              variant="ghost"
              data-testid={`edit-grant-definition-${d.uid}`}
            >
              <Link to="/grant-definitions/$uid" params={{ uid: d.uid }}>
                <Pencil className="h-4 w-4" />
              </Link>
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setDeactivating(d)}
              data-testid={`deactivate-grant-definition-${d.uid}`}
            >
              <Trash2 className="h-4 w-4" />
            </Button>
          </div>
        ) : null,
    },
  ];

  return (
    <div className="container mx-auto py-6">
      <PageHeader
        title="Grant Definitions"
        description="Templates for the grant request workflow. Active definitions appear in the request UI."
        actions={
          isAdmin && (
            <div className="flex items-center gap-4">
              <div className="flex items-center gap-2">
                <Switch
                  id="showDeactivated"
                  checked={showDeactivated}
                  onCheckedChange={setShowDeactivated}
                  data-testid="show-deactivated-toggle"
                />
                <Label htmlFor="showDeactivated">Show deactivated</Label>
              </div>
              <Button asChild data-testid="create-grant-definition-button">
                <Link to="/grant-definitions/new">
                  <Plus className="h-4 w-4 mr-2" />
                  New Definition
                </Link>
              </Button>
            </div>
          )
        }
      />

      <DataTable
        columns={columns}
        data={definitions}
        isLoading={isLoading}
        rowKey={(d: GrantDefinition) => d.uid}
        emptyMessage="No grant definitions yet. Create one to enable the grant request workflow."
      />

      <AlertDialog
        open={!!deactivating}
        onOpenChange={(o) => !o && setDeactivating(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Deactivate definition?</AlertDialogTitle>
            <AlertDialogDescription data-testid="deactivate-definition-warning">
              "{deactivating?.name}" will no longer appear in the request UI,
              and it can no longer be assigned.
              {(deactivating?.active_grant_count ?? 0) > 0 ? (
                <>
                  {" "}
                  <strong>
                    {deactivating?.active_grant_count} active grant
                    {deactivating?.active_grant_count === 1 ? "" : "s"} will
                    stop working:
                  </strong>{" "}
                  a grant reads its controls, quotas and approval rules from
                  this definition, so deactivating it fails those grants closed
                  on their next connection. Existing sessions keep running until
                  they reconnect.
                </>
              ) : (
                " No grant is currently issued from it."
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => deactivating && deactivate.mutate(deactivating.uid)}
            >
              Deactivate
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog
        open={!!confirmingAutoApprove}
        onOpenChange={(o) => !o && setConfirmingAutoApprove(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Enable auto-approve?</AlertDialogTitle>
            <AlertDialogDescription>
              Grant requests against "{confirmingAutoApprove?.name}" will skip
              admin review and be approved instantly at request time. This
              removes human review for this definition — requesters will
              still be required to provide a justification.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() =>
                confirmingAutoApprove &&
                toggleAutoApprove(confirmingAutoApprove, true)
              }
              data-testid="confirm-grant-definition-auto-approve"
            >
              Enable auto-approve
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
