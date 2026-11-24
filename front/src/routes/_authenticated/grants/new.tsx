import { createFileRoute } from "@tanstack/react-router";

import { AccessDenied } from "@/components/shared/AccessDenied";
import { AssignGrantForm } from "@/components/grants/AssignGrantForm";
import { useAuth } from "@/contexts/AuthContext";
import { canCreateGrant } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/grants/new")({
  component: AssignGrantPage,
});

function AssignGrantPage() {
  const { user } = useAuth();
  if (!canCreateGrant(user?.roles)) {
    return <AccessDenied requiredRole="admin" />;
  }
  return <AssignGrantForm />;
}
