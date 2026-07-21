import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  useDatabases,
  useDatabaseConnection,
  useCreateDatabase,
  useUpdateDatabase,
  useDeleteDatabase,
  useSSHServers,
  useTestServerConnection,
  type ConnectionTestResult,
  type Database,
  type DatabaseLimited,
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
  AlertCircle,
  PlugZap,
  Loader2,
} from "lucide-react";
import { toast } from "sonner";
import { CopyableField } from "@/components/shared/CopyableField";
import { Alert, AlertDescription } from "@/components/ui/alert";

export const Route = createFileRoute("/_authenticated/servers/")({
  component: ServersPage,
});

type DatabaseItem = Database | DatabaseLimited;

function isFullDatabase(db: DatabaseItem): db is Database {
  return "host" in db;
}

type Protocol =
  | "postgresql"
  | "oracle"
  | "mysql"
  | "mariadb"
  | "mongodb"
  | "ssh";

const PROTOCOL_LABEL: Record<Protocol, string> = {
  postgresql: "PostgreSQL",
  oracle: "Oracle",
  mysql: "MySQL",
  mariadb: "MariaDB",
  mongodb: "MongoDB",
  ssh: "SSH Bastion",
};

const PROTOCOL_DEFAULT_PORT: Record<Protocol, string> = {
  postgresql: "5432",
  oracle: "1521",
  mysql: "3306",
  mariadb: "3306",
  mongodb: "27017",
  ssh: "22",
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
  ssh: "bg-slate-100 text-slate-700 dark:bg-slate-800/50 dark:text-slate-300",
};

const PROTOCOL_USERNAME_PLACEHOLDER: Record<Protocol, string> = {
  postgresql: "postgres",
  oracle: "SYSTEM",
  mysql: "root",
  mariadb: "root",
  mongodb: "admin",
  ssh: "www-data",
};

function ServersPage() {
  const { user } = useAuth();
  const { data: databases, isLoading } = useDatabases();
  const [isCreateOpen, setIsCreateOpen] = useState(false);
  const [deleteDb, setDeleteDb] = useState<DatabaseItem | null>(null);
  const [detailDb, setDetailDb] = useState<DatabaseItem | null>(null);
  const [editSshServer, setEditSshServer] = useState<Database | null>(null);

  const canCreate = canCreateDatabase(user?.roles);
  const canDelete = canDeleteDatabase(user?.roles);
  const canUpdate = canUpdateDatabase(user?.roles);

  // SSH bastions are admin-only (GET /ssh-servers requires the admin role),
  // matching the create/delete-database gating already in place.
  const { data: sshServers, isLoading: sshLoading } = useSSHServers(canCreate);

  const sshColumns: Column<Database>[] = [
    {
      key: "name",
      header: "Name",
      cell: (srv) => <span className="font-medium">{srv.name}</span>,
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
      cell: (db) => <span className="font-medium">{db.name}</span>,
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
          <span className="font-mono text-sm">
            {db.protocol === "oracle"
              ? db.oracle_service_name || db.database_name
              : db.database_name}
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
            <h2 className="text-lg font-semibold">SSH Servers</h2>
            <p className="text-sm text-muted-foreground">
              Bastions used as tunnels to reach databases. Created via the "SSH
              Bastion" protocol above.
            </p>
          </div>
          <DataTable
            columns={sshColumns}
            data={sshServers ?? []}
            isLoading={sshLoading}
            rowKey={(srv) => srv.uid}
            emptyMessage="No SSH servers configured"
          />
        </div>
      )}

      <DeleteDatabaseDialog db={deleteDb} onClose={() => setDeleteDb(null)} />
      <DatabaseDetailsDialog db={detailDb} onClose={() => setDetailDb(null)} />
      <EditSSHServerDialog
        server={editSshServer}
        onClose={() => setEditSshServer(null)}
      />
    </div>
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
};

function describeTestResult(result: ConnectionTestResult): string {
  const stage = STAGE_LABEL[result.stage ?? ""] ?? result.stage ?? "";
  return stage ? `${stage}: ${result.message}` : (result.message ?? "");
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

  const isSSH = protocol === "ssh";
  const { data: sshServers } = useSSHServers();

  const createDb = useCreateDatabase({
    onSuccess: () => {
      toast.success(isSSH ? "SSH server created" : "Database created successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
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
                <SelectItem value="oracle">Oracle</SelectItem>
                <SelectItem value="mysql">MySQL</SelectItem>
                <SelectItem value="mariadb">MariaDB</SelectItem>
                <SelectItem value="mongodb">MongoDB</SelectItem>
                <SelectItem value="ssh" data-testid="protocol-option-ssh">SSH Bastion</SelectItem>
              </SelectContent>
            </Select>
            {isSSH && (
              <p className="text-xs text-muted-foreground">
                An SSH bastion is a dial path, not a database. Other databases
                can be reached "via" it. It never appears in access-request
                lists.
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
              placeholder="production-db"
              required
            />
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
          {!isSSH && (
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
          <div className="grid grid-cols-3 gap-2">
            <div className="col-span-2 space-y-2">
              <Label htmlFor="host">Host</Label>
              <Input
                id="host"
                value={host}
                onChange={(e) => setHost(e.target.value)}
                placeholder="localhost"
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
          {!isSSH && protocol !== "oracle" && (
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
            <Label htmlFor="username">Username</Label>
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder={PROTOCOL_USERNAME_PLACEHOLDER[protocol]}
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="password">
              Password{isSSH ? " (optional if using a key)" : ""}
            </Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required={!isSSH}
            />
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
          {!isSSH && protocol !== "oracle" && (
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
              <Label htmlFor="viaUid">Via SSH server</Label>
              <Select
                value={viaUid || "none"}
                onValueChange={(v) => setViaUid(v === "none" ? "" : v)}
              >
                <SelectTrigger>
                  <SelectValue placeholder="Direct (no tunnel)" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">Direct (no tunnel)</SelectItem>
                  {(sshServers ?? []).map((srv) => (
                    <SelectItem key={srv.uid} value={srv.uid}>
                      {srv.name} ({srv.host})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                Tunnel this database's connection through an SSH bastion. Create
                one by adding a server with protocol "SSH Bastion".
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
  const isSSH = !!db && isFullDatabase(db) && db.protocol === "ssh";

  const deleteDb = useDeleteDatabase({
    onSuccess: () => {
      toast.success(isSSH ? "SSH server deleted successfully" : "Database deleted successfully");
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
          <AlertDialogTitle>{isSSH ? "Delete SSH Server" : "Delete Database"}</AlertDialogTitle>
          <AlertDialogDescription>
            Are you sure you want to delete {isSSH ? "SSH server" : "database"} "{db?.name}"? This action
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
          (from `server`'s current values) every time a different bastion is
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
  const [description, setDescription] = useState(server.description || "");
  const [host, setHost] = useState(server.host || "");
  const [port, setPort] = useState(String(server.port ?? ""));
  const [username, setUsername] = useState(server.username || "");
  const [password, setPassword] = useState("");
  const [sshPrivateKey, setSshPrivateKey] = useState("");
  const [sshPassphrase, setSshPassphrase] = useState("");

  const updateServer = useUpdateDatabase(server.uid, {
    onSuccess: () => {
      toast.success("SSH server updated successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    updateServer.mutate({
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
          <DialogTitle>Edit SSH Server</DialogTitle>
          <DialogDescription>
            Update the bastion "{server.name}"'s connection details.
          </DialogDescription>
        </DialogHeader>
          <div className="space-y-4 py-4 max-h-[60vh] overflow-y-auto">
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
              <Label htmlFor="edit-ssh-username">Username</Label>
              <Input
                id="edit-ssh-username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="edit-ssh-password">
                Password (leave blank to keep unchanged)
              </Label>
              <Input
                id="edit-ssh-password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
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
