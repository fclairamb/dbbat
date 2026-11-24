import { createFileRoute } from "@tanstack/react-router";

import { useParameters } from "@/api";
import { AccessDenied } from "@/components/shared/AccessDenied";
import { EntityNotFound } from "@/components/shared/FormPage";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { ParameterForm } from "@/components/settings/ParameterForm";
import { useAuth } from "@/contexts/AuthContext";
import { parameterRouteKey } from "@/lib/parameter-key";

export const Route = createFileRoute("/_authenticated/settings/parameters/$key")({
  component: EditParameterPage,
});

function EditParameterPage() {
  const { key } = Route.useParams();
  const { user } = useAuth();
  const isAdmin = user?.roles?.includes("admin") ?? false;
  const { data: params, isLoading } = useParameters();

  if (!isAdmin) return <AccessDenied requiredRole="admin" />;
  if (isLoading) return <PageLoader />;

  const parameter = params?.find((p) => parameterRouteKey(p) === key);
  if (!parameter) {
    return (
      <EntityNotFound
        what="Parameter"
        backTo="/settings"
        backLabel="Back to settings"
      />
    );
  }
  return <ParameterForm key={parameter.uid} parameter={parameter} />;
}
