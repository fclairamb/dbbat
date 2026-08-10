import { useMemo, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Plus, Check, X, Ban, ShieldCheck } from "lucide-react";
import { toast } from "sonner";

import {
  useGrantRequests,
  useGrantDefinitions,
  useDatabases,
  useUsers,
  useCreateGrantRequest,
  useApproveGrantRequest,
  useDenyGrantRequest,
  useCancelGrantRequest,
  useUpdateGrantDefinition,
  type GrantRequest,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useAuth } from "@/contexts/AuthContext";
import { canApproveGrantRequest, canRequestGrant } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/grant-requests/")({
  component: GrantRequestsPage,
});

const STATUS_BADGE: Record<GrantRequest["status"], string> = {
  pending: "bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400",
  approved: "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400",
  denied: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400",
  cancelled: "bg-muted text-muted-foreground",
  expired: "bg-muted text-muted-foreground",
};

function GrantRequestsPage() {
  const { user } = useAuth();
  const isAdmin = canApproveGrantRequest(user?.roles);
  const canRequest = canRequestGrant(user?.roles);

  const [tab, setTab] = useState<"pending" | "all">("pending");
  const [createOpen, setCreateOpen] = useState(false);
  const [denying, setDenying] = useState<GrantRequest | null>(null);

  const { data: requests = [], isLoading } = useGrantRequests(
    isAdmin && tab === "pending" ? { status: "pending" } : {}
  );

  const { data: users = [] } = useUsers();
  const { data: databases = [] } = useDatabases();

  const userMap = useMemo(
    () => Object.fromEntries((users ?? []).map((u) => [u.uid, u.username])),
    [users]
  );
  const dbMap = useMemo(
    () => Object.fromEntries((databases ?? []).map((d) => [d.uid, d.name])),
    [databases]
  );

  const approve = useApproveGrantRequest({
    onSuccess: () => toast.success("Approved"),
    onError: (e) => toast.error(e.message),
  });
  const cancel = useCancelGrantRequest({
    onSuccess: () => toast.success("Cancelled"),
    onError: (e) => toast.error(e.message),
  });
  const updateDefinition = useUpdateGrantDefinition({
    onError: (e) => toast.error(e.message),
  });

  // Approve a pending request and, in the same action, flip its definition to
  // auto-approve so future requests against it skip review entirely.
  //
  // The PATCH is a genuine partial update, so it carries only the field being
  // changed: re-sending the whole shape would risk an edit racing this one
  // getting silently rolled back, and every edit is versioned, so a definition
  // is never mutated in place — this archives the current version and inserts
  // a successor. The request row is re-read afterwards (the approve mutation
  // invalidates it) and comes back carrying that successor.
  const approveAndEnableAutoApprove = (r: GrantRequest) => {
    const def = r.definition;
    if (!def) return;
    updateDefinition.mutate(
      { uid: def.uid, body: { auto_approve: true } },
      {
        onSuccess: () => {
          toast.success("Auto-approve enabled for this definition");
          approve.mutate(r.uid);
        },
      }
    );
  };

  const columns: Column<GrantRequest>[] = [
    {
      key: "user",
      header: "User",
      cell: (r: GrantRequest) => userMap[r.user_id] ?? r.user_id.slice(0, 8),
    },
    {
      key: "database",
      header: "Database",
      cell: (r: GrantRequest) => dbMap[r.database_id] ?? r.database_id.slice(0, 8),
    },
    {
      key: "definition",
      header: "Definition",
      cell: (r: GrantRequest) => {
        // The live version of the request's definition, embedded by the API.
        // Resolving grant_definition_id against the definitions listing would
        // miss every request whose definition has been edited since — that uid
        // names the archived version, which no listing returns.
        const def = r.definition;
        return (
          <div className="flex items-center gap-1.5">
            <span>{def?.name ?? r.grant_definition_id.slice(0, 8)}</span>
            {def?.auto_approve && (
              <span
                className="text-xs bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400 px-1.5 py-0.5 rounded"
                data-testid={`request-definition-auto-approve-${r.uid}`}
              >
                auto-approve
              </span>
            )}
          </div>
        );
      },
    },
    {
      key: "justification",
      header: "Justification",
      cell: (r: GrantRequest) =>
        r.justification ? (
          <span className="text-sm">{r.justification}</span>
        ) : (
          <span className="text-xs text-muted-foreground italic">none</span>
        ),
    },
    {
      key: "status",
      header: "Status",
      cell: (r: GrantRequest) => (
        <span
          className={`text-xs px-1.5 py-0.5 rounded ${STATUS_BADGE[r.status]}`}
        >
          {r.status}
        </span>
      ),
    },
    {
      key: "requested_at",
      header: "Requested",
      cell: (r: GrantRequest) =>
        r.requested_at ? new Date(r.requested_at).toLocaleString() : "",
    },
    {
      key: "actions",
      header: "",
      cell: (r: GrantRequest) =>
        r.status !== "pending" ? null : (
          <div className="flex gap-1 justify-end">
            {isAdmin && (
              <>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => approve.mutate(r.uid)}
                  data-testid={`approve-${r.uid}`}
                  title="Approve"
                >
                  <Check className="h-4 w-4 text-green-600" />
                </Button>
                {r.definition && !r.definition.auto_approve && (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => approveAndEnableAutoApprove(r)}
                        disabled={
                          updateDefinition.isPending || approve.isPending
                        }
                        data-testid={`approve-and-enable-auto-approve-${r.uid}`}
                        title="Approve and enable auto-approve for this definition"
                      >
                        <ShieldCheck className="h-4 w-4 text-blue-600" />
                      </Button>
                    </TooltipTrigger>
                    <TooltipContent>
                      Approve this request and enable auto-approve on "
                      {r.definition?.name ?? "this definition"}
                      " so future requests skip review.
                    </TooltipContent>
                  </Tooltip>
                )}
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setDenying(r)}
                  data-testid={`deny-${r.uid}`}
                  title="Deny"
                >
                  <X className="h-4 w-4 text-red-600" />
                </Button>
              </>
            )}
            {(isAdmin || r.user_id === user?.uid) && (
              <Button
                size="sm"
                variant="ghost"
                onClick={() => cancel.mutate(r.uid)}
                data-testid={`cancel-${r.uid}`}
                title="Cancel"
              >
                <Ban className="h-4 w-4" />
              </Button>
            )}
          </div>
        ),
    },
  ];

  return (
    <div className="container mx-auto py-6">
      <PageHeader
        title="Grant Requests"
        description={
          isAdmin
            ? "Approve or deny grant requests submitted by users."
            : "Track the status of your grant requests."
        }
        actions={
          canRequest && (
            <Dialog open={createOpen} onOpenChange={setCreateOpen}>
              <DialogTrigger asChild>
                <Button data-testid="request-grant-button">
                  <Plus className="h-4 w-4 mr-2" />
                  Request access
                </Button>
              </DialogTrigger>
              <CreateRequestDialog onClose={() => setCreateOpen(false)} />
            </Dialog>
          )
        }
      />

      {isAdmin && (
        <div className="flex gap-2 mb-4">
          <Button
            size="sm"
            variant={tab === "pending" ? "default" : "outline"}
            onClick={() => setTab("pending")}
          >
            Pending
          </Button>
          <Button
            size="sm"
            variant={tab === "all" ? "default" : "outline"}
            onClick={() => setTab("all")}
          >
            All
          </Button>
        </div>
      )}

      <DataTable
        columns={columns}
        data={requests}
        isLoading={isLoading}
        rowKey={(r: GrantRequest) => r.uid}
        emptyMessage={
          isAdmin && tab === "pending"
            ? "No pending requests."
            : "No grant requests yet."
        }
      />

      <DenyDialog
        request={denying}
        onClose={() => setDenying(null)}
      />
    </div>
  );
}

function CreateRequestDialog({ onClose }: { onClose: () => void }) {
  const { data: definitions = [] } = useGrantDefinitions({ active_only: true });
  const { data: databases = [] } = useDatabases();

  const [definitionId, setDefinitionId] = useState("");
  const [databaseId, setDatabaseId] = useState("");
  const [justification, setJustification] = useState("");

  const selectedDefinition = definitions.find((d) => d.uid === definitionId);
  const justificationRequired = selectedDefinition?.auto_approve ?? false;

  // The API resolves a definition's server-group scope to the concrete
  // databases currently in scope (scoped_database_uids), so the picker
  // narrows for every requester — not just admins, who are the only ones who
  // could previously read /server-groups to compute this themselves.
  //
  // null/omitted means the definition is unscoped: every database is in
  // scope. A present array — which may be empty — is the resolved set;
  // treating null and [] the same here would be wrong in both directions
  // (hiding every database, or offering every database when none are
  // actually in scope). The server still enforces the scope on submit (403
  // on an out-of-scope pick) — this is a convenience, never the control.
  const scopedDatabaseUids = selectedDefinition?.scoped_database_uids ?? null;
  const selectableDatabases =
    scopedDatabaseUids === null
      ? databases
      : databases.filter((d) => scopedDatabaseUids.includes(d.uid));

  // Switching to a definition that excludes the currently picked database
  // must clear the stale selection rather than submit it.
  if (
    databaseId &&
    !selectableDatabases.some((d) => d.uid === databaseId)
  ) {
    setDatabaseId("");
  }

  const create = useCreateGrantRequest({
    onSuccess: (req) => {
      toast.success(
        req.status === "approved"
          ? "Request submitted and auto-approved — access is active now."
          : "Request submitted"
      );
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    create.mutate({
      grant_definition_id: definitionId,
      database_id: databaseId,
      justification,
    });
  };

  return (
    <DialogContent>
      <form onSubmit={onSubmit}>
        <DialogHeader>
          <DialogTitle>Request access</DialogTitle>
          <DialogDescription>
            {justificationRequired
              ? "This definition auto-approves requests — access will be active immediately after you submit."
              : "Pick a grant definition and a database. An admin will approve or deny."}
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label>Definition</Label>
            <Select
              value={definitionId}
              onValueChange={setDefinitionId}
              required
            >
              <SelectTrigger data-testid="grant-request-definition">
                <SelectValue placeholder="Select a definition" />
              </SelectTrigger>
              <SelectContent>
                {definitions.map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label>Database</Label>
            <Select
              value={databaseId}
              onValueChange={setDatabaseId}
              required
            >
              <SelectTrigger data-testid="grant-request-database">
                <SelectValue
                  placeholder={
                    selectableDatabases.length === 0 && definitionId
                      ? "No database in this definition's scope"
                      : "Select a database"
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {selectableDatabases.map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="justification">
              Justification {justificationRequired ? "" : "(optional)"}
            </Label>
            <Textarea
              id="justification"
              value={justification}
              onChange={(e) => setJustification(e.target.value)}
              maxLength={1000}
              required={justificationRequired}
              placeholder="Investigating bug X, need read-only access for an hour."
              data-testid="grant-request-justification"
            />
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={
              create.isPending ||
              !definitionId ||
              !databaseId ||
              (justificationRequired && !justification.trim())
            }
            data-testid="grant-request-submit"
          >
            Submit
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

function DenyDialog({
  request,
  onClose,
}: {
  request: GrantRequest | null;
  onClose: () => void;
}) {
  const [reason, setReason] = useState("");

  const deny = useDenyGrantRequest({
    onSuccess: () => {
      toast.success("Denied");
      setReason("");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });

  return (
    <Dialog open={!!request} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deny grant request?</DialogTitle>
          <DialogDescription>
            Optionally include a reason; the requester will see it on their
            request page.
          </DialogDescription>
        </DialogHeader>
        <div className="py-2">
          <Label htmlFor="deny-reason">Reason (optional)</Label>
          <Textarea
            id="deny-reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder="Out of scope; please open a security review ticket first."
          />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="destructive"
            onClick={() =>
              request && deny.mutate({ uid: request.uid, reason })
            }
          >
            Deny
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
