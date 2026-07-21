import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Plus, Pencil, Trash2 } from "lucide-react";
import { toast } from "sonner";

import {
  useDatabases,
  useUserGroups,
  useGrantDefinitions,
  useCreateGrantDefinition,
  useUpdateGrantDefinition,
  useDeactivateGrantDefinition,
  type GrantDefinition,
  type CreateGrantDefinitionRequest,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { MultiSelect } from "@/components/shared/MultiSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Switch } from "@/components/ui/switch";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useAuth } from "@/contexts/AuthContext";
import { canManageGrantDefinitions } from "@/lib/permissions";
import { UsageLimit } from "@/components/shared/UsageMeter";
import { formatBytes } from "@/lib/utils";

export const Route = createFileRoute("/_authenticated/grant-definitions/")({
  component: GrantDefinitionsPage,
});

const CONTROLS = [
  { value: "read_only", label: "Read Only" },
  { value: "block_copy", label: "Block COPY" },
  { value: "block_ddl", label: "Block DDL" },
];

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

  // Admins see all definitions (active+inactive); other roles never reach
  // this page in the nav, but the API also enforces the active-only filter
  // for non-admins as a defense-in-depth.
  const { data: definitions = [], isLoading } = useGrantDefinitions({
    active_only: !isAdmin,
  });

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<GrantDefinition | null>(null);
  const [deactivating, setDeactivating] = useState<GrantDefinition | null>(
    null
  );
  const [confirmingAutoApprove, setConfirmingAutoApprove] =
    useState<GrantDefinition | null>(null);

  const deactivate = useDeactivateGrantDefinition({
    onSuccess: () => {
      toast.success("Definition deactivated");
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
    const body: CreateGrantDefinitionRequest = {
      name: d.name,
      description: d.description,
      duration_seconds: d.duration_seconds,
      controls: d.controls,
      max_query_counts: d.max_query_counts,
      max_bytes_transferred: d.max_bytes_transferred,
      auto_approve: next,
      // Preserve scope: this is a targeted toggle, not a full edit.
      group_uids: d.group_uids,
      database_uids: d.database_uids,
    };
    updateAutoApprove.mutate({ uid: d.uid, body });
  };

  const columns: Column<GrantDefinition>[] = [
    {
      key: "name",
      header: "Name",
      cell: (d: GrantDefinition) => (
        <div className="flex flex-col">
          <span className="font-medium">{d.name}</span>
          {d.description && (
            <span className="text-xs text-muted-foreground">
              {d.description}
            </span>
          )}
        </div>
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
            {d.group_uids.length === 0 ? (
              <span className="text-muted-foreground italic">all users</span>
            ) : (
              `${d.group_uids.length} group${d.group_uids.length > 1 ? "s" : ""}`
            )}
          </span>
          <span>
            {d.database_uids.length === 0 ? (
              <span className="text-muted-foreground italic">
                all databases
              </span>
            ) : (
              `${d.database_uids.length} database${
                d.database_uids.length > 1 ? "s" : ""
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
              size="sm"
              variant="ghost"
              onClick={() => {
                setEditing(d);
                setDialogOpen(true);
              }}
              data-testid={`edit-grant-definition-${d.uid}`}
            >
              <Pencil className="h-4 w-4" />
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
            <Dialog
              open={dialogOpen}
              onOpenChange={(o) => {
                setDialogOpen(o);
                if (!o) setEditing(null);
              }}
            >
              <DialogTrigger asChild>
                <Button data-testid="create-grant-definition-button">
                  <Plus className="h-4 w-4 mr-2" />
                  New Definition
                </Button>
              </DialogTrigger>
              {dialogOpen && (
                <DefinitionDialog
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
            <AlertDialogDescription>
              "{deactivating?.name}" will no longer appear in the request UI.
              Existing grants and pending requests are unaffected.
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

function DefinitionDialog({
  editing,
  onClose,
}: {
  editing: GrantDefinition | null;
  onClose: () => void;
}) {
  const [name, setName] = useState(editing?.name ?? "");
  const [description, setDescription] = useState(editing?.description ?? "");
  const [durationValue, setDurationValue] = useState<string>(() => {
    if (!editing) return "1";
    const s = editing.duration_seconds;
    if (s % 86400 === 0) return String(s / 86400);
    if (s % 3600 === 0) return String(s / 3600);
    return String(Math.max(1, Math.floor(s / 60)));
  });
  const [durationUnit, setDurationUnit] = useState<"m" | "h" | "d">(() => {
    if (!editing) return "h";
    const s = editing.duration_seconds;
    if (s % 86400 === 0) return "d";
    if (s % 3600 === 0) return "h";
    return "m";
  });
  const [controls, setControls] = useState<string[]>(editing?.controls ?? []);
  const [groupUids, setGroupUids] = useState<string[]>(
    editing?.group_uids ?? []
  );
  const [databaseUids, setDatabaseUids] = useState<string[]>(
    editing?.database_uids ?? []
  );
  const { data: groups = [] } = useUserGroups();
  const { data: databases = [] } = useDatabases();
  const [autoApprove, setAutoApprove] = useState(
    editing?.auto_approve ?? false
  );
  const [maxQueries, setMaxQueries] = useState<string>(
    editing?.max_query_counts != null ? String(editing.max_query_counts) : ""
  );
  const [maxBytesValue, setMaxBytesValue] = useState<string>(() => {
    if (editing?.max_bytes_transferred == null) return "";
    const v = editing.max_bytes_transferred;
    if (v >= 1024 * 1024 * 1024) return String(v / (1024 * 1024 * 1024));
    if (v >= 1024 * 1024) return String(v / (1024 * 1024));
    return String(v / 1024);
  });
  const [bytesUnit, setBytesUnit] = useState<"KB" | "MB" | "GB">(() => {
    if (editing?.max_bytes_transferred == null) return "MB";
    const v = editing.max_bytes_transferred;
    if (v >= 1024 * 1024 * 1024) return "GB";
    if (v >= 1024 * 1024) return "MB";
    return "KB";
  });

  const create = useCreateGrantDefinition({
    onSuccess: () => {
      toast.success("Definition created");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });
  const update = useUpdateGrantDefinition({
    onSuccess: () => {
      toast.success("Definition updated");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });

  const toggleControl = (v: string) =>
    setControls((prev) =>
      prev.includes(v) ? prev.filter((c) => c !== v) : [...prev, v]
    );

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    const durationSeconds =
      parseInt(durationValue || "0") *
      (durationUnit === "d" ? 86400 : durationUnit === "h" ? 3600 : 60);

    const unitMult =
      bytesUnit === "GB"
        ? 1024 * 1024 * 1024
        : bytesUnit === "MB"
          ? 1024 * 1024
          : 1024;

    const body: CreateGrantDefinitionRequest = {
      name,
      description,
      duration_seconds: durationSeconds,
      controls: controls as ("read_only" | "block_copy" | "block_ddl")[],
      max_query_counts: maxQueries ? parseInt(maxQueries) : null,
      max_bytes_transferred: maxBytesValue
        ? parseInt(maxBytesValue) * unitMult
        : null,
      auto_approve: autoApprove,
      group_uids: groupUids,
      database_uids: databaseUids,
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
            {editing ? "Edit definition" : "New definition"}
          </DialogTitle>
          <DialogDescription>
            Definitions are templates for the grant request workflow. Users will
            request grants by picking one.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label htmlFor="def-name">Name</Label>
            <Input
              id="def-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Read-only 1h"
              maxLength={64}
              required
              data-testid="grant-definition-name"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="def-desc">Description</Label>
            <Input
              id="def-desc"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Standard read access for an hour"
              data-testid="grant-definition-description"
            />
          </div>
          <div className="space-y-2">
            <Label>Duration</Label>
            <div className="flex gap-2">
              <Input
                type="number"
                min="1"
                value={durationValue}
                onChange={(e) => setDurationValue(e.target.value)}
                className="flex-1"
                required
                data-testid="grant-definition-duration-value"
              />
              <Select
                value={durationUnit}
                onValueChange={(v) => setDurationUnit(v as "m" | "h" | "d")}
              >
                <SelectTrigger
                  className="w-28"
                  data-testid="grant-definition-duration-unit"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="m">Minutes</SelectItem>
                  <SelectItem value="h">Hours</SelectItem>
                  <SelectItem value="d">Days</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="space-y-2">
            <Label>Access Controls</Label>
            <div className="space-y-2">
              {CONTROLS.map((c) => (
                <div key={c.value} className="flex items-center gap-2">
                  <Checkbox
                    id={`def-${c.value}`}
                    checked={controls.includes(c.value)}
                    onCheckedChange={() => toggleControl(c.value)}
                  />
                  <Label htmlFor={`def-${c.value}`} className="cursor-pointer">
                    {c.label}
                  </Label>
                </div>
              ))}
            </div>
          </div>
          <div className="space-y-2">
            <Label>Restrict to groups</Label>
            <p className="text-xs text-muted-foreground">
              Leave empty to let every user request this definition.
            </p>
            <MultiSelect
              options={groups.map((g) => ({ value: g.uid, label: g.name }))}
              selected={groupUids}
              onChange={setGroupUids}
              placeholder="Every user"
              emptyMessage="No groups defined yet — this definition applies to every user."
              testId="grant-definition-groups"
            />
          </div>
          <div className="space-y-2">
            <Label>Restrict to databases</Label>
            <p className="text-xs text-muted-foreground">
              Leave empty to allow this definition against every database.
            </p>
            <MultiSelect
              options={databases.map((d) => ({ value: d.uid, label: d.name }))}
              selected={databaseUids}
              onChange={setDatabaseUids}
              placeholder="Every database"
              emptyMessage="No databases configured yet."
              testId="grant-definition-databases"
            />
          </div>
          <div className="flex items-center gap-2">
            <Checkbox
              id="def-auto-approve"
              checked={autoApprove}
              onCheckedChange={(v) => setAutoApprove(!!v)}
              data-testid="grant-definition-auto-approve"
            />
            <Label htmlFor="def-auto-approve" className="cursor-pointer">
              Auto-approve requests
            </Label>
          </div>
          {autoApprove && (
            <p className="text-xs text-muted-foreground">
              Requests against this definition skip admin review and are
              approved instantly. A justification will be required from
              requesters.
            </p>
          )}
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-2">
              <Label htmlFor="def-max-q">Max Queries</Label>
              <Input
                id="def-max-q"
                type="number"
                min="1"
                placeholder="Unlimited"
                value={maxQueries}
                onChange={(e) => setMaxQueries(e.target.value)}
                data-testid="grant-definition-max-queries"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="def-max-b">Max Data Transfer</Label>
              <div className="flex gap-2">
                <Input
                  id="def-max-b"
                  type="number"
                  min="1"
                  placeholder="Unlimited"
                  value={maxBytesValue}
                  onChange={(e) => setMaxBytesValue(e.target.value)}
                  className="flex-1"
                  data-testid="grant-definition-max-bytes"
                />
                <Select
                  value={bytesUnit}
                  onValueChange={(v) =>
                    setBytesUnit(v as "KB" | "MB" | "GB")
                  }
                >
                  <SelectTrigger
                    className="w-20"
                    data-testid="grant-definition-bytes-unit"
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="KB">KB</SelectItem>
                    <SelectItem value="MB">MB</SelectItem>
                    <SelectItem value="GB">GB</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={create.isPending || update.isPending}
            data-testid="grant-definition-submit"
          >
            {editing ? "Save" : "Create"}
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}
