import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  useDatabases,
  useDatabaseConnection,
  useCreateDatabase,
  useUpdateDatabase,
  useDeleteDatabase,
  useTunnelServers,
  useTestServerConnection,
  type ConnectionTestResult,
  type Database,
  type DatabaseLimited,
  type OracleServiceNameConflict,
} from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { Button } from "@/components/ui/button";
import { PermissionButton } from "@/components/shared/PermissionButton";
import { useAuth } from "@/contexts/AuthContext";
import {
  canCreateDatabase,
  canDeleteDatabase,
  canUpdateDatabase,
  getDisabledReason,
  getActionTooltip,
} from "@/lib/permissions";
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
import {
  Plus,
  Trash2,
  Pencil,
  ShieldCheck,
  AlertCircle,
  AlertTriangle,
  PlugZap,
  Loader2,
} from "lucide-react";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { toast } from "sonner";
import { CopyableField } from "@/components/shared/CopyableField";
import { ApproverGroupPickers } from "@/components/shared/ApproverGroupPickers";
import { Alert, AlertDescription } from "@/components/ui/alert";

export const Route = createFileRoute("/_authenticated/servers/")({
  component: ServersPage,
});

type DatabaseItem = Database | DatabaseLimited;

function isFullDatabase(db: DatabaseItem): db is Database {
  return "host" in db;
}

// SERVER_NAME_PATTERN mirrors the store-level slug check
// (internal/store.IsValidServerName): creation is gated on it, but rows
// created before that gate existed are grandfathered — this is what lets the
// admin UI flag those rows instead of hiding the drift.
const SERVER_NAME_PATTERN = /^[a-z0-9_]{1,63}$/;

// NonSlugNameWarning flags a server row whose name predates the slug gate
// (or was created directly against the store/API). The row itself works, but
// the name is the client-facing selector on every protocol, so a stray space,
// hyphen or uppercase letter costs reachability somewhere down the line —
// see OracleServiceConflictWarning below, whose shape this mirrors.
function NonSlugNameWarning({ uid, name }: { uid: string; name: string }) {
  return (
    <Tooltip delayDuration={100}>
      <TooltipTrigger asChild>
        <span
          data-testid={`database-name-warning-${uid}`}
          title="Not a valid slug"
          className="inline-flex cursor-help text-amber-600"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span className="sr-only">Name is not a slug</span>
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm space-y-1">
        <p className="font-medium">Not a valid slug</p>
        <p>
          "{name}" predates the naming rule and was grandfathered in. New
          servers must be lowercase letters, numbers, and underscores only —
          this is the name every client types as the database name in its
          connection string, so anything else costs reachability on some
          protocol. Rename it from the pencil action on this row; every client
          config using the old name has to be updated to match.
        </p>
      </TooltipContent>
    </Tooltip>
  );
}

// ServerRenameField is the name input as it appears in the two *edit* forms —
// never in the create dialog, whose copy talks about choosing a name rather
// than moving one. The warning is the point of the component: on a database row
// the name is the connection target, so changing it invalidates every saved
// connection string, and that has to be said at the moment of the edit.
function ServerRenameField({
  id,
  testId,
  value,
  originalName,
  isTunnel,
  onChange,
}: {
  id: string;
  testId: string;
  value: string;
  originalName: string;
  isTunnel: boolean;
  onChange: (next: string) => void;
}) {
  const changed = value !== originalName;

  return (
    <div className="space-y-2">
      <Label htmlFor={id}>Name</Label>
      <Input
        id={id}
        data-testid={testId}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        maxLength={63}
        pattern="^[a-z0-9_]{1,63}$"
        title="Lowercase letters, numbers, and underscores only (no hyphens or spaces)"
        required
      />
      <p className="text-xs text-muted-foreground">
        Lowercase letters, numbers, and underscores only. Names are unique
        across every server, deleted ones included.
      </p>
      {changed && (
        <Alert data-testid="server-rename-warning">
          <AlertTriangle className="h-4 w-4" />
          <AlertDescription className="text-xs">
            {isTunnel ? (
              <>
                Renaming a tunnel row is safe: the servers that dial through it
                reference it by id, not by name. Only what you read in this UI
                changes.
              </>
            ) : (
              <>
                <strong>This moves the connection target.</strong> "
                {originalName}" is what every client sends as the database name
                — the Oracle SERVICE_NAME included — so every saved connection
                string, client config and script still using it stops resolving.
                Sessions already authenticated keep running; new connects must
                use "{value}".
              </>
            )}
          </AlertDescription>
        </Alert>
      )}
    </div>
  );
}

type Protocol =
  | "postgresql"
  | "oracle"
  | "mysql"
  | "mariadb"
  | "mongodb"
  | "mssql"
  | "ssh"
  | "kubernetes";

const PROTOCOL_LABEL: Record<Protocol, string> = {
  postgresql: "PostgreSQL",
  oracle: "Oracle",
  mysql: "MySQL",
  mariadb: "MariaDB",
  mongodb: "MongoDB",
  mssql: "SQL Server",
  ssh: "SSH Bastion",
  kubernetes: "Kubernetes cluster",
};

const PROTOCOL_DEFAULT_PORT: Record<Protocol, string> = {
  postgresql: "5432",
  oracle: "1521",
  mysql: "3306",
  mariadb: "3306",
  mongodb: "27017",
  mssql: "1433",
  ssh: "22",
  kubernetes: "6443",
};

const PROTOCOL_BADGE_CLASS: Record<Protocol, string> = {
  postgresql: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400",
  oracle: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400",
  mysql:
    "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400",
  mariadb:
    "bg-teal-100 text-teal-700 dark:bg-teal-900/30 dark:text-teal-400",
  mongodb:
    "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400",
  mssql:
    "bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-400",
  ssh: "bg-slate-100 text-slate-700 dark:bg-slate-800/50 dark:text-slate-300",
  kubernetes:
    "bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-400",
};

const PROTOCOL_USERNAME_PLACEHOLDER: Record<Protocol, string> = {
  postgresql: "postgres",
  oracle: "SYSTEM",
  mysql: "root",
  mariadb: "root",
  mongodb: "admin",
  mssql: "sa",
  ssh: "www-data",
  kubernetes: "dbbat",
};

function ServersPage() {
  const { user } = useAuth();
  const { data: databases, isLoading } = useDatabases();
  const [isCreateOpen, setIsCreateOpen] = useState(false);
  const [deleteDb, setDeleteDb] = useState<DatabaseItem | null>(null);
  const [detailDb, setDetailDb] = useState<DatabaseItem | null>(null);
  const [editSshServer, setEditSshServer] = useState<Database | null>(null);
  const [approversDb, setApproversDb] = useState<Database | null>(null);
  const [renameDb, setRenameDb] = useState<DatabaseItem | null>(null);

  const canCreate = canCreateDatabase(user?.roles);
  const canDelete = canDeleteDatabase(user?.roles);
  const canUpdate = canUpdateDatabase(user?.roles);

  // Tunnel rows are admin-only (GET /tunnel-servers requires the admin role),
  // matching the create/delete-database gating already in place.
  const { data: tunnelServers, isLoading: tunnelsLoading } =
    useTunnelServers(canCreate);

  const sshColumns: Column<Database>[] = [
    {
      key: "name",
      header: "Name",
      cell: (srv) => (
        <span className="inline-flex items-center gap-1.5">
          <span className="font-medium">{srv.name}</span>
          {!SERVER_NAME_PATTERN.test(srv.name) && (
            <NonSlugNameWarning uid={srv.uid} name={srv.name} />
          )}
        </span>
      ),
    },
    {
      key: "protocol",
      header: "Type",
      cell: (srv) => {
        const proto = srv.protocol as Protocol | undefined;
        return (
          <span
            className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${
              (proto && PROTOCOL_BADGE_CLASS[proto]) ?? ""
            }`}
          >
            {(proto && PROTOCOL_LABEL[proto]) ?? srv.protocol}
          </span>
        );
      },
    },
    {
      key: "description",
      header: "Description",
      cell: (srv) => (
        <span className="text-muted-foreground">{srv.description || "-"}</span>
      ),
    },
    {
      key: "host",
      header: "Host",
      cell: (srv) => (
        <span className="font-mono text-sm">
          {srv.host}:{srv.port}
        </span>
      ),
    },
    {
      key: "username",
      header: "Username",
      cell: (srv) => <span className="font-mono text-sm">{srv.username}</span>,
    },
    {
      key: "namespace",
      header: "Namespace",
      cell: (srv) => (
        <div className="flex items-center gap-2">
          <span className="font-mono text-sm text-muted-foreground">
            {srv.k8s_namespace || "-"}
          </span>
          {/* An escape hatch you cannot see is a trap: a row created with TLS
              verification off has to say so wherever it is listed. */}
          {srv.k8s_insecure_skip_tls_verify && (
            <span
              data-testid={`tunnel-insecure-badge-${srv.uid}`}
              title="TLS verification is disabled for this cluster's API server: anything that can intercept that connection can read the service account token."
              className="inline-flex items-center rounded-full bg-red-100 px-2 py-0.5 text-xs font-medium text-red-700 dark:bg-red-900/30 dark:text-red-400"
            >
              Insecure TLS
            </span>
          )}
          {/* Which CA is in force changes what a failure means, so it has to be
              visible from the list, not only inside the edit dialog. */}
          {!srv.k8s_insecure_skip_tls_verify &&
            !srv.k8s_ca_cert &&
            srv.k8s_learned_ca_cert && (
              <span
                data-testid={`tunnel-ca-pinned-badge-${srv.uid}`}
                title="No CA bundle was supplied, so dbbat pinned the one the API server presented on first connect and refuses any other. Paste the cluster's own bundle for a CA you verified yourself."
                className="inline-flex items-center rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-400"
              >
                CA pinned (TOFU)
              </span>
            )}
        </div>
      ),
    },
    {
      key: "actions",
      header: "",
      cell: (srv) => (
        <div className="flex items-center gap-1">
          <TestConnectionButton
            uid={srv.uid}
            testId={`ssh-server-test-${srv.uid}`}
            canTest={canUpdate}
            disabledReason={getDisabledReason("update-database", user?.roles)}
          />
          <PermissionButton
            data-testid={`ssh-server-edit-${srv.uid}`}
            variant="ghost"
            size="icon"
            disabled={!canUpdate}
            disabledReason={getDisabledReason("update-database", user?.roles)}
            enabledTooltip={getActionTooltip("update-database")}
            onClick={(e) => {
              e.stopPropagation();
              setEditSshServer(srv);
            }}
          >
            <Pencil className="h-4 w-4" />
          </PermissionButton>
          <PermissionButton
            data-testid={`ssh-server-delete-${srv.uid}`}
            variant="ghost"
            size="icon"
            disabled={!canDelete}
            disabledReason={getDisabledReason("delete-database", user?.roles)}
            enabledTooltip={getActionTooltip("delete-database")}
            onClick={(e) => {
              e.stopPropagation();
              setDeleteDb(srv);
            }}
          >
            <Trash2 className="h-4 w-4" />
          </PermissionButton>
        </div>
      ),
      className: "w-20",
    },
  ];

  const columns: Column<DatabaseItem>[] = [
    {
      key: "name",
      header: "Name",
      cell: (db) => (
        <span className="inline-flex items-center gap-1.5">
          <span className="font-medium">{db.name}</span>
          {!SERVER_NAME_PATTERN.test(db.name) && (
            <NonSlugNameWarning uid={db.uid} name={db.name} />
          )}
        </span>
      ),
    },
    {
      key: "protocol",
      header: "Type",
      cell: (db) => {
        if (!isFullDatabase(db)) {
          return <span className="text-muted-foreground">-</span>;
        }
        const proto = db.protocol as Protocol | undefined;
        const klass =
          (proto && PROTOCOL_BADGE_CLASS[proto]) ??
          "bg-gray-100 text-gray-700 dark:bg-gray-900/30 dark:text-gray-400";
        const label = (proto && PROTOCOL_LABEL[proto]) ?? db.protocol;
        return (
          <span
            className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${klass}`}
          >
            {label}
          </span>
        );
      },
    },
    {
      key: "description",
      header: "Description",
      cell: (db) => (
        <span className="text-muted-foreground">{db.description || "-"}</span>
      ),
    },
    {
      key: "host",
      header: "Host",
      cell: (db) =>
        isFullDatabase(db) ? (
          <span className="font-mono text-sm">
            {db.host}:{db.port}
          </span>
        ) : (
          <span className="text-muted-foreground">-</span>
        ),
    },
    {
      key: "database_name",
      header: "Database",
      cell: (db) =>
        isFullDatabase(db) ? (
          <span className="inline-flex items-center gap-1.5">
            <span className="font-mono text-sm">
              {db.protocol === "oracle"
                ? db.oracle_service_name || db.database_name
                : db.database_name}
            </span>
            {/*
              The conflict is a property of this very value — the shared Oracle
              service name — so the warning belongs next to it rather than in a
              column of its own.
            */}
            {db.oracle_service_name_conflict && (
              <OracleServiceConflictWarning
                uid={db.uid}
                conflict={db.oracle_service_name_conflict}
              />
            )}
          </span>
        ) : (
          <span className="text-muted-foreground">-</span>
        ),
    },
    {
      key: "ssl_mode",
      header: "SSL",
      cell: (db) =>
        isFullDatabase(db) && db.protocol !== "oracle" ? (
          <span className="text-sm">{db.ssl_mode || "-"}</span>
        ) : (
          <span className="text-muted-foreground">-</span>
        ),
    },
    {
      key: "actions",
      header: "",
      cell: (db) => (
        <div className="flex items-center gap-1">
          {canUpdate && (
            <TestConnectionButton
              uid={db.uid}
              testId={`database-test-${db.uid}`}
              canTest={canUpdate}
              disabledReason={getDisabledReason("update-database", user?.roles)}
            />
          )}
          {/* Renaming is the one edit a database row has always been missing:
              the name is the connection target, and correcting a bad one used
              to mean an UPDATE against the storage database. */}
          <PermissionButton
            data-testid={`database-rename-${db.uid}`}
            variant="ghost"
            size="icon"
            disabled={!canUpdate}
            disabledReason={getDisabledReason("update-database", user?.roles)}
            enabledTooltip="Rename this server"
            onClick={(e) => {
              e.stopPropagation();
              setRenameDb(db);
            }}
          >
            <Pencil className="h-4 w-4" />
          </PermissionButton>
          {canUpdate && isFullDatabase(db) && (
            <Button
              variant="ghost"
              size="icon"
              title="Approvers"
              data-testid={`database-approvers-${db.uid}`}
              onClick={(e) => {
                e.stopPropagation();
                setApproversDb(db);
              }}
            >
              <ShieldCheck className="h-4 w-4" />
            </Button>
          )}
          <PermissionButton
            variant="ghost"
            size="icon"
            disabled={!canDelete}
            disabledReason={getDisabledReason("delete-database", user?.roles)}
            enabledTooltip={getActionTooltip("delete-database")}
            onClick={(e) => {
              e.stopPropagation();
              setDeleteDb(db);
            }}
          >
            <Trash2 className="h-4 w-4" />
          </PermissionButton>
        </div>
      ),
      className: "w-20",
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Servers"
        description="Manage target database configurations and SSH bastions"
        actions={
          <Dialog open={isCreateOpen} onOpenChange={setIsCreateOpen}>
            <DialogTrigger asChild>
              <PermissionButton
                data-testid="add-database-button"
                disabled={!canCreate}
                disabledReason={getDisabledReason("create-database", user?.roles)}
                enabledTooltip={getActionTooltip("create-database")}
              >
                <Plus className="mr-2 h-4 w-4" />
                Add Server
              </PermissionButton>
            </DialogTrigger>
            <CreateDatabaseDialog onClose={() => setIsCreateOpen(false)} />
          </Dialog>
        }
      />

      <DataTable
        columns={columns}
        data={databases ?? []}
        isLoading={isLoading}
        rowKey={(db) => db.uid}
        emptyMessage="No databases configured"
        onRowClick={(db) => setDetailDb(db)}
      />

      {canCreate && (
        <div className="space-y-3" data-testid="ssh-servers-section">
          <div>
            <h2 className="text-lg font-semibold">Tunnel Servers</h2>
            <p className="text-sm text-muted-foreground">
              Dial paths used to reach databases: SSH bastions and Kubernetes
              clusters. Created via the "SSH Bastion" or "Kubernetes cluster"
              protocol above; neither is ever a grantable target.
            </p>
          </div>
          <DataTable
            columns={sshColumns}
            data={tunnelServers ?? []}
            isLoading={tunnelsLoading}
            rowKey={(srv) => srv.uid}
            emptyMessage="No tunnel servers configured"
          />
        </div>
      )}

      <DeleteDatabaseDialog db={deleteDb} onClose={() => setDeleteDb(null)} />
      <DatabaseDetailsDialog db={detailDb} onClose={() => setDetailDb(null)} />
      <EditSSHServerDialog
        server={editSshServer}
        onClose={() => setEditSshServer(null)}
      />
      <EditServerApproversDialog
        server={approversDb}
        onClose={() => setApproversDb(null)}
      />
      <RenameServerDialog
        server={renameDb}
        onClose={() => setRenameDb(null)}
      />
    </div>
  );
}

/**
 * Editing the two approver lists on one server.
 *
 * Kept out of the create dialog's edit counterpart because there isn't one:
 * databases are otherwise edited nowhere in this UI, and who may approve for a
 * server is exactly the field an operator revisits after the row was created —
 * on a rotation change, a team split, a departure.
 */
function EditServerApproversDialog({
  server,
  onClose,
}: {
  server: Database | null;
  onClose: () => void;
}) {
  return (
    <Dialog open={!!server} onOpenChange={() => onClose()}>
      {/* Keyed on the UID so the pickers re-seed from the row being opened. */}
      {server && (
        <EditServerApproversForm
          key={server.uid}
          server={server}
          onClose={onClose}
        />
      )}
    </Dialog>
  );
}

function EditServerApproversForm({
  server,
  onClose,
}: {
  server: Database;
  onClose: () => void;
}) {
  const [accessApproverUids, setAccessApproverUids] = useState<string[]>(
    server.access_approver_user_group_uids ?? [],
  );
  const [queryApproverUids, setQueryApproverUids] = useState<string[]>(
    server.query_approver_user_group_uids ?? [],
  );

  const updateServer = useUpdateDatabase(server.uid, {
    onSuccess: () => {
      toast.success("Approvers updated");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    // Both sent unconditionally, empty included: clearing a list is a real
    // policy change (the decision falls back to the server groups, then to
    // admins), so it has to be expressible from this form.
    updateServer.mutate({
      access_approver_user_group_uids: accessApproverUids,
      query_approver_user_group_uids: queryApproverUids,
    });
  };

  return (
    <DialogContent
      data-testid="server-approvers-dialog"
      className="max-w-md"
    >
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Approvers for {server.name}</DialogTitle>
          <DialogDescription>
            Who, besides admins, may decide access requests and release approval
            holds on this server.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4 max-h-[60vh] overflow-y-auto">
          <ApproverGroupPickers
            scope="server"
            accessSelected={accessApproverUids}
            onAccessChange={setAccessApproverUids}
            querySelected={queryApproverUids}
            onQueryChange={setQueryApproverUids}
            testIdPrefix="server-approvers"
          />
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={updateServer.isPending}
            data-testid="server-approvers-submit"
          >
            Save
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

// STAGE_LABEL turns a failed check's stage into the thing the admin should go
// look at. The stage — not the message — is what identifies the wrong field.
const STAGE_LABEL: Record<string, string> = {
  config: "Configuration",
  bastion_dial: "Reaching the SSH bastion",
  bastion_auth: "SSH authentication",
  target_dial: "Reaching the database",
  target_auth: "Database authentication",
  cluster_api: "Reaching the Kubernetes API server",
  cluster_auth: "Kubernetes service account token",
  cluster_rbac: "Kubernetes RBAC (pods/portforward)",
  cluster_target: "Resolving the target pod",
};

function describeTestResult(result: ConnectionTestResult): string {
  const stage = STAGE_LABEL[result.stage ?? ""] ?? result.stage ?? "";
  return stage ? `${stage}: ${result.message}` : (result.message ?? "");
}

// reportTestWarnings surfaces the advisory half of a check. It is reported
// whether the check passed or failed: a warning is about how the row sits next
// to its siblings, so a green check can perfectly well carry one.
function reportTestWarnings(result: ConnectionTestResult) {
  for (const warning of result.warnings ?? []) {
    toast.warning(warning.message);
  }
}

// OracleServiceConflictWarning marks an Oracle row whose upstream service name
// is also claimed by rows pointing at a different host:port.
//
// The row itself is fine. What is broken is connecting with the *shared service
// name* rather than the dbbat server name: the proxy compares candidate
// upstreams as text, so a CNAME here and the A-record there read as two
// machines and the connect is refused ORA-12514. Showing it here is what makes
// that visible before a user hits it — the whole reason the textual compare can
// stay as it is.
function OracleServiceConflictWarning({
  uid,
  conflict,
}: {
  uid: string;
  conflict: OracleServiceNameConflict;
}) {
  return (
    <Tooltip delayDuration={100}>
      <TooltipTrigger asChild>
        <span
          data-testid={`database-oracle-conflict-${uid}`}
          title={conflict.message}
          className="inline-flex cursor-help text-amber-600"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span className="sr-only">Conflicting Oracle service name</span>
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm space-y-1">
        <p className="font-medium">Shared Oracle service name</p>
        <p>{conflict.message}</p>
        <ul className="font-mono text-xs">
          {conflict.servers?.map((srv) => (
            <li key={srv.uid}>
              {srv.name} → {srv.host}:{srv.port}
            </li>
          ))}
        </ul>
      </TooltipContent>
    </Tooltip>
  );
}

// TestConnectionButton dials the server for real and reports the staged
// outcome. Rendered per row, so each button owns its own mutation state.
function TestConnectionButton({
  uid,
  testId,
  canTest,
  disabledReason,
}: {
  uid: string;
  testId: string;
  canTest: boolean;
  disabledReason?: string;
}) {
  const testConnection = useTestServerConnection(uid);

  return (
    <PermissionButton
      data-testid={testId}
      variant="ghost"
      size="icon"
      disabled={!canTest || testConnection.isPending}
      disabledReason={disabledReason}
      enabledTooltip="Test this server's connectivity"
      onClick={(e) => {
        e.stopPropagation();
        testConnection.mutate(undefined, {
          onSuccess: (result) => {
            reportTestWarnings(result);
            if (result.ok) {
              toast.success(
                result.host_key_pinned
                  ? `${result.message} (host key pinned)`
                  : result.message
              );
              return;
            }
            toast.error(describeTestResult(result));
          },
          onError: (error: Error) => toast.error(error.message),
        });
      }}
    >
      {testConnection.isPending ? (
        <Loader2 className="h-4 w-4 animate-spin" />
      ) : (
        <PlugZap className="h-4 w-4" />
      )}
    </PermissionButton>
  );
}

function CreateDatabaseDialog({ onClose }: { onClose: () => void }) {
  const [protocol, setProtocol] = useState<Protocol>("postgresql");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [host, setHost] = useState("");
  const [port, setPort] = useState(PROTOCOL_DEFAULT_PORT.postgresql);
  const [databaseName, setDatabaseName] = useState("");
  const [oracleServiceName, setOracleServiceName] = useState("");
  const [mongoAuthSource, setMongoAuthSource] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [sslMode, setSslMode] = useState("prefer");
  const [listable, setListable] = useState(true);
  const [viaUid, setViaUid] = useState<string>("");
  const [sshPrivateKey, setSshPrivateKey] = useState("");
  const [sshPassphrase, setSshPassphrase] = useState("");
  const [k8sCaCert, setK8sCaCert] = useState("");
  const [k8sNamespace, setK8sNamespace] = useState("");
  const [k8sInsecure, setK8sInsecure] = useState(false);
  const [accessApproverUids, setAccessApproverUids] = useState<string[]>([]);
  const [queryApproverUids, setQueryApproverUids] = useState<string[]>([]);

  const isSSH = protocol === "ssh";
  const isKubernetes = protocol === "kubernetes";
  // Both are dial paths rather than database targets, so they share every
  // "this row has no database behind it" branch below.
  const isTunnel = isSSH || isKubernetes;
  const { data: tunnelServers } = useTunnelServers();
  // A cluster row may itself sit behind an SSH bastion; the reverse nesting is
  // unsupported, so a cluster is never offered as a cluster's own via.
  const viaOptions = (tunnelServers ?? []).filter((srv) =>
    isKubernetes ? srv.protocol === "ssh" : true,
  );

  const createDb = useCreateDatabase({
    onSuccess: () => {
      toast.success(
        isKubernetes
          ? "Kubernetes cluster created"
          : isSSH
            ? "SSH server created"
            : "Database created successfully",
      );
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (isKubernetes) {
      createDb.mutate({
        name,
        description: description || undefined,
        host,
        port: parseInt(port, 10),
        username,
        // The service account bearer token rides in the password field: it is
        // this row's one secret, encrypted exactly like a database password.
        password,
        protocol,
        ssl_mode: "",
        listable: false,
        k8s_ca_cert: k8sCaCert || undefined,
        k8s_namespace: k8sNamespace,
        k8s_insecure_skip_tls_verify: k8sInsecure || undefined,
        via_uid: viaUid || undefined,
      });
      return;
    }
    if (isSSH) {
      createDb.mutate({
        name,
        description: description || undefined,
        host,
        port: parseInt(port, 10),
        username,
        password: password || undefined,
        protocol,
        ssl_mode: "",
        listable: false,
        ssh_private_key: sshPrivateKey || undefined,
        ssh_passphrase: sshPassphrase || undefined,
      });
      return;
    }
    createDb.mutate({
      name,
      description: description || undefined,
      host,
      port: parseInt(port, 10),
      database_name:
        protocol === "oracle" ? oracleServiceName : databaseName,
      username,
      password,
      ssl_mode: protocol === "oracle" ? "" : sslMode,
      protocol,
      oracle_service_name:
        protocol === "oracle" ? oracleServiceName : undefined,
      mongo_auth_source:
        protocol === "mongodb" && mongoAuthSource
          ? mongoAuthSource
          : undefined,
      listable,
      via_uid: viaUid || undefined,
      access_approver_user_group_uids: accessApproverUids,
      query_approver_user_group_uids: queryApproverUids,
    });
  };

  return (
    <DialogContent className="max-w-md">
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Add Server</DialogTitle>
          <DialogDescription>
            Configure a new target database connection.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4 max-h-[60vh] overflow-y-auto">
          <div className="space-y-2">
            <Label htmlFor="protocol">Protocol</Label>
            <Select
              value={protocol}
              onValueChange={(val) => {
                const next = val as Protocol;
                setProtocol(next);
                // Auto-cycle the port when the user hasn't customised it away
                // from one of the conventional defaults.
                if (Object.values(PROTOCOL_DEFAULT_PORT).includes(port)) {
                  setPort(PROTOCOL_DEFAULT_PORT[next]);
                }
              }}
            >
              <SelectTrigger data-testid="protocol-select">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="postgresql">PostgreSQL</SelectItem>
                <SelectItem value="oracle" data-testid="protocol-option-oracle">Oracle</SelectItem>
                <SelectItem value="mysql">MySQL</SelectItem>
                <SelectItem value="mariadb">MariaDB</SelectItem>
                <SelectItem value="mongodb">MongoDB</SelectItem>
                <SelectItem value="mssql">SQL Server</SelectItem>
                <SelectItem value="ssh" data-testid="protocol-option-ssh">SSH Bastion</SelectItem>
                <SelectItem value="kubernetes" data-testid="protocol-option-kubernetes">Kubernetes cluster</SelectItem>
              </SelectContent>
            </Select>
            {isSSH && (
              <p className="text-xs text-muted-foreground">
                An SSH bastion is a dial path, not a database. Other databases
                can be reached "via" it. It never appears in access-request
                lists.
              </p>
            )}
            {isKubernetes && (
              <p className="text-xs text-muted-foreground">
                A Kubernetes cluster is a dial path, not a database. Databases
                reached "via" it must <strong>run as pods</strong> in the
                namespace below: a port-forward reaches a pod's own port, so a
                database merely routable from the cluster network (an RDS in the
                same VPC) is out of scope.
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="name">Name</Label>
            <Input
              id="name"
              data-testid="database-name-input"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="production_db"
              maxLength={63}
              pattern="^[a-z0-9_]{1,63}$"
              title="Lowercase letters, numbers, and underscores only (no hyphens or spaces)"
              required
            />
            <p className="text-xs text-muted-foreground">
              This is the client-facing selector every protocol uses — the
              "database name" typed in a connection string — so it must be a
              slug: lowercase letters, numbers, and underscores only.
            </p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="description">Description</Label>
            <Input
              id="description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Production database"
            />
          </div>
          {!isTunnel && (
            <div className="flex items-center justify-between rounded-lg border p-3">
              <div className="space-y-0.5">
                <Label htmlFor="listable">Listable</Label>
                <p className="text-sm text-muted-foreground">
                  Show in the access-request dropdown for non-admin users
                </p>
              </div>
              <Switch
                id="listable"
                checked={listable}
                onCheckedChange={setListable}
              />
            </div>
          )}
          {/* Tunnel rows are dial paths — nothing is ever granted or held on
              them, so neither approver kind applies. */}
          {!isTunnel && (
            <ApproverGroupPickers
              scope="server"
              accessSelected={accessApproverUids}
              onAccessChange={setAccessApproverUids}
              querySelected={queryApproverUids}
              onQueryChange={setQueryApproverUids}
              testIdPrefix="database"
            />
          )}
          <div className="grid grid-cols-3 gap-2">
            <div className="col-span-2 space-y-2">
              <Label htmlFor="host">
                {isKubernetes ? "API server" : "Host"}
              </Label>
              <Input
                id="host"
                value={host}
                onChange={(e) => setHost(e.target.value)}
                placeholder={
                  isKubernetes ? "https://api.cluster.example.com" : "localhost"
                }
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="port">Port</Label>
              <Input
                id="port"
                type="number"
                value={port}
                onChange={(e) => setPort(e.target.value)}
                required
              />
            </div>
          </div>
          {!isTunnel && protocol !== "oracle" && (
            <div className="space-y-2">
              <Label htmlFor="databaseName">Database Name</Label>
              <Input
                id="databaseName"
                value={databaseName}
                onChange={(e) => setDatabaseName(e.target.value)}
                placeholder={protocol === "mysql" || protocol === "mariadb" || protocol === "mongodb" ? "mydb" : "myapp"}
                required
              />
            </div>
          )}
          {protocol === "oracle" && (
            <div className="space-y-2">
              <Label htmlFor="oracleServiceName">Service Name</Label>
              <Input
                id="oracleServiceName"
                value={oracleServiceName}
                onChange={(e) => setOracleServiceName(e.target.value)}
                placeholder="ORCL"
                required
              />
            </div>
          )}
          {protocol === "mongodb" && (
            <div className="space-y-2">
              <Label htmlFor="mongoAuthSource">Auth Source</Label>
              <Input
                id="mongoAuthSource"
                value={mongoAuthSource}
                onChange={(e) => setMongoAuthSource(e.target.value)}
                placeholder="admin"
              />
              <p className="text-xs text-muted-foreground">
                Upstream MongoDB database where the proxy user's credentials
                live. Defaults to <code className="font-mono">admin</code>.
              </p>
            </div>
          )}
          <div className="space-y-2">
            <Label htmlFor="username">
              {isKubernetes ? "Service account name" : "Username"}
            </Label>
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder={PROTOCOL_USERNAME_PLACEHOLDER[protocol]}
              required
            />
            {isKubernetes && (
              <p className="text-xs text-muted-foreground">
                Informational: it names the account in the UI and the logs. The
                credential is the token below.
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="password">
              {isKubernetes
                ? "Service account token"
                : `Password${isSSH ? " (optional if using a key)" : ""}`}
            </Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required={!isSSH}
            />
            {isKubernetes && (
              <p className="text-xs text-muted-foreground">
                A long-lived ServiceAccount bearer token. Kubeconfig files are
                not accepted: managed clusters authenticate through exec
                credential plugins, which a server daemon cannot run.
              </p>
            )}
          </div>
          {isSSH && (
            <>
              <div className="space-y-2">
                <Label htmlFor="sshPrivateKey">SSH Private Key (PEM)</Label>
                <textarea
                  id="sshPrivateKey"
                  className="flex min-h-[96px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                  value={sshPrivateKey}
                  onChange={(e) => setSshPrivateKey(e.target.value)}
                  placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                />
                <p className="text-xs text-muted-foreground">
                  Write-only: the stored key is never shown again.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="sshPassphrase">Key Passphrase (optional)</Label>
                <Input
                  id="sshPassphrase"
                  type="password"
                  value={sshPassphrase}
                  onChange={(e) => setSshPassphrase(e.target.value)}
                />
              </div>
            </>
          )}
          {isKubernetes && (
            <>
              <div className="space-y-2">
                <Label htmlFor="k8sNamespace">Namespace</Label>
                <Input
                  id="k8sNamespace"
                  data-testid="k8s-namespace-input"
                  value={k8sNamespace}
                  onChange={(e) => setK8sNamespace(e.target.value)}
                  placeholder="data"
                  required
                />
                <p className="text-xs text-muted-foreground">
                  Every pod lookup and every port-forward is scoped to it: it is
                  the namespace the Role and RoleBinding cover.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="k8sCaCert">
                  CA certificate (PEM){" "}
                  <span className="font-normal text-muted-foreground">
                    — optional
                  </span>
                </Label>
                <textarea
                  id="k8sCaCert"
                  data-testid="k8s-ca-cert-input"
                  className="flex min-h-[96px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                  value={k8sCaCert}
                  onChange={(e) => setK8sCaCert(e.target.value)}
                  placeholder="-----BEGIN CERTIFICATE-----"
                />
                {/* Optional, but the recommendation: a pasted bundle is a CA
                    you verified, where a pin is only a CA we happened to meet
                    first. Say so at the point of choosing. */}
                <p className="text-xs text-muted-foreground">
                  From the token Secret&apos;s{" "}
                  <code className="font-mono">ca.crt</code> key. Leave it blank
                  and dbbat pins whatever the API server presents on the first
                  connect, then refuses anything else — safer than skipping
                  verification, weaker than a bundle you checked yourself.
                </p>
              </div>
              <div className="flex items-center justify-between rounded-lg border p-3">
                <div className="space-y-0.5">
                  <Label htmlFor="k8sInsecure">Skip TLS verification</Label>
                  <p className="text-sm text-muted-foreground">
                    Throwaway clusters only: anything that can intercept the API
                    server connection can then read the token.
                  </p>
                </div>
                <Switch
                  id="k8sInsecure"
                  checked={k8sInsecure}
                  onCheckedChange={setK8sInsecure}
                />
              </div>
            </>
          )}
          {!isTunnel && protocol !== "oracle" && (
            <div className="space-y-2">
              <Label htmlFor="sslMode">SSL Mode</Label>
              <Select value={sslMode} onValueChange={setSslMode}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="disable">Disable</SelectItem>
                  <SelectItem value="prefer">Prefer</SelectItem>
                  <SelectItem value="require">Require</SelectItem>
                  <SelectItem value="verify-ca">Verify CA</SelectItem>
                  <SelectItem value="verify-full">Verify Full</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}
          {!isSSH && (
            <div className="space-y-2">
              <Label htmlFor="viaUid">
                {isKubernetes ? "Via SSH bastion" : "Via tunnel server"}
              </Label>
              <Select
                value={viaUid || "none"}
                onValueChange={(v) => setViaUid(v === "none" ? "" : v)}
              >
                <SelectTrigger data-testid="via-select">
                  <SelectValue placeholder="Direct (no tunnel)" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">Direct (no tunnel)</SelectItem>
                  {viaOptions.map((srv) => (
                    <SelectItem key={srv.uid} value={srv.uid}>
                      {srv.name} (
                      {PROTOCOL_LABEL[srv.protocol as Protocol] ?? srv.protocol}
                      )
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                {isKubernetes
                  ? "Only needed when the API server itself is reachable only through a jump host."
                  : 'Dial this database through an SSH bastion or a Kubernetes cluster. Through a cluster, Host is a pod name or "svc/<name>" in that cluster\'s namespace and Port is the container port.'}
              </p>
            </div>
          )}
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" data-testid="database-create-submit" disabled={createDb.isPending}>
            Create
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

function DatabaseDetailsDialog({
  db,
  onClose,
}: {
  db: DatabaseItem | null;
  onClose: () => void;
}) {
  const { data: connInfo, error: connError } = useDatabaseConnection(
    db?.uid
  );

  const isProxyDisabled =
    connError &&
    "status" in connError &&
    (connError as { status?: number }).status === 409;
  const noGrant =
    connError &&
    "status" in connError &&
    (connError as { status?: number }).status === 404;

  return (
    <Dialog open={!!db} onOpenChange={() => onClose()}>
      <DialogContent data-testid="database-details-dialog" className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{db?.name}</DialogTitle>
          {db?.description && (
            <DialogDescription>{db.description}</DialogDescription>
          )}
        </DialogHeader>
        <div className="space-y-4 py-4">
          {db && isFullDatabase(db) && (
            <div className="space-y-2 text-sm">
              <div className="grid grid-cols-3 gap-1">
                <span className="text-muted-foreground">Protocol</span>
                <span className="col-span-2 font-medium">
                  {PROTOCOL_LABEL[(db as Database).protocol as Protocol] ??
                    (db as Database).protocol}
                </span>
              </div>
              <div className="grid grid-cols-3 gap-1">
                <span className="text-muted-foreground">Target</span>
                <span className="col-span-2 font-mono">
                  {(db as Database).host}:{(db as Database).port} /{" "}
                  {(db as Database).database_name}
                </span>
              </div>
              {(db as Database).protocol !== "oracle" && (
                <div className="grid grid-cols-3 gap-1">
                  <span className="text-muted-foreground">SSL mode</span>
                  <span className="col-span-2">
                    {(db as Database).ssl_mode ?? "-"}
                  </span>
                </div>
              )}
            </div>
          )}

          {!noGrant && (
            <div className="space-y-2">
              <h3 className="text-sm font-medium">Connection URL</h3>
              <p className="text-xs text-muted-foreground">
                Replace{" "}
                <code className="rounded bg-muted px-1 py-0.5 font-mono">
                  {"{DBBAT_KEY}"}
                </code>{" "}
                with one of your dbbat API keys (the{" "}
                <code className="font-mono">dbb_…</code> token).
              </p>
              {isProxyDisabled ? (
                <Alert>
                  <AlertCircle className="h-4 w-4" />
                  <AlertDescription>
                    The proxy for this protocol is currently disabled.
                  </AlertDescription>
                </Alert>
              ) : connInfo ? (
                <CopyableField
                  value={connInfo.url}
                  testId="database-connection-url"
                  toastMessage="Connection URL copied"
                />
              ) : null}
            </div>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function DeleteDatabaseDialog({
  db,
  onClose,
}: {
  db: DatabaseItem | null;
  onClose: () => void;
}) {
  const protocol = !!db && isFullDatabase(db) ? db.protocol : undefined;
  const isSSH = protocol === "ssh" || protocol === "kubernetes";
  const kind =
    protocol === "kubernetes"
      ? "Kubernetes cluster"
      : protocol === "ssh"
        ? "SSH server"
        : "database";

  const deleteDb = useDeleteDatabase({
    onSuccess: () => {
      toast.success(isSSH ? `${kind} deleted successfully` : "Database deleted successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  return (
    <AlertDialog open={!!db} onOpenChange={() => onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{isSSH ? `Delete ${kind}` : "Delete Database"}</AlertDialogTitle>
          <AlertDialogDescription>
            Are you sure you want to delete {kind} "{db?.name}"? This action
            cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => db && deleteDb.mutate(db.uid)}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

/**
 * Renaming one database row.
 *
 * A dialog of its own rather than a field in a general edit form, because
 * database rows have no general edit form in this UI — and because a rename is
 * not an edit like the others: it changes what clients type, so it deserves the
 * warning to itself. Tunnel rows get the same field inside their existing edit
 * form (EditSSHServerForm) instead.
 */
function RenameServerDialog({
  server,
  onClose,
}: {
  server: DatabaseItem | null;
  onClose: () => void;
}) {
  return (
    <Dialog open={!!server} onOpenChange={() => onClose()}>
      {/* Keyed on the UID so the input re-seeds from the row that was opened,
          the same trick EditSSHServerDialog uses. */}
      {server && (
        <RenameServerForm key={server.uid} server={server} onClose={onClose} />
      )}
    </Dialog>
  );
}

function RenameServerForm({
  server,
  onClose,
}: {
  server: DatabaseItem;
  onClose: () => void;
}) {
  const [name, setName] = useState(server.name);

  const renameServer = useUpdateDatabase(server.uid, {
    onSuccess: () => {
      toast.success(`Renamed to "${name}"`);
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    renameServer.mutate({ name });
  };

  return (
    <DialogContent data-testid="database-rename-dialog" className="max-w-md">
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Rename server</DialogTitle>
          <DialogDescription>
            "{server.name}" keeps its grants, history and query chains — only
            the name clients connect with changes.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <ServerRenameField
            id="rename-server-name"
            testId="database-rename-input"
            value={name}
            originalName={server.name}
            isTunnel={false}
            onChange={setName}
          />
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            data-testid="database-rename-submit"
            disabled={renameServer.isPending || name === server.name}
          >
            Rename
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

function EditSSHServerDialog({
  server,
  onClose,
}: {
  server: Database | null;
  onClose: () => void;
}) {
  return (
    <Dialog open={!!server} onOpenChange={() => onClose()}>
      {/* Keyed on the server UID so the form state re-initializes fresh
          (from `server`'s current values) every time a different tunnel row is
          opened for editing, without needing an effect to re-seed state. */}
      {server && (
        <EditSSHServerForm key={server.uid} server={server} onClose={onClose} />
      )}
    </Dialog>
  );
}

function EditSSHServerForm({
  server,
  onClose,
}: {
  server: Database;
  onClose: () => void;
}) {
  const [name, setName] = useState(server.name);
  const [description, setDescription] = useState(server.description || "");
  const [host, setHost] = useState(server.host || "");
  const [port, setPort] = useState(String(server.port ?? ""));
  const [username, setUsername] = useState(server.username || "");
  const [password, setPassword] = useState("");
  const [sshPrivateKey, setSshPrivateKey] = useState("");
  const [sshPassphrase, setSshPassphrase] = useState("");
  const [k8sCaCert, setK8sCaCert] = useState(server.k8s_ca_cert || "");
  const [k8sNamespace, setK8sNamespace] = useState(server.k8s_namespace || "");
  const [k8sInsecure, setK8sInsecure] = useState(
    server.k8s_insecure_skip_tls_verify ?? false,
  );
  // Read-only: the learned pin is the dialer's to write, never a form field.
  const learnedCaCert = server.k8s_learned_ca_cert || "";
  const [resetLearnedCa, setResetLearnedCa] = useState(false);

  // One form for both dial-path kinds: they share host/port/credential and
  // differ only in the extra material they carry.
  const isKubernetes = server.protocol === "kubernetes";
  const kindLabel = isKubernetes ? "Kubernetes cluster" : "SSH server";

  const updateServer = useUpdateDatabase(server.uid, {
    onSuccess: () => {
      toast.success(`${kindLabel} updated successfully`);
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    // Only sent when actually changed: a PUT that repeats the current name
    // would still be a rename as far as the uniqueness check is concerned, and
    // it keeps the audit entry free of a "renamed to itself" line.
    const renamed = name !== server.name ? name : undefined;
    if (isKubernetes) {
      updateServer.mutate({
        name: renamed,
        description: description || undefined,
        host,
        port: parseInt(port, 10),
        username,
        // Blank means "keep the stored token", exactly like the SSH secrets.
        password: password || undefined,
        // Sent unconditionally, unlike the CA: the whole point is that the
        // flag can be turned back off from here.
        k8s_ca_cert: k8sCaCert,
        k8s_namespace: k8sNamespace || undefined,
        k8s_insecure_skip_tls_verify: k8sInsecure,
        // Only ever sent when explicitly asked for: forgetting a pin is a
        // decision, not a side effect of opening the dialog.
        k8s_reset_learned_ca_cert: resetLearnedCa || undefined,
      });
      return;
    }
    updateServer.mutate({
      name: renamed,
      description: description || undefined,
      host,
      port: parseInt(port, 10),
      username,
      password: password || undefined,
      ssh_private_key: sshPrivateKey || undefined,
      ssh_passphrase: sshPassphrase || undefined,
    });
  };

  return (
    <DialogContent data-testid="ssh-server-edit-dialog" className="max-w-md">
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Edit {kindLabel}</DialogTitle>
          <DialogDescription>
            Update "{server.name}"'s connection details.
          </DialogDescription>
        </DialogHeader>
          <div className="space-y-4 py-4 max-h-[60vh] overflow-y-auto">
            <ServerRenameField
              id="edit-ssh-name"
              testId="ssh-server-edit-name-input"
              value={name}
              originalName={server.name}
              isTunnel
              onChange={setName}
            />
            <div className="space-y-2">
              <Label htmlFor="edit-ssh-description">Description</Label>
              <Input
                id="edit-ssh-description"
                data-testid="ssh-server-edit-description-input"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
            </div>
            <div className="grid grid-cols-3 gap-2">
              <div className="col-span-2 space-y-2">
                <Label htmlFor="edit-ssh-host">Host</Label>
                <Input
                  id="edit-ssh-host"
                  value={host}
                  onChange={(e) => setHost(e.target.value)}
                  required
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="edit-ssh-port">Port</Label>
                <Input
                  id="edit-ssh-port"
                  type="number"
                  value={port}
                  onChange={(e) => setPort(e.target.value)}
                  required
                />
              </div>
            </div>
            <div className="space-y-2">
              <Label htmlFor="edit-ssh-username">
                {isKubernetes ? "Service account name" : "Username"}
              </Label>
              <Input
                id="edit-ssh-username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="edit-ssh-password">
                {isKubernetes ? "Service account token" : "Password"} (leave
                blank to keep unchanged)
              </Label>
              <Input
                id="edit-ssh-password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            {isKubernetes && (
              <>
                <div className="space-y-2">
                  <Label htmlFor="edit-k8s-namespace">Namespace</Label>
                  <Input
                    id="edit-k8s-namespace"
                    data-testid="k8s-server-edit-namespace-input"
                    value={k8sNamespace}
                    onChange={(e) => setK8sNamespace(e.target.value)}
                    required
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="edit-k8s-ca-cert">
                    CA certificate (PEM){" "}
                    <span className="font-normal text-muted-foreground">
                      — optional
                    </span>
                  </Label>
                  <textarea
                    id="edit-k8s-ca-cert"
                    data-testid="k8s-server-edit-ca-cert-input"
                    className="flex min-h-[96px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                    value={k8sCaCert}
                    onChange={(e) => setK8sCaCert(e.target.value)}
                  />
                  <p className="text-xs text-muted-foreground">
                    Public challenge material, so unlike the token it is shown
                    back to you. Blank means the CA pinned on first connect is
                    what verifies the API server.
                  </p>
                </div>
                {/* Which CA is actually in force is the question an operator
                    asks when a connection starts failing, so answer it here
                    rather than leaving them to infer it from two fields. */}
                {learnedCaCert && !k8sCaCert && (
                  <div
                    data-testid="k8s-server-edit-learned-ca"
                    className="space-y-2 rounded-lg border p-3"
                  >
                    <p className="text-sm font-medium">
                      Pinned on first connect
                    </p>
                    <p className="text-xs text-muted-foreground">
                      dbbat learned this CA itself and refuses anything else. If
                      the cluster&apos;s CA has rotated, paste the new bundle
                      above — or forget the pin and let the next connect pin
                      afresh, which only makes sense once you know why it
                      changed.
                    </p>
                    <pre className="max-h-24 overflow-auto rounded bg-muted p-2 font-mono text-[10px] leading-tight">
                      {learnedCaCert}
                    </pre>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      data-testid="k8s-server-edit-reset-learned-ca"
                      onClick={() => setResetLearnedCa(true)}
                      disabled={resetLearnedCa}
                    >
                      {resetLearnedCa
                        ? "Will be forgotten on save"
                        : "Forget the learned CA"}
                    </Button>
                  </div>
                )}
                <div className="flex items-center justify-between rounded-lg border p-3">
                  <div className="space-y-0.5">
                    <Label htmlFor="edit-k8s-insecure">
                      Skip TLS verification
                    </Label>
                    <p className="text-sm text-muted-foreground">
                      Anything that can intercept the API server connection can
                      read the service account token. Throwaway clusters only.
                    </p>
                  </div>
                  <Switch
                    id="edit-k8s-insecure"
                    data-testid="k8s-server-edit-insecure-switch"
                    checked={k8sInsecure}
                    onCheckedChange={setK8sInsecure}
                  />
                </div>
              </>
            )}
            {!isKubernetes && (
              <>
            <div className="space-y-2">
              <Label htmlFor="edit-ssh-private-key">
                SSH Private Key (PEM, leave blank to keep unchanged)
              </Label>
              <textarea
                id="edit-ssh-private-key"
                className="flex min-h-[96px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                value={sshPrivateKey}
                onChange={(e) => setSshPrivateKey(e.target.value)}
                placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              />
              <p className="text-xs text-muted-foreground">
                Write-only: the stored key is never shown again.
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="edit-ssh-passphrase">Key Passphrase</Label>
              <Input
                id="edit-ssh-passphrase"
                type="password"
                value={sshPassphrase}
                onChange={(e) => setSshPassphrase(e.target.value)}
              />
            </div>
              </>
            )}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button
              type="submit"
              data-testid="ssh-server-edit-submit"
              disabled={updateServer.isPending}
            >
              Save
            </Button>
          </DialogFooter>
      </form>
    </DialogContent>
  );
}
