import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  useUsers,
  useDeleteUser,
  useSsoRoleMapping,
  useLastRoleSyncs,
  useLastConnections,
  type User,
} from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { navItem } from "@/lib/navigation";
import { Button } from "@/components/ui/button";
import { PermissionButton } from "@/components/shared/PermissionButton";
import { useAuth } from "@/contexts/AuthContext";
import {
  canCreateUser,
  canDeleteUser,
  canResetPassword,
  canUpdateUser,
  canViewAudit,
  getDisabledReason,
  getActionTooltip,
} from "@/lib/permissions";
import { ManagedRoleMarker } from "@/components/shared/SsoRoleSync";
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
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { Dialog, DialogTrigger } from "@/components/ui/dialog";
import { CreateUserDialog } from "@/components/users/UserForms";
import { ResetPasswordDialog } from "@/components/shared/ResetPasswordDialog";
import { Plus, Trash2, KeyRound, Pencil } from "lucide-react";
import { toast } from "sonner";
import { formatDistanceToNow } from "date-fns";

export const Route = createFileRoute("/_authenticated/users/")({
  component: UsersPage,
});

function UsersPage() {
  const { user } = useAuth();
  const { data: users, isLoading } = useUsers();
  const [isCreateOpen, setIsCreateOpen] = useState(false);
  const [deleteUser, setDeleteUser] = useState<User | null>(null);
  const [resetPasswordUser, setResetPasswordUser] = useState<User | null>(null);

  const canCreate = canCreateUser(user?.roles);
  const canUpdate = canUpdateUser(user?.roles);
  const canDelete = canDeleteUser(user?.roles);
  const canReset = canResetPassword(user?.roles);

  // Roles the identity provider owns. Editing one here is temporary: the next
  // login re-resolves it from directory groups, so the page says so up front.
  const { data: roleMapping } = useSsoRoleMapping();
  const managedRoles = new Set<string>(
    roleMapping?.enabled ? roleMapping.roles : [],
  );

  // The role-syncs endpoint is admin-or-viewer, like the audit list it reads
  // from; anyone else would just collect a 403.
  const { data: lastSyncs } = useLastRoleSyncs({
    enabled: canViewAudit(user?.roles),
  });

  // Last proxy session per user, computed in the database (one row per user)
  // rather than derived from a page of /connections — the latter reports
  // "never" for anyone whose last session fell off that page. Admin-or-viewer,
  // like the endpoint (GET /connections is gated the same way); a connector
  // would just collect a 403.
  const canReadActivity = canViewAudit(user?.roles);
  const { data: lastConnections } = useLastConnections({
    enabled: canReadActivity,
  });

  // Show the column when the mapping is live, and also when it is not but the
  // store still holds syncs — a mapping switched off does not unmake history.
  const showSyncColumn =
    (roleMapping?.enabled ?? false) || (lastSyncs?.size ?? 0) > 0;

  const syncColumn: Column<User> = {
    key: "last_synced",
    header: "SSO sync",
    cell: (u) => {
      const sync = lastSyncs?.get(u.uid);
      // Absent means never synced, full stop: the endpoint answers per user
      // rather than from a window over the audit log.
      if (!sync) {
        return <span className="text-muted-foreground">Never</span>;
      }
      return (
        <span
          className="text-sm text-muted-foreground"
          data-testid={`last-sync-${u.username}`}
        >
          {formatDistanceToNow(new Date(sync.created_at), { addSuffix: true })}
        </span>
      );
    },
  };

  /** Relative time, or a muted "Never" when the timestamp is absent. */
  const relativeOrNever = (at: string | null | undefined, testId: string) => {
    if (!at) {
      return (
        <span className="text-muted-foreground" data-testid={testId}>
          Never
        </span>
      );
    }

    return (
      <span className="text-sm text-muted-foreground" data-testid={testId}>
        {formatDistanceToNow(new Date(at), { addSuffix: true })}
      </span>
    );
  };

  // Rendered only for a role that may fetch it. A connector sees their own row
  // in this list, and showing them a column the API refuses to answer would
  // read as a confident "Never" — worse than not offering the column at all.
  const lastConnectionColumn: Column<User> = {
    key: "last_connection",
    header: "Last SQL connection",
    // "Last connection still on record": connection rows are deletable and are
    // reaped by retention, so a user whose sessions have all aged out reads as
    // Never.
    cell: (u) =>
      relativeOrNever(
        lastConnections?.get(u.uid)?.connected_at,
        `last-connection-${u.username}`,
      ),
  };

  const columns: Column<User>[] = [
    {
      key: "username",
      header: "Username",
      cell: (u) =>
        canUpdate ? (
          <Link
            to="/users/$uid"
            params={{ uid: u.uid }}
            className="font-medium hover:underline"
            data-testid={`user-link-${u.username}`}
          >
            {u.username}
          </Link>
        ) : (
          <span className="font-medium">{u.username}</span>
        ),
    },
    {
      key: "roles",
      header: "Roles",
      cell: (u) => (
        <div className="flex gap-1 flex-wrap items-center">
          {u.roles?.map((role) => (
            <span key={role} className="inline-flex items-center gap-1">
              <Badge variant={role === "admin" ? "default" : "secondary"}>
                {role}
              </Badge>
              {managedRoles.has(role) && <ManagedRoleMarker role={role} />}
            </span>
          ))}
        </div>
      ),
    },
    {
      key: "rate_limit_exempt",
      header: "Rate Limit",
      cell: (u) =>
        u.rate_limit_exempt ? (
          <Badge variant="outline">Exempt</Badge>
        ) : (
          <span className="text-muted-foreground">Standard</span>
        ),
    },
    {
      key: "created_at",
      header: "Created",
      cell: (u) => (
        <span className="text-sm text-muted-foreground">
          {formatDistanceToNow(new Date(u.created_at), { addSuffix: true })}
        </span>
      ),
    },
    {
      key: "last_login",
      header: "Last login",
      // Interactive sign-ins only — password or SSO. Token refreshes and
      // API-key requests deliberately do not move it, so this reads "last seen
      // in the UI", not "last request from anything".
      cell: (u) => relativeOrNever(u.last_login_at, `last-login-${u.username}`),
    },
    ...(canReadActivity ? [lastConnectionColumn] : []),
    ...(showSyncColumn ? [syncColumn] : []),
    {
      key: "actions",
      header: "",
      cell: (u) => (
        <div className="flex items-center justify-end gap-1">
          <Tooltip>
            <TooltipTrigger asChild>
              {canUpdate ? (
                <Button
                  asChild
                  variant="ghost"
                  size="icon"
                  data-testid={`edit-user-${u.username}`}
                  aria-label={`Edit user ${u.username}`}
                >
                  <Link to="/users/$uid" params={{ uid: u.uid }}>
                    <Pencil className="h-4 w-4" />
                  </Link>
                </Button>
              ) : (
                <Button
                  variant="ghost"
                  size="icon"
                  disabled
                  data-testid={`edit-user-${u.username}`}
                  aria-label={`Edit user ${u.username}`}
                >
                  <Pencil className="h-4 w-4" />
                </Button>
              )}
            </TooltipTrigger>
            <TooltipContent>
              {canUpdate
                ? getActionTooltip("update-user")
                : getDisabledReason("update-user", user?.roles)}
            </TooltipContent>
          </Tooltip>
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                disabled={!canReset}
                onClick={(e) => {
                  e.stopPropagation();
                  setResetPasswordUser(u);
                }}
                data-testid={`reset-password-${u.username}`}
                aria-label={`Reset password for ${u.username}`}
              >
                <KeyRound className="h-4 w-4" />
              </Button>
            </TooltipTrigger>
            <TooltipContent>
              {canReset
                ? "Reset password"
                : getDisabledReason("reset-password", user?.roles)}
            </TooltipContent>
          </Tooltip>
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                disabled={!canDelete}
                onClick={(e) => {
                  e.stopPropagation();
                  setDeleteUser(u);
                }}
                className="text-destructive hover:text-destructive"
                data-testid={`delete-user-${u.username}`}
                aria-label={`Delete user ${u.username}`}
              >
                <Trash2 className="h-4 w-4" />
              </Button>
            </TooltipTrigger>
            <TooltipContent>
              {canDelete
                ? "Delete user"
                : getDisabledReason("delete-user", user?.roles)}
            </TooltipContent>
          </Tooltip>
        </div>
      ),
      className: "w-20",
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        icon={navItem("/users")!.icon}
        title="Users"
        description="Manage user accounts"
        actions={
          <Dialog open={isCreateOpen} onOpenChange={setIsCreateOpen}>
            <DialogTrigger asChild>
              <PermissionButton
                disabled={!canCreate}
                disabledReason={getDisabledReason("create-user", user?.roles)}
                enabledTooltip={getActionTooltip("create-user")}
              >
                <Plus className="mr-2 h-4 w-4" />
                New User
              </PermissionButton>
            </DialogTrigger>
            <CreateUserDialog onClose={() => setIsCreateOpen(false)} />
          </Dialog>
        }
      />

      <DataTable
        columns={columns}
        data={users ?? []}
        isLoading={isLoading}
        rowKey={(u) => u.uid}
        emptyMessage="No users found"
      />

      <DeleteUserDialog user={deleteUser} onClose={() => setDeleteUser(null)} />

      {resetPasswordUser && (
        <ResetPasswordDialog
          user={resetPasswordUser}
          open={!!resetPasswordUser}
          onOpenChange={(open) => !open && setResetPasswordUser(null)}
          onSuccess={() => {
            toast.success(`Password reset for ${resetPasswordUser.username}`);
          }}
        />
      )}
    </div>
  );
}

function DeleteUserDialog({
  user,
  onClose,
}: {
  user: User | null;
  onClose: () => void;
}) {
  const deleteUser = useDeleteUser({
    onSuccess: () => {
      toast.success("User deleted successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  return (
    <AlertDialog open={!!user} onOpenChange={() => onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete User</AlertDialogTitle>
          <AlertDialogDescription>
            Are you sure you want to delete user "{user?.username}"? This action
            cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => user && deleteUser.mutate(user.uid)}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
