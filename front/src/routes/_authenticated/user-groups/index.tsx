import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Plus, Pencil, Trash2 } from "lucide-react";
import { toast } from "sonner";

import {
  useUserGroups,
  useCreateUserGroup,
  useUpdateUserGroup,
  useDeleteUserGroup,
  useUsers,
  type UserGroup,
  type CreateUserGroupRequest,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { MultiSelect } from "@/components/shared/MultiSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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
import { useAuth } from "@/contexts/AuthContext";
import { canManageUserGroups } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/user-groups/")({
  component: UserGroupsPage,
});

function UserGroupsPage() {
  const { user } = useAuth();
  const isAdmin = canManageUserGroups(user?.roles);

  const { data: groups = [], isLoading } = useUserGroups({ enabled: isAdmin });
  const { data: users = [] } = useUsers();

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<UserGroup | null>(null);
  const [deleting, setDeleting] = useState<UserGroup | null>(null);

  const remove = useDeleteUserGroup({
    onSuccess: () => {
      toast.success("Group deleted");
      setDeleting(null);
    },
    onError: (e) => toast.error(e.message),
  });

  const usernameFor = (uid: string) =>
    users.find((u) => u.uid === uid)?.username ?? uid.slice(0, 8);

  const columns: Column<UserGroup>[] = [
    {
      key: "name",
      header: "Name",
      cell: (g: UserGroup) => (
        <div className="flex flex-col">
          <span className="font-medium">{g.name}</span>
          {g.description && (
            <span className="text-xs text-muted-foreground">
              {g.description}
            </span>
          )}
        </div>
      ),
    },
    {
      key: "member_uids",
      header: "Members",
      cell: (g: UserGroup) =>
        g.member_uids.length === 0 ? (
          <span className="text-muted-foreground italic">none</span>
        ) : (
          <div className="flex gap-1 flex-wrap">
            {g.member_uids.map((uid) => (
              <span
                key={uid}
                className="text-xs bg-secondary px-1.5 py-0.5 rounded"
              >
                {usernameFor(uid)}
              </span>
            ))}
          </div>
        ),
    },
    {
      key: "actions",
      header: "",
      cell: (g: UserGroup) =>
        isAdmin ? (
          <div className="flex gap-1 justify-end">
            <Button
              size="sm"
              variant="ghost"
              onClick={() => {
                setEditing(g);
                setDialogOpen(true);
              }}
              data-testid={`edit-user-group-${g.uid}`}
            >
              <Pencil className="h-4 w-4" />
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setDeleting(g)}
              data-testid={`delete-user-group-${g.uid}`}
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
        title="User Groups"
        description="Organizational groups (data-analysts, SRE, …) used to scope grant definitions. Distinct from roles, which are functional."
        actions={
          isAdmin && (
            <Dialog
              open={dialogOpen}
              onOpenChange={(o) => {
                setDialogOpen(o);
                if (!o) setEditing(null);
              }}
            >
              <DialogTrigger asChild>
                <Button data-testid="create-user-group-button">
                  <Plus className="h-4 w-4 mr-2" />
                  New Group
                </Button>
              </DialogTrigger>
              {dialogOpen && (
                <GroupDialog
                  key={editing?.uid ?? "new"}
                  editing={editing}
                  onClose={() => {
                    setDialogOpen(false);
                    setEditing(null);
                  }}
                />
              )}
            </Dialog>
          )
        }
      />

      <DataTable
        columns={columns}
        data={groups}
        isLoading={isLoading}
        rowKey={(g: UserGroup) => g.uid}
        emptyMessage="No user groups yet. Create one to scope grant definitions to a subset of users."
      />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete group?</AlertDialogTitle>
            <AlertDialogDescription>
              "{deleting?.name}" and its memberships are removed. Grant
              definitions scoped to this group will then match nobody until you
              edit them — they fail closed, never open.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => deleting && remove.mutate(deleting.uid)}
              data-testid="confirm-delete-user-group"
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function GroupDialog({
  editing,
  onClose,
}: {
  editing: UserGroup | null;
  onClose: () => void;
}) {
  const { data: users = [] } = useUsers();

  const [name, setName] = useState(editing?.name ?? "");
  const [description, setDescription] = useState(editing?.description ?? "");
  const [memberUids, setMemberUids] = useState<string[]>(
    editing?.member_uids ?? []
  );

  const create = useCreateUserGroup({
    onSuccess: () => {
      toast.success("Group created");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });
  const update = useUpdateUserGroup({
    onSuccess: () => {
      toast.success("Group updated");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    const body: CreateUserGroupRequest = {
      name,
      description,
      member_uids: memberUids,
    };

    if (editing) {
      update.mutate({ uid: editing.uid, body });
    } else {
      create.mutate(body);
    }
  };

  return (
    <DialogContent>
      <form onSubmit={onSubmit}>
        <DialogHeader>
          <DialogTitle>{editing ? "Edit group" : "New group"}</DialogTitle>
          <DialogDescription>
            Groups let a grant definition apply to a subset of users instead of
            everyone.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label htmlFor="group-name">Name</Label>
            <Input
              id="group-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="data-analysts"
              maxLength={64}
              required
              data-testid="user-group-name"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="group-desc">Description</Label>
            <Input
              id="group-desc"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Analysts who self-serve read-only access"
              data-testid="user-group-description"
            />
          </div>
          <div className="space-y-2">
            <Label>Members</Label>
            <MultiSelect
              options={users.map((u) => ({ value: u.uid, label: u.username }))}
              selected={memberUids}
              onChange={setMemberUids}
              placeholder="No members"
              testId="user-group-members"
            />
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={create.isPending || update.isPending}
            data-testid="user-group-submit"
          >
            {editing ? "Save" : "Create"}
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}
