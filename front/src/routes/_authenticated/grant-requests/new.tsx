import { createFileRoute } from "@tanstack/react-router";

import { AccessDenied } from "@/components/shared/AccessDenied";
import { RequestAccessForm } from "@/components/grant-requests/RequestAccessForm";
import { useAuth } from "@/contexts/AuthContext";
import { canRequestGrant } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/grant-requests/new")({
  component: RequestAccessPage,
});

function RequestAccessPage() {
  const { user } = useAuth();
  if (!canRequestGrant(user?.roles)) {
    return <AccessDenied message="Your role cannot request access." />;
  }
  return <RequestAccessForm />;
}
