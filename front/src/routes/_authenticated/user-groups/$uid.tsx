import { createFileRoute } from "@tanstack/react-router";

import { useUserGroups } from "@/api";
import { AccessDenied } from "@/components/shared/AccessDenied";
import { EntityNotFound } from "@/components/shared/FormPage";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { UserGroupForm } from "@/components/user-groups/UserGroupForm";
import { useAuth } from "@/contexts/AuthContext";
import { canManageUserGroups } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/user-groups/$uid")({
  component: EditUserGroupPage,
});

function EditUserGroupPage() {
  const { uid } = Route.useParams();
  const { user } = useAuth();
  const isAdmin = canManageUserGroups(user?.roles);
  const { data: groups, isLoading } = useUserGroups({ enabled: isAdmin });

  if (!isAdmin) return <AccessDenied requiredRole="admin" />;
  if (isLoading) return <PageLoader />;

  const group = groups?.find((g) => g.uid === uid);
  if (!group) {
    return (
      <EntityNotFound
        what="User group"
        backTo="/user-groups"
        backLabel="Back to user groups"
      />
    );
  }
  return <UserGroupForm key={group.uid} editing={group} />;
}
