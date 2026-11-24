import { useEffect } from "react";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";

import { useDatabases, useTunnelServers } from "@/api";
import { EntityNotFound } from "@/components/shared/FormPage";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { Button } from "@/components/ui/button";
import {
  EditDatabaseForm,
  EditServerApproversForm,
  EditSSHServerForm,
  RenameServerForm,
  ServerDetailsSection,
} from "@/components/servers/ServerForms";
import { isFullDatabase } from "@/components/servers/serverShared";
import { useAuth } from "@/contexts/AuthContext";
import { canCreateDatabase, canUpdateDatabase } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/servers/$uid")({
  component: ServerPage,
});

/**
 * One page per server, with a section per concern: details (everyone, the
 * connection URL is for connectors), connection edit, rename (its own warning)
 * and approvers (admins). Tunnel servers (SSH bastions, Kubernetes clusters)
 * resolve to their own edit section. Each section saves on its own because the
 * API calls behind them are separate.
 */
function ServerPage() {
  const { uid } = Route.useParams();
  const { user } = useAuth();
  const navigate = useNavigate();
  const canUpdate = canUpdateDatabase(user?.roles);
  const { data: databases, isLoading } = useDatabases();
  const { data: tunnelServers, isLoading: tunnelsLoading } = useTunnelServers(
    canCreateDatabase(user?.roles),
  );

  const hash = typeof window === "undefined" ? "" : window.location.hash;
  const ready = !isLoading && !tunnelsLoading;
  useEffect(() => {
    if (!ready || !hash) return;
    document.getElementById(hash.slice(1))?.scrollIntoView();
  }, [ready, hash]);

  if (!ready) return <PageLoader />;

  const goBack = () => navigate({ to: "/servers" });
  const db = databases?.find((d) => d.uid === uid);
  const tunnel = tunnelServers?.find((t) => t.uid === uid);

  if (!db && !tunnel) {
    return (
      <EntityNotFound
        what="Server"
        backTo="/servers"
        backLabel="Back to servers"
      />
    );
  }

  const name = (db ?? tunnel)?.name;

  return (
    <div className="container mx-auto py-6" data-testid="server-page">
      <Link
        to="/servers"
        className="mb-4 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
        data-testid="back-to-list"
      >
        <ArrowLeft className="h-4 w-4" />
        Servers
      </Link>
      <h1 className="text-2xl font-semibold tracking-tight">{name}</h1>

      <div className="mt-6 space-y-6">
        {db && <ServerDetailsSection db={db} />}
        {db && canUpdate && isFullDatabase(db) && (
          <>
            <div id="edit">
              <EditDatabaseForm key={`edit-${db.uid}`} server={db} onClose={goBack} />
            </div>
            <div id="rename">
              <RenameServerForm key={`rename-${db.uid}`} server={db} onClose={goBack} />
            </div>
            <div id="approvers">
              <EditServerApproversForm
                key={`approvers-${db.uid}`}
                server={db}
                onClose={goBack}
              />
            </div>
          </>
        )}
        {db && canUpdate && !isFullDatabase(db) && (
          <div id="rename">
            <RenameServerForm key={`rename-${db.uid}`} server={db} onClose={goBack} />
          </div>
        )}
        {tunnel && canUpdate && (
          <EditSSHServerForm key={tunnel.uid} server={tunnel} onClose={goBack} />
        )}
      </div>

      <div className="sticky bottom-0 -mx-4 mt-8 flex justify-end border-t bg-background px-4 py-3">
        <Button
          type="button"
          variant="outline"
          onClick={goBack}
          data-testid="form-cancel"
        >
          Back to servers
        </Button>
      </div>
    </div>
  );
}
