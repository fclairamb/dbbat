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
          <span className="font-mono text-sm">{db.database_name}</span>
        ) : (
          <span className="text-muted-foreground">-</span>
        ),
    },
    {
      key: "ssl_mode",
      header: "SSL",
      cell: (db) =>
        isFullDatabase(db) ? (
          <span className="text-sm">{db.ssl_mode}</span>
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
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [host, setHost] = useState("");
  const [port, setPort] = useState("5432");
  const [databaseName, setDatabaseName] = useState("");
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
      database_name: databaseName,
      username,
      password,
      ssl_mode: sslMode,
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
          <div className="space-y-2">
            <Label htmlFor="databaseName">Database Name</Label>
            <Input
              id="databaseName"
              value={databaseName}
              onChange={(e) => setDatabaseName(e.target.value)}
              placeholder="myapp"
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="username">Username</Label>
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="postgres"
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
