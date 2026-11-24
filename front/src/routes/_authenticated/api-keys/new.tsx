import { createFileRoute } from "@tanstack/react-router";

import { AccessDenied } from "@/components/shared/AccessDenied";
import { CreateKeyForm } from "@/components/api-keys/CreateKeyForm";
import { useAuth } from "@/contexts/AuthContext";
import { canCreateAPIKey } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/api-keys/new")({
  component: NewAPIKeyPage,
});

function NewAPIKeyPage() {
  const { user } = useAuth();
  if (!canCreateAPIKey(user?.roles)) {
    return <AccessDenied message="Your role cannot create API keys." />;
  }
  return <CreateKeyForm />;
}
