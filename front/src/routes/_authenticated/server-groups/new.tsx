import { createFileRoute } from "@tanstack/react-router";

import { AccessDenied } from "@/components/shared/AccessDenied";
import { ServerGroupForm } from "@/components/server-groups/ServerGroupForm";
import { useAuth } from "@/contexts/AuthContext";
import { canManageServerGroups } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/server-groups/new")({
  component: NewServerGroupPage,
});

function NewServerGroupPage() {
  const { user } = useAuth();
  if (!canManageServerGroups(user?.roles)) {
    return <AccessDenied requiredRole="admin" />;
  }
  return <ServerGroupForm editing={null} />;
}
