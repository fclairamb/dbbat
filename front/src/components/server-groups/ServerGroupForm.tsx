import { useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { AlertTriangle } from "lucide-react";
import { toast } from "sonner";

import {
  useCreateServerGroup,
  useUpdateServerGroup,
  useDatabases,
  useInstance,
  type ServerGroup,
  type CreateServerGroupRequest,
} from "@/api";
import { FormPage } from "@/components/shared/FormPage";
import { MultiSelect } from "@/components/shared/MultiSelect";
import {
  RetentionFields,
  type RetentionSeconds,
} from "@/components/shared/RetentionDuration";
import { ApproverGroupPickers } from "@/components/shared/ApproverGroupPickers";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

/** Create/edit form for a server group, shared by `new` and `$uid`. */
export function ServerGroupForm({ editing }: { editing: ServerGroup | null }) {
  const navigate = useNavigate();
  const { data: servers = [] } = useDatabases();
  const onClose = () => navigate({ to: "/server-groups" });

  const [name, setName] = useState(editing?.name ?? "");
  const [description, setDescription] = useState(editing?.description ?? "");
  const [memberUids, setMemberUids] = useState<string[]>(
    editing?.member_uids ?? []
  );
  const [accessApproverUids, setAccessApproverUids] = useState<string[]>(
    editing?.access_approver_user_group_uids ?? []
  );
  const [queryApproverUids, setQueryApproverUids] = useState<string[]>(
    editing?.query_approver_user_group_uids ?? []
  );

  const { data: instance } = useInstance();
  const archiveEnabled = instance?.resolved_retention?.archive_enabled ?? false;
  const [archiveAfter, setArchiveAfter] = useState<RetentionSeconds>(
    editing?.archive_after_seconds ?? null
  );
  const [deleteAfter, setDeleteAfter] = useState<RetentionSeconds>(
    editing?.delete_after_seconds ?? null
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
      access_approver_user_group_uids: accessApproverUids,
      query_approver_user_group_uids: queryApproverUids,
    };

    // Absent leaves a window alone, so only what changed goes on the wire.
    if (archiveAfter !== (editing?.archive_after_seconds ?? null)) {
      body.archive_after_seconds = archiveAfter;
    }
    if (deleteAfter !== (editing?.delete_after_seconds ?? null)) {
      body.delete_after_seconds = deleteAfter;
    }

    if (editing) {
      update.mutate({ uid: editing.uid, body });
    } else {
      create.mutate(body);
    }
  };

  return (
    <FormPage
      testId={editing ? "edit-server-group-page" : "new-server-group-page"}
      title={editing ? "Edit server group" : "New server group"}
      description="A grant definition scopes on server groups instead of listing servers, and a grant binds to the group it was issued under."
      backTo="/server-groups"
      backLabel="Server Groups"
      onSubmit={onSubmit}
      onCancel={onClose}
      submitLabel={editing ? "Save" : "Create"}
      submitTestId="server-group-submit"
      submitting={create.isPending || update.isPending}
    >
      <div className="max-w-3xl space-y-6">
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
                    narrows them, even if it is the database a grant was
                    originally issued for.
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
          {/*
            The group-level fallback for both approver kinds. A server that
            names its own approvers ignores these entirely; several groups
            holding the same server union their lists.
          */}
          <ApproverGroupPickers
            scope="group"
            accessSelected={accessApproverUids}
            onAccessChange={setAccessApproverUids}
            querySelected={queryApproverUids}
            onQueryChange={setQueryApproverUids}
            testIdPrefix="server-group"
          />
          <RetentionFields
            archive={archiveAfter}
            onArchiveChange={setArchiveAfter}
            del={deleteAfter}
            onDeleteChange={setDeleteAfter}
            originalDelete={editing?.delete_after_seconds}
            archiveEnabled={archiveEnabled}
            testIdPrefix="server-group"
            inheritNote="A server's own window overrides the group's. When several groups hold a server, the longest delete window and the shortest archive window win."
          />
      </div>
    </FormPage>
  );
}
