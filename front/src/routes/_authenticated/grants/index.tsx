import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  useGrants,
  useUsers,
  useDatabases,
  useCreateGrant,
  useRevokeGrant,
  type AccessGrant,
} from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { Button } from "@/components/ui/button";
import { PermissionButton } from "@/components/shared/PermissionButton";
import { useAuth } from "@/contexts/AuthContext";
import {
  canCreateGrant,
  canRevokeGrant,
  getDisabledReason,
  getActionTooltip,
} from "@/lib/permissions";
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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Checkbox } from "@/components/ui/checkbox";
import { Plus, Ban } from "lucide-react";
import { toast } from "sonner";
import { formatDateTimeLocal, formatDateTime } from "@/lib/date-utils";

// Control options with descriptions
const CONTROLS = [
  { value: "read_only", label: "Read Only", description: "Enable PostgreSQL read-only mode" },
  { value: "block_copy", label: "Block COPY", description: "Prevent COPY commands (data export/import)" },
  { value: "block_ddl", label: "Block DDL", description: "Prevent schema modifications (CREATE, ALTER, DROP)" },
] as const;

// Helper to format control names for display
function formatControlName(control: string): string {
  return control
    .replace(/_/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

// Helper to format bytes in human-readable format
function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + " " + sizes[i];
}

export const Route = createFileRoute("/_authenticated/grants/")({
  component: GrantsPage,
});

function GrantsPage() {
  const { user } = useAuth();
  const [activeOnly, setActiveOnly] = useState(true);
  const { data: grants, isLoading } = useGrants({ active_only: activeOnly });
  const { data: users } = useUsers();
  const { data: databases } = useDatabases();
  const [isCreateOpen, setIsCreateOpen] = useState(false);
  const [revokeGrant, setRevokeGrant] = useState<AccessGrant | null>(null);

  const canCreate = canCreateGrant(user?.roles);
  const canRevoke = canRevokeGrant(user?.roles);

  const getUserName = (uid: string) =>
    users?.find((u) => u.uid === uid)?.username ?? uid;
  const getDbName = (uid: string) =>
    databases?.find((d) => d.uid === uid)?.name ?? uid;

  const getStatus = (grant: AccessGrant) => {
    if (grant.revoked_at) return "revoked";
    const now = new Date();
    if (new Date(grant.starts_at) > now) return "pending";
    if (new Date(grant.expires_at) < now) return "expired";
    return "active";
  };

  const columns: Column<AccessGrant>[] = [
    {
      key: "user",
      header: "User",
      cell: (g) => (
        <span className="font-medium">{getUserName(g.user_id)}</span>
      ),
    },
    {
      key: "database",
      header: "Database",
      cell: (g) => (
        <span className="font-mono text-sm">{getDbName(g.database_id)}</span>
      ),
    },
    {
      key: "controls",
      header: "Controls",
      cell: (g) => {
        const controls = g.controls || [];
        if (controls.length === 0) {
          return <Badge variant="default">Full Access</Badge>;
        }
        return (
          <div className="flex flex-wrap gap-1">
            {controls.map((control) => (
              <Badge key={control} variant="secondary">
                {formatControlName(control)}
              </Badge>
            ))}
          </div>
        );
      },
    },
    {
      key: "time_window",
      header: "Time Window",
      cell: (g) => (
        <div className="text-sm">
          <div>{formatDateTime(g.starts_at)}</div>
          <div className="text-muted-foreground">
            to {formatDateTime(g.expires_at)}
          </div>
        </div>
      ),
    },
    {
      key: "status",
      header: "Status",
      cell: (g) => {
        const status = getStatus(g);
        const variants: Record<string, "default" | "secondary" | "destructive" | "outline"> = {
          active: "default",
          pending: "outline",
          expired: "secondary",
          revoked: "destructive",
        };
        return <Badge variant={variants[status]}>{status}</Badge>;
      },
    },
    {
      key: "usage",
      header: "Usage",
      cell: (g) => (
        <div className="text-sm space-y-1">
          <div>
            {g.query_count ?? 0}
            {g.max_query_counts && ` / ${g.max_query_counts}`} queries
          </div>
          <div className="text-muted-foreground">
            {formatBytes(g.bytes_transferred ?? 0)}
            {g.max_bytes_transferred && ` / ${formatBytes(g.max_bytes_transferred)}`}
          </div>
        </div>
      ),
    },
    {
      key: "actions",
      header: "",
      cell: (g) =>
        getStatus(g) === "active" && (
          <PermissionButton
            variant="ghost"
            size="icon"
            disabled={!canRevoke}
            disabledReason={getDisabledReason("revoke-grant", user?.roles)}
            enabledTooltip={getActionTooltip("revoke-grant")}
            onClick={(e) => {
              e.stopPropagation();
              setRevokeGrant(g);
            }}
          >
            <Ban className="h-4 w-4" />
          </PermissionButton>
        ),
      className: "w-10",
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Grants"
        description="Manage database access grants"
        actions={
          <div className="flex items-center gap-4">
            <div className="flex items-center gap-2">
              <Switch
                id="activeOnly"
                checked={activeOnly}
                onCheckedChange={setActiveOnly}
              />
              <Label htmlFor="activeOnly">Active only</Label>
            </div>
            <Dialog open={isCreateOpen} onOpenChange={setIsCreateOpen}>
              <DialogTrigger asChild>
                <PermissionButton
                  disabled={!canCreate}
                  disabledReason={getDisabledReason("create-grant", user?.roles)}
                  enabledTooltip={getActionTooltip("create-grant")}
                >
                  <Plus className="mr-2 h-4 w-4" />
                  Create Grant
                </PermissionButton>
              </DialogTrigger>
              {isCreateOpen && (
                <CreateGrantDialog
                  users={users ?? []}
                  databases={databases ?? []}
                  onClose={() => setIsCreateOpen(false)}
                />
              )}
            </Dialog>
          </div>
        }
      />

      <DataTable
        columns={columns}
        data={grants ?? []}
        isLoading={isLoading}
        rowKey={(g) => g.uid}
        emptyMessage="No grants found"
      />

      <RevokeGrantDialog
        grant={revokeGrant}
        getUserName={getUserName}
        getDbName={getDbName}
        onClose={() => setRevokeGrant(null)}
      />
    </div>
  );
}

function CreateGrantDialog({
  users,
  databases,
  onClose,
}: {
  users: { uid: string; username: string }[];
  databases: { uid: string; name: string }[];
  onClose: () => void;
}) {
  const [userId, setUserId] = useState("");
  const [databaseId, setDatabaseId] = useState("");
  const [controls, setControls] = useState<string[]>([]);
  const [startsAt, setStartsAt] = useState(() => {
    const now = new Date();
    now.setSeconds(0, 0);
    return formatDateTimeLocal(now);
  });
  const [expiresAt, setExpiresAt] = useState(() => {
    const future = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
    future.setSeconds(0, 0);
    return formatDateTimeLocal(future);
  });
  const [maxQueries, setMaxQueries] = useState<string>("");
  const [maxBytesValue, setMaxBytesValue] = useState<string>("");
  const [bytesUnit, setBytesUnit] = useState<"MB" | "GB">("MB");

  const createGrant = useCreateGrant({
    onSuccess: () => {
      toast.success("Grant created successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    // Convert bytes unit to actual bytes
    const maxBytesTransferred = maxBytesValue
      ? parseInt(maxBytesValue) * (bytesUnit === "GB" ? 1024 * 1024 * 1024 : 1024 * 1024)
      : undefined;

    createGrant.mutate({
      user_id: userId,
      database_id: databaseId,
      controls: controls as ("read_only" | "block_copy" | "block_ddl")[],
      starts_at: new Date(startsAt).toISOString(),
      expires_at: new Date(expiresAt).toISOString(),
      max_query_counts: maxQueries ? parseInt(maxQueries) : undefined,
      max_bytes_transferred: maxBytesTransferred,
    });
  };

  const toggleControl = (controlValue: string) => {
    setControls((prev) =>
      prev.includes(controlValue)
        ? prev.filter((c) => c !== controlValue)
        : [...prev, controlValue]
    );
  };

  return (
    <DialogContent>
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Create Grant</DialogTitle>
          <DialogDescription>
            Grant a user access to a database.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label htmlFor="user">User</Label>
            <Select value={userId} onValueChange={setUserId} required>
              <SelectTrigger>
                <SelectValue placeholder="Select user" />
              </SelectTrigger>
              <SelectContent>
                {users.map((u) => (
                  <SelectItem key={u.uid} value={u.uid}>
                    {u.username}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="database">Database</Label>
            <Select value={databaseId} onValueChange={setDatabaseId} required>
              <SelectTrigger>
                <SelectValue placeholder="Select database" />
              </SelectTrigger>
              <SelectContent>
                {databases.map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-3">
            <Label>Access Controls</Label>
            <p className="text-sm text-muted-foreground">
              Select restrictions to apply. No selections = full write access.
            </p>
            <div className="space-y-2">
              {CONTROLS.map((control) => (
                <div key={control.value} className="flex items-start space-x-3">
                  <Checkbox
                    id={control.value}
                    checked={controls.includes(control.value)}
                    onCheckedChange={() => toggleControl(control.value)}
                  />
                  <div className="grid gap-0.5 leading-none">
                    <Label
                      htmlFor={control.value}
                      className="text-sm font-medium cursor-pointer"
                    >
                      {control.label}
                    </Label>
                    <p className="text-xs text-muted-foreground">
                      {control.description}
                    </p>
                  </div>
                </div>
              ))}
            </div>
          </div>
          <div className="space-y-3">
            <Label>Quotas (Optional)</Label>
            <p className="text-sm text-muted-foreground">
              Set limits on usage. Leave empty for unlimited.
            </p>
            <div className="grid grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label htmlFor="maxQueries">Max Queries</Label>
                <Input
                  id="maxQueries"
                  type="number"
                  min="1"
                  placeholder="Unlimited"
                  value={maxQueries}
                  onChange={(e) => setMaxQueries(e.target.value)}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="maxBytes">Max Data Transfer</Label>
                <div className="flex gap-2">
                  <Input
                    id="maxBytes"
                    type="number"
                    min="1"
                    placeholder="Unlimited"
                    value={maxBytesValue}
                    onChange={(e) => setMaxBytesValue(e.target.value)}
                    className="flex-1"
                  />
                  <Select value={bytesUnit} onValueChange={(v) => setBytesUnit(v as "MB" | "GB")}>
                    <SelectTrigger className="w-20">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="MB">MB</SelectItem>
                      <SelectItem value="GB">GB</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
              </div>
            </div>
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-2">
              <Label htmlFor="startsAt">Start Date & Time</Label>
              <Input
                id="startsAt"
                type="datetime-local"
                value={startsAt}
                onChange={(e) => setStartsAt(e.target.value)}
                required
              />
              <p className="text-xs text-muted-foreground">
                Displayed in your local timezone
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="expiresAt">Expiration Date & Time</Label>
              <Input
                id="expiresAt"
                type="datetime-local"
                value={expiresAt}
                min={startsAt}
                onChange={(e) => setExpiresAt(e.target.value)}
                required
              />
              <p className="text-xs text-muted-foreground">
                Displayed in your local timezone
              </p>
            </div>
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={createGrant.isPending || !userId || !databaseId}
          >
            Create
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

function RevokeGrantDialog({
  grant,
  getUserName,
  getDbName,
  onClose,
}: {
  grant: AccessGrant | null;
  getUserName: (uid: string) => string;
  getDbName: (uid: string) => string;
  onClose: () => void;
}) {
  const revokeGrant = useRevokeGrant({
    onSuccess: () => {
      toast.success("Grant revoked successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  return (
    <AlertDialog open={!!grant} onOpenChange={() => onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Revoke Grant</AlertDialogTitle>
          <AlertDialogDescription>
            Are you sure you want to revoke {getUserName(grant?.user_id ?? "")}'s
            access to {getDbName(grant?.database_id ?? "")}?
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => grant && revokeGrant.mutate(grant.uid)}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            Revoke
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
