import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Plus, Pencil, Trash2 } from "lucide-react";
import { toast } from "sonner";

import {
  useUserGroups,
  useDeleteUserGroup,
  useUsers,
  type UserGroup,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { Button } from "@/components/ui/button";
import { Dialog, DialogTrigger } from "@/components/ui/dialog";
import { UserGroupForm } from "@/components/user-groups/UserGroupForm";
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
  const [createOpen, setCreateOpen] = useState(false);
  const isAdmin = canManageUserGroups(user?.roles);

  const { data: groups = [], isLoading } = useUserGroups({ enabled: isAdmin });
  const { data: users = [] } = useUsers();

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
          {isAdmin ? (
            <Link
              to="/user-groups/$uid"
              params={{ uid: g.uid }}
              className="font-medium hover:underline"
              data-testid={`user-group-link-${g.uid}`}
            >
              {g.name}
            </Link>
          ) : (
            <span className="font-medium">{g.name}</span>
          )}
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
              asChild
              size="sm"
              variant="ghost"
              data-testid={`edit-user-group-${g.uid}`}
            >
              <Link to="/user-groups/$uid" params={{ uid: g.uid }}>
                <Pencil className="h-4 w-4" />
              </Link>
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
            <Dialog open={createOpen} onOpenChange={setCreateOpen}>
              <DialogTrigger asChild>
                <Button data-testid="create-user-group-button">
                  <Plus className="h-4 w-4 mr-2" />
                  New Group
                </Button>
              </DialogTrigger>
              {createOpen && <UserGroupForm editing={null} onDone={() => setCreateOpen(false)} />}
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
