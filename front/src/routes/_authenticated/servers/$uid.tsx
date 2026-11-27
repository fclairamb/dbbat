import { useEffect } from "react";
import { createFileRoute } from "@tanstack/react-router";

import { useDatabases, useTunnelServers } from "@/api";
import { DetailHeader } from "@/components/shared/DetailHeader";
import { EntityNotFound } from "@/components/shared/FormPage";
import { PageLoader } from "@/components/shared/LoadingSpinner";
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

  // Each section saves on its own and stays on the page, so the unsaved edits
  // of the other sections are still there.
  const refreshed = () => {};
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

  const name = (db ?? tunnel)?.name ?? "";

  return (
    <div data-testid="server-page">
      <DetailHeader backTo="/servers" backLabel="Servers" title={name} />

      <div className="mt-6 space-y-6">
        {db && <ServerDetailsSection db={db} />}
        {db && canUpdate && isFullDatabase(db) && (
          <>
            <div id="edit">
              <EditDatabaseForm key={`edit-${db.uid}`} server={db} onSaved={refreshed} />
            </div>
            <div id="rename">
              <RenameServerForm key={`rename-${db.uid}`} server={db} onSaved={refreshed} />
            </div>
            <div id="approvers">
              <EditServerApproversForm
                key={`approvers-${db.uid}`}
                server={db}
                onSaved={refreshed}
              />
            </div>
          </>
        )}
        {db && canUpdate && !isFullDatabase(db) && (
          <div id="rename">
            <RenameServerForm key={`rename-${db.uid}`} server={db} onSaved={refreshed} />
          </div>
        )}
        {tunnel && canUpdate && (
          <EditSSHServerForm key={tunnel.uid} server={tunnel} onSaved={refreshed} />
        )}
      </div>
    </div>
  );
}
