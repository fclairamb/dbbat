import { createFileRoute } from "@tanstack/react-router";

import { AccessDenied } from "@/components/shared/AccessDenied";
import { CreateServerForm } from "@/components/servers/ServerForms";
import { useAuth } from "@/contexts/AuthContext";
import { canCreateDatabase } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/servers/new")({
  component: NewServerPage,
});

function NewServerPage() {
  const { user } = useAuth();
  if (!canCreateDatabase(user?.roles)) {
    return <AccessDenied requiredRole="admin" />;
  }
  return <CreateServerForm />;
}
