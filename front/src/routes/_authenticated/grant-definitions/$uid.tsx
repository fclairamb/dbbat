import { createFileRoute } from "@tanstack/react-router";

import { useGrantDefinitions } from "@/api";
import { AccessDenied } from "@/components/shared/AccessDenied";
import { EntityNotFound } from "@/components/shared/FormPage";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { GrantDefinitionForm } from "@/components/grant-definitions/GrantDefinitionForm";
import { useAuth } from "@/contexts/AuthContext";
import { canManageGrantDefinitions } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/grant-definitions/$uid")({
  component: EditGrantDefinitionPage,
});

// Definitions are immutably versioned: a uid names one version, and saving
// creates a successor. Only the live row of a lineage is editable, so the
// lookup is over active definitions — a superseded or deactivated uid resolves
// to the not-found state, never to a form that would fork from a stale version.
function EditGrantDefinitionPage() {
  const { uid } = Route.useParams();
  const { user } = useAuth();
  const isAdmin = canManageGrantDefinitions(user?.roles);
  const { data: definitions, isLoading } = useGrantDefinitions({
    active_only: true,
  });

  if (!isAdmin) return <AccessDenied requiredRole="admin" />;
  if (isLoading) return <PageLoader />;

  const definition = definitions?.find((d) => d.uid === uid && d.is_active);
  if (!definition) {
    return (
      <EntityNotFound
        what="Grant definition"
        backTo="/grant-definitions"
        backLabel="Back to grant definitions"
        detail="It may not exist, or it was archived: a newer version replaced it, or it was deactivated."
      />
    );
  }
  return <GrantDefinitionForm key={definition.uid} editing={definition} />;
}
