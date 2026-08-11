import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  useUsers,
  useCreateUser,
  useUpdateUser,
  useUserGroups,
  useDeleteUser,
  useSsoRoleMapping,
  useLastRoleSyncs,
  type AuditEvent,
  type User,
} from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
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
  type UserRole,
} from "@/lib/permissions";
import {
  LastRoleSync,
  ManagedRoleBadge,
  ManagedRoleMarker,
} from "@/components/shared/SsoRoleSync";
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
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { ResetPasswordDialog } from "@/components/shared/ResetPasswordDialog";
import { MultiSelect } from "@/components/shared/MultiSelect";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
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
  const [editUser, setEditUser] = useState<User | null>(null);
  const [deleteUser, setDeleteUser] = useState<User | null>(null);
  const [resetPasswordUser, setResetPasswordUser] = useState<User | null>(null);

  const canCreate = canCreateUser(user?.roles);
  const canUpdate = canUpdateUser(user?.roles);
  const canDelete = canDeleteUser(user?.roles);
  const canReset = canResetPassword(user?.roles);

  const adminCount =
    users?.filter((u) => u.roles?.includes("admin")).length ?? 0;

  // Roles the identity provider owns. Editing one here is temporary: the next
  // login re-resolves it from directory groups, so the page says so up front.
  const { data: roleMapping } = useSsoRoleMapping();
  const managedRoles = new Set<string>(
    roleMapping?.enabled ? roleMapping.roles : [],
  );

  // The audit list is admin-or-viewer; anyone else would just collect a 403.
  const { data: lastSyncs } = useLastRoleSyncs({
    enabled: canViewAudit(user?.roles),
  });

  // Show the column when the mapping is live, and also when it is not but the
  // store still holds syncs — a mapping switched off does not unmake history.
  const showSyncColumn =
    (roleMapping?.enabled ?? false) || (lastSyncs?.size ?? 0) > 0;

  const syncColumn: Column<User> = {
    key: "last_synced",
    header: "SSO sync",
    cell: (u) => {
      const event = lastSyncs?.get(u.uid);
      if (!event) {
        return <span className="text-muted-foreground">Never</span>;
      }
      return (
        <span
          className="text-sm text-muted-foreground"
          data-testid={`last-sync-${u.username}`}
        >
          {formatDistanceToNow(new Date(event.created_at), { addSuffix: true })}
        </span>
      );
    },
  };

  const columns: Column<User>[] = [
    {
      key: "username",
      header: "Username",
      cell: (u) => <span className="font-medium">{u.username}</span>,
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
    ...(showSyncColumn ? [syncColumn] : []),
    {
      key: "actions",
      header: "",
      cell: (u) => (
        <div className="flex items-center justify-end gap-1">
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                disabled={!canUpdate}
                onClick={(e) => {
                  e.stopPropagation();
                  setEditUser(u);
                }}
                data-testid={`edit-user-${u.username}`}
                aria-label={`Edit user ${u.username}`}
              >
                <Pencil className="h-4 w-4" />
              </Button>
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
                Create User
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

      {editUser && (
        <Dialog open onOpenChange={(open) => !open && setEditUser(null)}>
          <EditUserDialog
            key={editUser.uid}
            user={editUser}
            currentUserUid={user?.uid}
            isLastAdmin={
              (editUser.roles?.includes("admin") ?? false) && adminCount <= 1
            }
            managedRoles={managedRoles}
            lastRoleSync={lastSyncs?.get(editUser.uid)}
            onClose={() => setEditUser(null)}
          />
        </Dialog>
      )}

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

const ROLE_OPTIONS: { value: UserRole; label: string }[] = [
  { value: "admin", label: "Administrator" },
  { value: "viewer", label: "Viewer" },
  { value: "connector", label: "Connector" },
];

/** Normalize an API roles array to known roles, in canonical order */
const toUserRoles = (roles: string[] | undefined): UserRole[] =>
  ROLE_OPTIONS.map((o) => o.value).filter((v) => roles?.includes(v));

function RoleCheckboxes({
  idPrefix,
  roles,
  onChange,
  lockedRole,
  lockedReason,
  managedRoles,
}: {
  idPrefix: string;
  roles: UserRole[];
  onChange: (roles: UserRole[]) => void;
  lockedRole?: UserRole;
  lockedReason?: string;
  /** Roles the directory owns; badged, but still editable (temporarily). */
  managedRoles?: Set<string>;
}) {
  const toggleRole = (value: UserRole, checked: boolean) => {
    const next = checked
      ? [...new Set([...roles, value])]
      : roles.filter((r) => r !== value);
    // Keep a stable, canonical role ordering
    onChange(ROLE_OPTIONS.map((o) => o.value).filter((v) => next.includes(v)));
  };

  return (
    <div className="space-y-2">
      <Label>Roles</Label>
      {ROLE_OPTIONS.map((role) => {
        const isLocked = role.value === lockedRole;
        const row = (
          <div className="flex items-center space-x-2">
            <Checkbox
              id={`${idPrefix}-role-${role.value}`}
              checked={roles.includes(role.value)}
              disabled={isLocked}
              onCheckedChange={(checked) =>
                toggleRole(role.value, checked === true)
              }
              data-testid={`${idPrefix}-role-${role.value}`}
              aria-label={`${role.label} role`}
            />
            <Label
              htmlFor={`${idPrefix}-role-${role.value}`}
              className="font-normal"
            >
              {role.label}
            </Label>
            {managedRoles?.has(role.value) && (
              <ManagedRoleBadge role={role.value} />
            )}
          </div>
        );

        if (!isLocked || !lockedReason) {
          return <div key={role.value}>{row}</div>;
        }

        return (
          <Tooltip key={role.value} delayDuration={0}>
            <TooltipTrigger asChild>
              <div>{row}</div>
            </TooltipTrigger>
            <TooltipContent>{lockedReason}</TooltipContent>
          </Tooltip>
        );
      })}
    </div>
  );
}

function CreateUserDialog({ onClose }: { onClose: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [roles, setRoles] = useState<UserRole[]>(["connector"]);

  const createUser = useCreateUser({
    onSuccess: () => {
      toast.success("User created successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createUser.mutate({
      username,
      password,
      roles,
    });
  };

  return (
    <DialogContent data-testid="create-user-dialog">
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Create User</DialogTitle>
          <DialogDescription>Add a new user to the system.</DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label htmlFor="username">Username</Label>
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="password">Password</Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </div>
          <RoleCheckboxes
            idPrefix="create-user"
            roles={roles}
            onChange={setRoles}
          />
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={createUser.isPending || roles.length === 0}
            data-testid="create-user-submit"
          >
            Create
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

function EditUserDialog({
  user: targetUser,
  currentUserUid,
  isLastAdmin,
  managedRoles,
  lastRoleSync,
  onClose,
}: {
  user: User;
  currentUserUid?: string;
  isLastAdmin: boolean;
  managedRoles: Set<string>;
  lastRoleSync?: AuditEvent;
  onClose: () => void;
}) {
  const [roles, setRoles] = useState<UserRole[]>(toUserRoles(targetUser.roles));
  const [confirmSelfDemote, setConfirmSelfDemote] = useState(false);
  const [confirmManagedRoles, setConfirmManagedRoles] = useState(false);
  // The groups listing carries membership, so we can seed the selection
  // without a second per-user round-trip.
  const { data: groups = [] } = useUserGroups();
  const [groupUids, setGroupUids] = useState<string[] | null>(null);
  const currentGroupUids = groups
    .filter((g) => g.member_uids.includes(targetUser.uid))
    .map((g) => g.uid);
  const selectedGroupUids = groupUids ?? currentGroupUids;

  const updateUser = useUpdateUser(targetUser.uid, {
    onSuccess: () => {
      toast.success(`User "${targetUser.username}" updated`);
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const wasAdmin = targetUser.roles?.includes("admin") ?? false;
  const isSelfDemotion =
    targetUser.uid === currentUserUid && wasAdmin && !roles.includes("admin");

  // Which directory-owned roles this edit moves. Saving them is allowed — an
  // admin may legitimately want the change until the next login — but it is
  // temporary, and that has to be said before the request goes out.
  const touchedManagedRoles = ROLE_OPTIONS.map((o) => o.value).filter(
    (role) =>
      managedRoles.has(role) &&
      roles.includes(role) !== (targetUser.roles?.includes(role) ?? false),
  );

  const submit = () => {
    updateUser.mutate({ roles, user_group_uids: selectedGroupUids });
  };

  // Self-demotion is the second gate rather than the first: the SSO warning is
  // about the change not lasting, and there is no point confirming a lockout
  // for an edit the operator abandons once they learn it will be reverted.
  const confirmSelfDemotionOrSubmit = () => {
    if (isSelfDemotion) {
      setConfirmSelfDemote(true);
      return;
    }
    submit();
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (touchedManagedRoles.length > 0) {
      setConfirmManagedRoles(true);
      return;
    }
    confirmSelfDemotionOrSubmit();
  };

  return (
    <>
      <DialogContent data-testid="edit-user-dialog">
        <form onSubmit={handleSubmit}>
          <DialogHeader>
            <DialogTitle>Edit User</DialogTitle>
            <DialogDescription>
              Manage roles for "{targetUser.username}".
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4 max-h-[60vh] overflow-y-auto">
            <div className="space-y-2">
              <Label htmlFor="edit-username">Username</Label>
              <Input
                id="edit-username"
                value={targetUser.username}
                readOnly
                disabled
                data-testid="edit-user-username"
              />
            </div>
            <RoleCheckboxes
              idPrefix="edit-user"
              roles={roles}
              onChange={setRoles}
              lockedRole={isLastAdmin ? "admin" : undefined}
              lockedReason="Cannot remove the admin role from the last administrator"
              managedRoles={managedRoles}
            />
            {lastRoleSync && <LastRoleSync event={lastRoleSync} />}
            <div className="space-y-2">
              <Label>Groups</Label>
              <p className="text-xs text-muted-foreground">
                Groups are organizational and gate which grant definitions this
                user can request. Roles above stay functional.
              </p>
              <MultiSelect
                options={groups.map((g) => ({ value: g.uid, label: g.name }))}
                selected={selectedGroupUids}
                onChange={setGroupUids}
                placeholder="No groups"
                emptyMessage="No user groups defined yet."
                testId="edit-user-groups"
              />
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={updateUser.isPending || roles.length === 0}
              data-testid="edit-user-submit"
            >
              Save
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>

      <AlertDialog
        open={confirmManagedRoles}
        onOpenChange={setConfirmManagedRoles}
      >
        <AlertDialogContent data-testid="edit-user-managed-roles-dialog">
          <AlertDialogHeader>
            <AlertDialogTitle>
              This change will be undone at the next login
            </AlertDialogTitle>
            <AlertDialogDescription>
              {touchedManagedRoles.length === 1
                ? `The "${touchedManagedRoles[0]}" role is`
                : `The ${touchedManagedRoles
                    .map((r) => `"${r}"`)
                    .join(" and ")} roles are`}{" "}
              managed by your identity provider: it resolves them from
              directory group membership every time "{targetUser.username}"
              signs in. Saving here changes them now, but the next SSO login
              will put them back the way the directory says. To make this
              stick, change the user's group membership in the directory.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel data-testid="edit-user-managed-roles-cancel">
              Cancel
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={confirmSelfDemotionOrSubmit}
              data-testid="edit-user-managed-roles-confirm"
            >
              Save anyway
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={confirmSelfDemote} onOpenChange={setConfirmSelfDemote}>
        <AlertDialogContent data-testid="edit-user-demote-self-dialog">
          <AlertDialogHeader>
            <AlertDialogTitle>Remove your own admin rights?</AlertDialogTitle>
            <AlertDialogDescription>
              You are about to remove the administrator role from your own
              account. You will immediately lose access to user management and
              other admin-only features.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel data-testid="edit-user-demote-self-cancel">
              Cancel
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={submit}
              className="bg-destructive text-white hover:bg-destructive/90"
              data-testid="edit-user-demote-self-confirm"
            >
              Remove my admin role
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
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
