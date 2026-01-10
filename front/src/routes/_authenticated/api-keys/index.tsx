import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  useAPIKeys,
  useCreateAPIKey,
  useRevokeAPIKey,
  type APIKey,
  type CreateAPIKeyResponse,
} from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { Button } from "@/components/ui/button";
import { PermissionButton } from "@/components/shared/PermissionButton";
import { useAuth } from "@/contexts/AuthContext";
import {
  canCreateAPIKey,
  canRevokeAPIKey,
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
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Plus, Ban, Copy, Key, AlertTriangle } from "lucide-react";
import { toast } from "sonner";
import { formatDistanceToNow, format } from "date-fns";

export const Route = createFileRoute("/_authenticated/api-keys/")({
  component: APIKeysPage,
});

function APIKeysPage() {
  const { user } = useAuth();
  const { data: keys, isLoading } = useAPIKeys();
  const [isCreateOpen, setIsCreateOpen] = useState(false);
  const [newKey, setNewKey] = useState<CreateAPIKeyResponse | null>(null);
  const [revokeKey, setRevokeKey] = useState<APIKey | null>(null);

  const canCreate = canCreateAPIKey(user?.roles);
  const canRevoke = canRevokeAPIKey(user?.roles);

  const getStatus = (key: APIKey) => {
    if (key.revoked_at) return "revoked";
    if (key.expires_at && new Date(key.expires_at) < new Date())
      return "expired";
    return "active";
  };

  const columns: Column<APIKey>[] = [
    {
      key: "name",
      header: "Name",
      cell: (k) => <span className="font-medium">{k.name}</span>,
    },
    {
      key: "key_prefix",
      header: "Key",
      cell: (k) => (
        <span className="font-mono text-sm">{k.key_prefix}...</span>
      ),
    },
    {
      key: "status",
      header: "Status",
      cell: (k) => {
        const status = getStatus(k);
        const variants: Record<string, "default" | "secondary" | "destructive"> = {
          active: "default",
          expired: "secondary",
          revoked: "destructive",
        };
        return <Badge variant={variants[status]}>{status}</Badge>;
      },
    },
    {
      key: "expires_at",
      header: "Expires",
      cell: (k) =>
        k.expires_at ? (
          <span className="text-sm text-muted-foreground">
            {format(new Date(k.expires_at), "MMM d, yyyy")}
          </span>
        ) : (
          <span className="text-muted-foreground">Never</span>
        ),
    },
    {
      key: "last_used_at",
      header: "Last Used",
      cell: (k) =>
        k.last_used_at ? (
          <span className="text-sm text-muted-foreground">
            {formatDistanceToNow(new Date(k.last_used_at), { addSuffix: true })}
          </span>
        ) : (
          <span className="text-muted-foreground">Never</span>
        ),
    },
    {
      key: "request_count",
      header: "Requests",
      cell: (k) => <span>{k.request_count}</span>,
    },
    {
      key: "actions",
      header: "",
      cell: (k) =>
        getStatus(k) === "active" && (
          <PermissionButton
            variant="ghost"
            size="icon"
            disabled={!canRevoke}
            disabledReason={getDisabledReason("revoke-api-key", user?.roles)}
            enabledTooltip={getActionTooltip("revoke-api-key")}
            onClick={(e) => {
              e.stopPropagation();
              setRevokeKey(k);
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
        title="API Keys"
        description="Manage API authentication keys"
        actions={
          <Dialog open={isCreateOpen} onOpenChange={setIsCreateOpen}>
            <DialogTrigger asChild>
              <PermissionButton
                disabled={!canCreate}
                disabledReason={getDisabledReason("create-api-key", user?.roles)}
                enabledTooltip={getActionTooltip("create-api-key")}
              >
                <Plus className="mr-2 h-4 w-4" />
                Create Key
              </PermissionButton>
            </DialogTrigger>
            <CreateKeyDialog
              onClose={() => setIsCreateOpen(false)}
              onCreated={setNewKey}
            />
          </Dialog>
        }
      />

      <DataTable
        columns={columns}
        data={keys ?? []}
        isLoading={isLoading}
        rowKey={(k) => k.id}
        emptyMessage="No API keys found"
      />

      <ShowKeyDialog newKey={newKey} onClose={() => setNewKey(null)} />
      <RevokeKeyDialog apiKey={revokeKey} onClose={() => setRevokeKey(null)} />
    </div>
  );
}

function CreateKeyDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (key: CreateAPIKeyResponse) => void;
}) {
  const [name, setName] = useState("");
  const [expiresAt, setExpiresAt] = useState("");

  const createKey = useCreateAPIKey({
    onSuccess: (data) => {
      onCreated(data);
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createKey.mutate({
      name,
      expires_at: expiresAt ? new Date(expiresAt).toISOString() : undefined,
    });
  };

  return (
    <DialogContent>
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Create API Key</DialogTitle>
          <DialogDescription>
            Create a new API key for programmatic access.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label htmlFor="name">Name</Label>
            <Input
              id="name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="My API Key"
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="expiresAt">Expires At (optional)</Label>
            <Input
              id="expiresAt"
              type="date"
              value={expiresAt}
              onChange={(e) => setExpiresAt(e.target.value)}
            />
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={createKey.isPending || !name}>
            Create
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

function ShowKeyDialog({
  newKey,
  onClose,
}: {
  newKey: CreateAPIKeyResponse | null;
  onClose: () => void;
}) {
  const copyKey = () => {
    if (newKey) {
      navigator.clipboard.writeText(newKey.key);
      toast.success("API key copied to clipboard");
    }
  };

  return (
    <Dialog open={!!newKey} onOpenChange={() => onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <Key className="h-5 w-5" />
            API Key Created
          </DialogTitle>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <Alert>
            <AlertTriangle className="h-4 w-4" />
            <AlertTitle>Important</AlertTitle>
            <AlertDescription>
              This is the only time you will see this key. Copy it now and store
              it securely.
            </AlertDescription>
          </Alert>
          <div className="space-y-2">
            <Label>API Key</Label>
            <div className="flex gap-2">
              <Input
                readOnly
                value={newKey?.key ?? ""}
                className="font-mono text-sm"
              />
              <Button type="button" variant="outline" size="icon" onClick={copyKey}>
                <Copy className="h-4 w-4" />
              </Button>
            </div>
          </div>
        </div>
        <DialogFooter>
          <Button onClick={onClose}>Done</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function RevokeKeyDialog({
  apiKey,
  onClose,
}: {
  apiKey: APIKey | null;
  onClose: () => void;
}) {
  const revokeKey = useRevokeAPIKey({
    onSuccess: () => {
      toast.success("API key revoked successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  return (
    <AlertDialog open={!!apiKey} onOpenChange={() => onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Revoke API Key</AlertDialogTitle>
          <AlertDialogDescription>
            Are you sure you want to revoke "{apiKey?.name}"? This action cannot
            be undone and the key will immediately stop working.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => apiKey && revokeKey.mutate(apiKey.id)}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            Revoke
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
