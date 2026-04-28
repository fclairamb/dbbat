import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  useDatabases,
  useCreateDatabase,
  useDeleteDatabase,
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
import { Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";

export const Route = createFileRoute("/_authenticated/databases/")({
  component: DatabasesPage,
});

type DatabaseItem = Database | DatabaseLimited;

function isFullDatabase(db: DatabaseItem): db is Database {
  return "host" in db;
}

type Protocol = "postgresql" | "oracle" | "mysql" | "mariadb";

const PROTOCOL_LABEL: Record<Protocol, string> = {
  postgresql: "PostgreSQL",
  oracle: "Oracle",
  mysql: "MySQL",
  mariadb: "MariaDB",
};

const PROTOCOL_DEFAULT_PORT: Record<Protocol, string> = {
  postgresql: "5432",
  oracle: "1521",
  mysql: "3306",
  mariadb: "3306",
};

const PROTOCOL_BADGE_CLASS: Record<Protocol, string> = {
  postgresql: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400",
  oracle: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400",
  mysql:
    "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400",
  mariadb:
    "bg-teal-100 text-teal-700 dark:bg-teal-900/30 dark:text-teal-400",
};

const PROTOCOL_USERNAME_PLACEHOLDER: Record<Protocol, string> = {
  postgresql: "postgres",
  oracle: "SYSTEM",
  mysql: "root",
  mariadb: "root",
};

function DatabasesPage() {
  const { user } = useAuth();
  const { data: databases, isLoading } = useDatabases();
  const [isCreateOpen, setIsCreateOpen] = useState(false);
  const [deleteDb, setDeleteDb] = useState<DatabaseItem | null>(null);

  const canCreate = canCreateDatabase(user?.roles);
  const canDelete = canDeleteDatabase(user?.roles);

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
      ),
      className: "w-10",
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Databases"
        description="Manage target database configurations"
        actions={
          <Dialog open={isCreateOpen} onOpenChange={setIsCreateOpen}>
            <DialogTrigger asChild>
              <PermissionButton
                disabled={!canCreate}
                disabledReason={getDisabledReason("create-database", user?.roles)}
                enabledTooltip={getActionTooltip("create-database")}
              >
                <Plus className="mr-2 h-4 w-4" />
                Add Database
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
      />

      <DeleteDatabaseDialog db={deleteDb} onClose={() => setDeleteDb(null)} />
    </div>
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
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [sslMode, setSslMode] = useState("prefer");

  const createDb = useCreateDatabase({
    onSuccess: () => {
      toast.success("Database created successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
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
    });
  };

  return (
    <DialogContent className="max-w-md">
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Add Database</DialogTitle>
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
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="postgresql">PostgreSQL</SelectItem>
                <SelectItem value="oracle">Oracle</SelectItem>
                <SelectItem value="mysql">MySQL</SelectItem>
                <SelectItem value="mariadb">MariaDB</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="name">Name</Label>
            <Input
              id="name"
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
          {protocol !== "oracle" && (
            <div className="space-y-2">
              <Label htmlFor="databaseName">Database Name</Label>
              <Input
                id="databaseName"
                value={databaseName}
                onChange={(e) => setDatabaseName(e.target.value)}
                placeholder={protocol === "mysql" || protocol === "mariadb" ? "mydb" : "myapp"}
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
            <Label htmlFor="password">Password</Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </div>
          {protocol !== "oracle" && (
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
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={createDb.isPending}>
            Create
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

function DeleteDatabaseDialog({
  db,
  onClose,
}: {
  db: DatabaseItem | null;
  onClose: () => void;
}) {
  const deleteDb = useDeleteDatabase({
    onSuccess: () => {
      toast.success("Database deleted successfully");
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
          <AlertDialogTitle>Delete Database</AlertDialogTitle>
          <AlertDialogDescription>
            Are you sure you want to delete database "{db?.name}"? This action
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
