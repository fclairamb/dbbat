import { createFileRoute } from "@tanstack/react-router";

import { AccessDenied } from "@/components/shared/AccessDenied";
import { CreateUserForm } from "@/components/users/UserForms";
import { useAuth } from "@/contexts/AuthContext";
import { canCreateUser } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/users/new")({
  component: NewUserPage,
});

function NewUserPage() {
  const { user } = useAuth();
  if (!canCreateUser(user?.roles)) {
    return <AccessDenied requiredRole="admin" />;
  }
  return <CreateUserForm />;
}
