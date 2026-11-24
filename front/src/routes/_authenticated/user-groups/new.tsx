import { createFileRoute } from "@tanstack/react-router";

import { AccessDenied } from "@/components/shared/AccessDenied";
import { UserGroupForm } from "@/components/user-groups/UserGroupForm";
import { useAuth } from "@/contexts/AuthContext";
import { canManageUserGroups } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/user-groups/new")({
  component: NewUserGroupPage,
});

function NewUserGroupPage() {
  const { user } = useAuth();
  if (!canManageUserGroups(user?.roles)) {
    return <AccessDenied requiredRole="admin" />;
  }
  return <UserGroupForm editing={null} />;
}
