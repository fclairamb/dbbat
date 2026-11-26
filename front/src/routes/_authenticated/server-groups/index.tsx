import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Plus, Pencil, Trash2 } from "lucide-react";
import { toast } from "sonner";

import {
  useServerGroups,
  useDeleteServerGroup,
  useDatabases,
  type ServerGroup,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { navItem } from "@/lib/navigation";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { Button } from "@/components/ui/button";
import { Dialog, DialogTrigger } from "@/components/ui/dialog";
import { ServerGroupForm } from "@/components/server-groups/ServerGroupForm";
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
import { canManageServerGroups } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/server-groups/")({
  component: ServerGroupsPage,
});

function ServerGroupsPage() {
  const { user } = useAuth();
  const [createOpen, setCreateOpen] = useState(false);
  const isAdmin = canManageServerGroups(user?.roles);

  const { data: groups = [], isLoading } = useServerGroups({ enabled: isAdmin });
  const { data: servers = [] } = useDatabases();

  const [deleting, setDeleting] = useState<ServerGroup | null>(null);

  const remove = useDeleteServerGroup({
    onSuccess: () => {
      toast.success("Server group deleted");
      setDeleting(null);
    },
    onError: (e) => toast.error(e.message),
  });

  const serverNameFor = (uid: string) =>
    servers.find((s) => s.uid === uid)?.name ?? uid.slice(0, 8);

  const columns: Column<ServerGroup>[] = [
    {
      key: "name",
      header: "Name",
      cell: (g: ServerGroup) => (
        <div className="flex flex-col">
          {isAdmin ? (
            <Link
              to="/server-groups/$uid"
              params={{ uid: g.uid }}
              className="font-medium hover:underline"
              data-testid={`server-group-link-${g.uid}`}
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
      header: "Servers",
      cell: (g: ServerGroup) =>
        g.member_uids.length === 0 ? (
          <span className="text-muted-foreground italic">none</span>
        ) : (
          <div className="flex gap-1 flex-wrap">
            {g.member_uids.map((uid) => (
              <span
                key={uid}
                className="text-xs bg-secondary px-1.5 py-0.5 rounded"
              >
                {serverNameFor(uid)}
              </span>
            ))}
          </div>
        ),
    },
    {
      key: "active_grant_count",
      header: "Live grants",
      cell: (g: ServerGroup) =>
        g.active_grant_count === 0 ? (
          <span className="text-muted-foreground italic">none</span>
        ) : (
          <span title="Grants bound to this group. Editing membership changes what they cover, immediately.">
            {g.active_grant_count}
          </span>
        ),
    },
    {
      key: "actions",
      header: "",
      cell: (g: ServerGroup) =>
        isAdmin ? (
          <div className="flex gap-1 justify-end">
            <Button
              asChild
              size="sm"
              variant="ghost"
              data-testid={`edit-server-group-${g.uid}`}
              aria-label={`Edit server group ${g.name}`}
            >
              <Link to="/server-groups/$uid" params={{ uid: g.uid }}>
                <Pencil className="h-4 w-4" />
              </Link>
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setDeleting(g)}
              data-testid={`delete-server-group-${g.uid}`}
              aria-label={`Delete server group ${g.name}`}
              className="text-destructive hover:text-destructive"
            >
              <Trash2 className="h-4 w-4" />
            </Button>
          </div>
        ) : null,
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        icon={navItem("/server-groups")!.icon}
        title="Server Groups"
        description="Named sets of database servers (the analytics replicas, all staging databases, …). Grant definitions scope on them, and grants bind to them — so membership is what decides reach."
        actions={
          isAdmin && (
            <Dialog open={createOpen} onOpenChange={setCreateOpen}>
              <DialogTrigger asChild>
                <Button data-testid="create-server-group-button">
                  <Plus className="h-4 w-4 mr-2" />
                  New Server Group
                </Button>
              </DialogTrigger>
              {createOpen && <ServerGroupForm editing={null} onDone={() => setCreateOpen(false)} />}
            </Dialog>
          )
        }
      />

      <DataTable
        columns={columns}
        data={groups}
        isLoading={isLoading}
        rowKey={(g: ServerGroup) => g.uid}
        emptyMessage="No server groups yet. Create one to scope grant definitions to a named set of servers instead of listing them one by one."
      />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete server group?</AlertDialogTitle>
            <AlertDialogDescription>
              "{deleting?.name}" and its memberships are removed. Grants bound
              to it fall back to the single database they were issued for —
              access narrows, never widens. Grant definitions scoped to this
              group will then match no database until you edit them: they fail
              closed.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => deleting && remove.mutate(deleting.uid)}
              data-testid="confirm-delete-server-group"
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
