import { createFileRoute } from "@tanstack/react-router";

import {
  useUsers,
  useSsoRoleMapping,
  useLastRoleSyncs,
} from "@/api";
import { AccessDenied } from "@/components/shared/AccessDenied";
import { EntityNotFound } from "@/components/shared/FormPage";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { EditUserForm } from "@/components/users/UserForms";
import { useAuth } from "@/contexts/AuthContext";
import { canUpdateUser, canViewAudit } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/users/$uid")({
  component: EditUserPage,
});

function EditUserPage() {
  const { uid } = Route.useParams();
  const { user } = useAuth();
  const canUpdate = canUpdateUser(user?.roles);

  const { data: users, isLoading } = useUsers();
  const { data: roleMapping } = useSsoRoleMapping();
  const { data: lastSyncs } = useLastRoleSyncs({
    enabled: canViewAudit(user?.roles),
  });

  if (!canUpdate) return <AccessDenied requiredRole="admin" />;
  if (isLoading) return <PageLoader />;

  const target = users?.find((u) => u.uid === uid);
  if (!target) {
    return (
      <EntityNotFound
        what="User"
        backTo="/users"
        backLabel="Back to users"
      />
    );
  }

  const adminCount =
    users?.filter((u) => u.roles?.includes("admin")).length ?? 0;
  const managedRoles = new Set<string>(
    roleMapping?.enabled ? roleMapping.roles : [],
  );

  return (
    <EditUserForm
      key={target.uid}
      user={target}
      currentUserUid={user?.uid}
      isLastAdmin={
        (target.roles?.includes("admin") ?? false) && adminCount <= 1
      }
      managedRoles={managedRoles}
      lastRoleSync={lastSyncs?.get(target.uid)}
    />
  );
}
