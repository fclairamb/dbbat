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

  // The router hands the segment over decoded on a client-side navigation and,
  // depending on the reload path, still percent-encoded: accept both.
  const parameter = params?.find((p) => {
    const k = parameterRouteKey(p);
    return k === key || encodeURIComponent(k) === key;
  });
  if (!parameter) {
    return (
      <EntityNotFound
        what="Parameter"
        backTo="/settings"
        backLabel="Back to settings"
      />
    );
  }
  // Keyed on the stored value too: the list may have been cached before the
  // last save, and the form must re-seed from the fresh row rather than edit a
  // stale one.
  return (
    <ParameterForm
      key={`${parameter.uid}:${parameter.value}`}
      parameter={parameter}
    />
  );
}
