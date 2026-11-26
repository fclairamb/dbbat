import { useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";

import {
  useCreateUser,
  useUpdateUser,
  useUserGroups,
  type User,
  type UserRoleSync,
} from "@/api";
import { FormPage } from "@/components/shared/FormPage";
import { MultiSelect } from "@/components/shared/MultiSelect";
import {
  LastRoleSync,
  ManagedRoleBadge,
} from "@/components/shared/SsoRoleSync";
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
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import type { UserRole } from "@/lib/permissions";

export const ROLE_OPTIONS: { value: UserRole; label: string }[] = [
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

/** Create dialog for a user (opened from the list). */
export function CreateUserDialog({ onClose }: { onClose: () => void }) {
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
          <DialogTitle>New User</DialogTitle>
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

/** Edit form for a user (the `$uid` route). */
export function EditUserForm({
  user: targetUser,
  currentUserUid,
  isLastAdmin,
  managedRoles,
  lastRoleSync,
}: {
  user: User;
  currentUserUid?: string;
  isLastAdmin: boolean;
  managedRoles: Set<string>;
  lastRoleSync?: UserRoleSync;
}) {
  const navigate = useNavigate();
  const onClose = () => navigate({ to: "/users" });
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
      <FormPage
        testId="edit-user-page"
        title={targetUser.username}
        description="Roles and user groups of this account."
        backTo="/users"
        backLabel="Users"
        onSubmit={handleSubmit}
        onCancel={onClose}
        submitLabel="Save"
        submitTestId="edit-user-submit"
        submitting={updateUser.isPending}
        submitDisabled={roles.length === 0}
      >
        <div className="max-w-2xl space-y-4">
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
          {lastRoleSync && <LastRoleSync sync={lastRoleSync} />}
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
      </FormPage>

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

