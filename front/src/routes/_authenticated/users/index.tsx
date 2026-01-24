import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useUsers, useCreateUser, useDeleteUser, type User } from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { Button } from "@/components/ui/button";
import { PermissionButton } from "@/components/shared/PermissionButton";
import { useAuth } from "@/contexts/AuthContext";
import {
  canCreateUser,
  canDeleteUser,
  canResetPassword,
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { ResetPasswordDialog } from "@/components/shared/ResetPasswordDialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Plus, Trash2, MoreHorizontal, KeyRound } from "lucide-react";
import { toast } from "sonner";
import { formatDistanceToNow } from "date-fns";

export const Route = createFileRoute("/_authenticated/users/")({
  component: UsersPage,
});

function UsersPage() {
  const { user } = useAuth();
  const { data: users, isLoading } = useUsers();
  const [isCreateOpen, setIsCreateOpen] = useState(false);
  const [deleteUser, setDeleteUser] = useState<User | null>(null);
  const [resetPasswordUser, setResetPasswordUser] = useState<User | null>(null);

  const canCreate = canCreateUser(user?.roles);
  const canDelete = canDeleteUser(user?.roles);
  const canReset = canResetPassword(user?.roles);

  const columns: Column<User>[] = [
    {
      key: "username",
      header: "Username",
      cell: (u) => <span className="font-medium">{u.username}</span>,
    },
    {
      key: "roles",
      header: "Roles",
      cell: (u) => (
        <div className="flex gap-1 flex-wrap">
          {u.roles?.map((role) => (
            <Badge key={role} variant={role === "admin" ? "default" : "secondary"}>
              {role}
            </Badge>
          ))}
        </div>
      ),
    },
    {
      key: "rate_limit_exempt",
      header: "Rate Limit",
      cell: (u) =>
        u.rate_limit_exempt ? (
          <Badge variant="outline">Exempt</Badge>
        ) : (
          <span className="text-muted-foreground">Standard</span>
        ),
    },
    {
      key: "created_at",
      header: "Created",
      cell: (u) => (
        <span className="text-sm text-muted-foreground">
          {formatDistanceToNow(new Date(u.created_at), { addSuffix: true })}
        </span>
      ),
    },
    {
      key: "actions",
      header: "",
      cell: (u) => (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              data-testid={`user-actions-${u.username}`}
            >
              <MoreHorizontal className="h-4 w-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {canReset ? (
              <DropdownMenuItem
                onClick={() => setResetPasswordUser(u)}
                data-testid={`reset-password-${u.username}`}
              >
                <KeyRound className="mr-2 h-4 w-4" />
                Reset Password
              </DropdownMenuItem>
            ) : (
              <Tooltip>
                <TooltipTrigger asChild>
                  <DropdownMenuItem disabled>
                    <KeyRound className="mr-2 h-4 w-4" />
                    Reset Password
                  </DropdownMenuItem>
                </TooltipTrigger>
                <TooltipContent>
                  {getDisabledReason("reset-password", user?.roles)}
                </TooltipContent>
              </Tooltip>
            )}
            {canDelete ? (
              <DropdownMenuItem
                onClick={() => setDeleteUser(u)}
                className="text-destructive focus:text-destructive"
                data-testid={`delete-user-${u.username}`}
              >
                <Trash2 className="mr-2 h-4 w-4" />
                Delete User
              </DropdownMenuItem>
            ) : (
              <Tooltip>
                <TooltipTrigger asChild>
                  <DropdownMenuItem disabled>
                    <Trash2 className="mr-2 h-4 w-4" />
                    Delete User
                  </DropdownMenuItem>
                </TooltipTrigger>
                <TooltipContent>
                  {getDisabledReason("delete-user", user?.roles)}
                </TooltipContent>
              </Tooltip>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      ),
      className: "w-10",
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Users"
        description="Manage user accounts"
        actions={
          <Dialog open={isCreateOpen} onOpenChange={setIsCreateOpen}>
            <DialogTrigger asChild>
              <PermissionButton
                disabled={!canCreate}
                disabledReason={getDisabledReason("create-user", user?.roles)}
                enabledTooltip={getActionTooltip("create-user")}
              >
                <Plus className="mr-2 h-4 w-4" />
                Create User
              </PermissionButton>
            </DialogTrigger>
            <CreateUserDialog onClose={() => setIsCreateOpen(false)} />
          </Dialog>
        }
      />

      <DataTable
        columns={columns}
        data={users ?? []}
        isLoading={isLoading}
        rowKey={(u) => u.uid}
        emptyMessage="No users found"
      />

      <DeleteUserDialog user={deleteUser} onClose={() => setDeleteUser(null)} />

      {resetPasswordUser && (
        <ResetPasswordDialog
          user={resetPasswordUser}
          open={!!resetPasswordUser}
          onOpenChange={(open) => !open && setResetPasswordUser(null)}
          onSuccess={() => {
            toast.success(`Password reset for ${resetPasswordUser.username}`);
          }}
        />
      )}
    </div>
  );
}

function CreateUserDialog({ onClose }: { onClose: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [isAdmin, setIsAdmin] = useState(false);

  const createUser = useCreateUser({
    onSuccess: () => {
      toast.success("User created successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createUser.mutate({
      username,
      password,
      roles: isAdmin ? ["admin"] : ["connector"],
    });
  };

  return (
    <DialogContent>
      <form onSubmit={handleSubmit}>
        <DialogHeader>
          <DialogTitle>Create User</DialogTitle>
          <DialogDescription>Add a new user to the system.</DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label htmlFor="username">Username</Label>
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
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
          <div className="flex items-center space-x-2">
            <Checkbox
              id="isAdmin"
              checked={isAdmin}
              onCheckedChange={(checked) => setIsAdmin(checked === true)}
            />
            <Label htmlFor="isAdmin">Admin user</Label>
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={createUser.isPending}>
            Create
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}

function DeleteUserDialog({
  user,
  onClose,
}: {
  user: User | null;
  onClose: () => void;
}) {
  const deleteUser = useDeleteUser({
    onSuccess: () => {
      toast.success("User deleted successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  return (
    <AlertDialog open={!!user} onOpenChange={() => onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete User</AlertDialogTitle>
          <AlertDialogDescription>
            Are you sure you want to delete user "{user?.username}"? This action
            cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => user && deleteUser.mutate(user.uid)}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
