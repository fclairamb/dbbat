import {
  useState,
} from "react";
import {
  createFileRoute,
  useNavigate,
} from "@tanstack/react-router";
import {
  useDatabases,
  useDeleteDatabase,
  useTunnelServers,
  type Database,
} from "@/api";
import {
  DataTable,
  type Column,
} from "@/components/shared/DataTable";
import {
  PageHeader,
} from "@/components/shared/PageHeader";
import {
  Button,
} from "@/components/ui/button";
import {
  PermissionButton,
} from "@/components/shared/PermissionButton";
import {
  useAuth,
} from "@/contexts/AuthContext";
import { Dialog, DialogTrigger } from "@/components/ui/dialog";
import { CreateDatabaseDialog } from "@/components/servers/ServerForms";
import {
  canCreateDatabase,
  canDeleteDatabase,
  canUpdateDatabase,
  getDisabledReason,
  getActionTooltip,
} from "@/lib/permissions";
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
  Plus,
  Trash2,
  Pencil,
  Tag,
  ShieldCheck,
} from "lucide-react";
import {
  toast,
} from "sonner";
import {
  type DatabaseItem,
  isFullDatabase,
  PROTOCOL_LABEL,
  PROTOCOL_BADGE_CLASS,
  SERVER_NAME_PATTERN,
  NonSlugNameWarning,
  OracleServiceConflictWarning,
  type Protocol,
  TestConnectionButton,
} from "@/components/servers/serverShared";

export const Route = createFileRoute("/_authenticated/servers/")({
  component: ServersPage,
});

function ServersPage() {
  const { user } = useAuth();
  const { data: databases, isLoading } = useDatabases();
  const navigate = useNavigate();
  const [isCreateOpen, setIsCreateOpen] = useState(false);
  const [deleteDb, setDeleteDb] = useState<DatabaseItem | null>(null);

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
              navigate({ to: "/servers/$uid", params: { uid: srv.uid } });
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
          {/* Correcting where the row points — host, port, credentials, SSL
              mode, tunnel — without the delete-and-recreate that would drop
              its grants, its session ledger and its query chains. */}
          {canUpdate && isFullDatabase(db) && (
            <PermissionButton
              data-testid={`database-edit-${db.uid}`}
              variant="ghost"
              size="icon"
              disabled={!canUpdate}
              disabledReason={getDisabledReason("update-database", user?.roles)}
              enabledTooltip="Edit this server's connection details"
              onClick={(e) => {
                e.stopPropagation();
                navigate({ to: "/servers/$uid", params: { uid: db.uid } });
              }}
            >
              <Pencil className="h-4 w-4" />
            </PermissionButton>
          )}
          {/* The rename keeps a button of its own: the name is what every
              client types, so the edit carries a warning the others do not. */}
          <PermissionButton
            data-testid={`database-rename-${db.uid}`}
            variant="ghost"
            size="icon"
            disabled={!canUpdate}
            disabledReason={getDisabledReason("update-database", user?.roles)}
            enabledTooltip="Rename this server"
            onClick={(e) => {
              e.stopPropagation();
              navigate({
                to: "/servers/$uid",
                params: { uid: db.uid },
                hash: "rename",
              });
            }}
          >
            <Tag className="h-4 w-4" />
          </PermissionButton>
          {canUpdate && isFullDatabase(db) && (
            <Button
              variant="ghost"
              size="icon"
              title="Approvers"
              data-testid={`database-approvers-${db.uid}`}
              onClick={(e) => {
                e.stopPropagation();
                navigate({
                  to: "/servers/$uid",
                  params: { uid: db.uid },
                  hash: "approvers",
                });
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
      className: "w-28",
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
                disabledReason={getDisabledReason(
                  "create-database",
                  user?.roles,
                )}
                enabledTooltip={getActionTooltip("create-database")}
              >
                <Plus className="mr-2 h-4 w-4" />
                Add Server
              </PermissionButton>
            </DialogTrigger>
            {/* Mounted only while open: left mounted, the closed DialogContent
                keeps its form state (reopening it after a create showed the
                previous server's host and password) and its overlay stays in
                the DOM for the whole fade-out, where it can swallow a click on
                this very trigger. */}
            {isCreateOpen && (
              <CreateDatabaseDialog onClose={() => setIsCreateOpen(false)} />
            )}
          </Dialog>
        }
      />

      <DataTable
        columns={columns}
        data={databases ?? []}
        isLoading={isLoading}
        rowKey={(db) => db.uid}
        emptyMessage="No databases configured"
        onRowClick={(db) =>
          navigate({ to: "/servers/$uid", params: { uid: db.uid } })
        }
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
    </div>
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
      toast.success(
        isSSH
          ? `${kind} deleted successfully`
          : "Database deleted successfully",
      );
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
          <AlertDialogTitle>
            {isSSH ? `Delete ${kind}` : "Delete Database"}
          </AlertDialogTitle>
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
 * A dialog of its own rather than a field in the general edit form next to it,
 * because a rename is not an edit like the others: it changes what clients
 * type, so it deserves the warning to itself. Tunnel rows get the same field
 * inside their existing edit form (EditSSHServerForm) instead.
 */
