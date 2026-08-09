import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Plus, Pencil, Trash2, AlertTriangle } from "lucide-react";
import { toast } from "sonner";

import {
  useServerGroups,
  useCreateServerGroup,
  useUpdateServerGroup,
  useDeleteServerGroup,
  useDatabases,
  type ServerGroup,
  type CreateServerGroupRequest,
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
import { canManageServerGroups } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/server-groups/")({
  component: ServerGroupsPage,
});

function ServerGroupsPage() {
  const { user } = useAuth();
  const isAdmin = canManageServerGroups(user?.roles);

  const { data: groups = [], isLoading } = useServerGroups({ enabled: isAdmin });
  const { data: servers = [] } = useDatabases();

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<ServerGroup | null>(null);
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
              size="sm"
              variant="ghost"
              onClick={() => {
                setEditing(g);
                setDialogOpen(true);
              }}
              data-testid={`edit-server-group-${g.uid}`}
            >
              <Pencil className="h-4 w-4" />
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setDeleting(g)}
              data-testid={`delete-server-group-${g.uid}`}
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
        title="Server Groups"
        description="Named sets of database servers (the analytics replicas, all staging databases, …). Grant definitions scope on them, and grants bind to them — so membership is what decides reach."
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
                <Button data-testid="create-server-group-button">
                  <Plus className="h-4 w-4 mr-2" />
                  New Group
                </Button>
              </DialogTrigger>
              {dialogOpen && (
                <ServerGroupDialog
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

function ServerGroupDialog({
  editing,
  onClose,
}: {
  editing: ServerGroup | null;
  onClose: () => void;
}) {
  const { data: servers = [] } = useDatabases();

  const [name, setName] = useState(editing?.name ?? "");
  const [description, setDescription] = useState(editing?.description ?? "");
  const [memberUids, setMemberUids] = useState<string[]>(
    editing?.member_uids ?? []
  );

  const create = useCreateServerGroup({
    onSuccess: () => {
      toast.success("Server group created");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });
  const update = useUpdateServerGroup({
    onSuccess: () => {
      toast.success("Server group updated");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });

  const liveGrants = editing?.active_grant_count ?? 0;
  const originalMembers = editing?.member_uids ?? [];
  const added = memberUids.filter((uid) => !originalMembers.includes(uid));
  const removed = originalMembers.filter((uid) => !memberUids.includes(uid));
  const membershipChanged = added.length > 0 || removed.length > 0;

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    const body: CreateServerGroupRequest = {
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
          <DialogTitle>
            {editing ? "Edit server group" : "New server group"}
          </DialogTitle>
          <DialogDescription>
            A grant definition scopes on server groups instead of listing
            servers, and a grant binds to the group it was issued under.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4 max-h-[60vh] overflow-y-auto">
          <div className="space-y-2">
            <Label htmlFor="server-group-name">Name</Label>
            <Input
              id="server-group-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="analytics-replicas"
              maxLength={64}
              required
              data-testid="server-group-name"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="server-group-desc">Description</Label>
            <Input
              id="server-group-desc"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Read replicas the analysts self-serve against"
              data-testid="server-group-description"
            />
          </div>
          <div className="space-y-2">
            <Label>Servers</Label>
            <MultiSelect
              options={servers.map((s) => ({ value: s.uid, label: s.name }))}
              selected={memberUids}
              onChange={setMemberUids}
              placeholder="No servers"
              testId="server-group-members"
            />
            {/*
              Membership is live: it is not versioned and there is no
              re-issuance step, so the moment this saves, every grant bound to
              this group covers a different set of servers. That is the one
              place dbbat's "a live grant's behaviour never changes under it"
              rule is deliberately broken, so the blast radius is stated here,
              before the operator commits to it.
            */}
            {liveGrants > 0 && (
              <div
                className="flex gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs"
                data-testid="server-group-live-membership-warning"
              >
                <AlertTriangle className="h-4 w-4 shrink-0 text-amber-600" />
                <div className="space-y-1">
                  <p className="font-medium">
                    Membership takes effect immediately on {liveGrants} live
                    grant{liveGrants > 1 ? "s" : ""}.
                  </p>
                  <p className="text-muted-foreground">
                    Server groups are not versioned. Adding a server widens
                    every grant bound to this group the instant you save —
                    including sessions already running — and removing one
                    narrows them.
                    {membershipChanged && (
                      <>
                        {" "}
                        This edit adds {added.length} and removes{" "}
                        {removed.length}.
                      </>
                    )}
                  </p>
                </div>
              </div>
            )}
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={create.isPending || update.isPending}
            data-testid="server-group-submit"
          >
            {editing ? "Save" : "Create"}
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}
