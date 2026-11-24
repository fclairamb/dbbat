import { createFileRoute } from "@tanstack/react-router";

import { AccessDenied } from "@/components/shared/AccessDenied";
import { GrantDefinitionForm } from "@/components/grant-definitions/GrantDefinitionForm";
import { useAuth } from "@/contexts/AuthContext";
import { canManageGrantDefinitions } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/grant-definitions/new")({
  component: NewGrantDefinitionPage,
});

function NewGrantDefinitionPage() {
  const { user } = useAuth();
  if (!canManageGrantDefinitions(user?.roles)) {
    return <AccessDenied requiredRole="admin" />;
  }
  return <GrantDefinitionForm editing={null} />;
}
