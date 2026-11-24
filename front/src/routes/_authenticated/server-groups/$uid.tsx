import { createFileRoute } from "@tanstack/react-router";

import { useServerGroups } from "@/api";
import { AccessDenied } from "@/components/shared/AccessDenied";
import { EntityNotFound } from "@/components/shared/FormPage";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { ServerGroupForm } from "@/components/server-groups/ServerGroupForm";
import { useAuth } from "@/contexts/AuthContext";
import { canManageServerGroups } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/server-groups/$uid")({
  component: EditServerGroupPage,
});

function EditServerGroupPage() {
  const { uid } = Route.useParams();
  const { user } = useAuth();
  const isAdmin = canManageServerGroups(user?.roles);
  const { data: groups, isLoading } = useServerGroups({ enabled: isAdmin });

  if (!isAdmin) return <AccessDenied requiredRole="admin" />;
  if (isLoading) return <PageLoader />;

  const group = groups?.find((g) => g.uid === uid);
  if (!group) {
    return (
      <EntityNotFound
        what="Server group"
        backTo="/server-groups"
        backLabel="Back to server groups"
      />
    );
  }
  return <ServerGroupForm key={group.uid} editing={group} />;
}
