import { createFileRoute, Link } from "@tanstack/react-router";
import {
  useConnection,
  useUsers,
  useDatabases,
  useDownloadConnectionDump,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { Download } from "lucide-react";
import { toast } from "sonner";
import { format } from "date-fns";
import { useBreadcrumbTitle } from "@/contexts/BreadcrumbContext";
import { useAuth } from "@/contexts/AuthContext";
import { ConnectionQueryFeed } from "@/components/shared/ConnectionQueryFeed";
import { UpstreamTlsIndicator } from "@/components/shared/UpstreamTlsIndicator";
import {
  upstreamTlsExplanation,
  upstreamTlsState,
} from "@/lib/upstream-tls";
import { formatBytes } from "@/lib/utils";
import { formatDateTime } from "@/lib/date-utils";

// Mirrors the Grants page's control-name formatting: "block_ddl" -> "Block Ddl".
function formatControlName(control: string): string {
  return control
    .replace(/_/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

export const Route = createFileRoute("/_authenticated/connections/$uid")({
  // ?watch=1 is the deep link Slack approval notifications point at. It is now
  // a no-op: an open connection streams from mount, so the approver lands on
  // the hold whichever way they arrive. It is still accepted (and still
  // validated) because links already sent out carry it.
  //
  // The key is omitted entirely when it is off rather than returned as false:
  // the router serializes whatever this returns, so `watch: false` would stamp
  // `?watch=false` onto every connection detail URL. A default state does not
  // belong in the URL — it makes shared links noisy and breaks anything
  // matching the bare `/connections/<uid>` form.
  validateSearch: (search: Record<string, unknown>): { watch?: true } =>
    search.watch === "1" || search.watch === 1 || search.watch === true
      ? { watch: true }
      : {},
  component: ConnectionDetailPage,
});

function ConnectionDetailPage() {
  const { uid } = Route.useParams();
  const { isAdmin } = useAuth();
  const { data: connection, isLoading: isLoadingConnection } =
    useConnection(uid);
  const { data: users } = useUsers();
  const { data: databases } = useDatabases();
  const downloadDump = useDownloadConnectionDump(uid, {
    onError: (error) => toast.error(error.message),
  });

  const getUserName = (userId: string) =>
    users?.find((u) => u.uid === userId)?.username ?? userId;
  const getDbName = (databaseId: string) =>
    databases?.find((d) => d.uid === databaseId)?.name ?? databaseId;

  // Publish a "Connections › username @ database" breadcrumb once the
  // connection (and the users/databases needed to resolve it) has loaded.
  useBreadcrumbTitle(
    `/connections/${uid}`,
    connection
      ? `${getUserName(connection.user_id)} @ ${getDbName(connection.database_id)}`
      : undefined,
  );

  if (isLoadingConnection) {
    return <PageLoader />;
  }

  if (!connection) {
    return (
      <div className="text-center text-muted-foreground py-12">
        Connection not found
      </div>
    );
  }

  // The protocol and ssl_mode fields only exist on the full (admin) server
  // payload — a viewer or connector gets uid/name/description and nothing
  // more, so both stay undefined for them and the indicator degrades to the
  // honest, unattributed reading.
  const server = databases?.find((d) => d.uid === connection.database_id);
  const fullServer = server && "host" in server ? server : undefined;
  const tlsState = upstreamTlsState(
    connection.upstream_tls,
    fullServer?.protocol,
  );

  const end = connection.disconnected_at
    ? new Date(connection.disconnected_at)
    : new Date();
  const start = new Date(connection.connected_at);
  const durationMs = end.getTime() - start.getTime();
  const durationLabel = formatDuration(durationMs);

  return (
    <div className="space-y-6">
      <PageHeader
        title={`${getUserName(connection.user_id)} @ ${getDbName(connection.database_id)}`}
        description={`Connected ${format(new Date(connection.connected_at), "PPpp")}`}
        actions={
          <>
            {isAdmin && connection.dump?.available && (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    variant="outline"
                    size="sm"
                    data-testid="download-dump-button"
                    disabled={downloadDump.isPending}
                    onClick={() => downloadDump.mutate()}
                  >
                    <Download className="h-4 w-4 mr-1.5" />
                    {downloadDump.isPending
                      ? "Downloading..."
                      : `Download capture (${formatBytes(connection.dump.size_bytes)})`}
                  </Button>
                </TooltipTrigger>
                <TooltipContent className="max-w-xs">
                  Raw pcapng packet capture — opens in Wireshark or tcpdump.
                  See docs/dump-format.md in the dbbat repository for the
                  format reference, and use{" "}
                  <code className="font-mono">dbbat dump anonymise</code>{" "}
                  before sharing this file with anyone else.
                </TooltipContent>
              </Tooltip>
            )}
            {connection.disconnected_at ? (
              <Badge variant="secondary">Disconnected</Badge>
            ) : (
              <Badge variant="default">Active</Badge>
            )}
          </>
        }
      />

      <Card>
        <CardHeader>
          <CardTitle>Connection Information</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid gap-4 sm:grid-cols-2 md:grid-cols-3">
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                User
              </dt>
              <dd className="font-medium">{getUserName(connection.user_id)}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Database
              </dt>
              <dd className="font-mono text-sm">
                {getDbName(connection.database_id)}
              </dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Source IP
              </dt>
              <dd className="font-mono text-sm">{connection.source_ip}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Connected
              </dt>
              <dd>{format(new Date(connection.connected_at), "PPpp")}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Disconnected
              </dt>
              <dd>
                {connection.disconnected_at
                  ? format(new Date(connection.disconnected_at), "PPpp")
                  : "-"}
              </dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Duration
              </dt>
              <dd>{durationLabel}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Queries
              </dt>
              <dd>{connection.queries}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Data Transferred
              </dt>
              <dd>{formatBytes(connection.bytes_transferred)}</dd>
            </div>
            <div>
              <dt className="text-sm font-medium text-muted-foreground mb-1">
                Upstream Encryption
              </dt>
              <dd className="flex flex-wrap items-center gap-2">
                <UpstreamTlsIndicator
                  upstreamTls={connection.upstream_tls}
                  protocol={fullServer?.protocol}
                  sslMode={fullServer?.ssl_mode}
                />
                {/* The policy sits next to the outcome on purpose: "policy
                    said prefer, outcome was plaintext" is one statement, and
                    reading it as two separate facts is what made a silent
                    downgrade invisible in the first place. */}
                {fullServer && tlsState !== "not-applicable" && (
                  <span
                    className="text-xs text-muted-foreground"
                    data-testid="upstream-tls-policy"
                  >
                    policy:{" "}
                    <span className="font-mono">
                      {fullServer.ssl_mode || "prefer (default)"}
                    </span>
                  </span>
                )}
              </dd>
              {tlsState !== "encrypted" && (
                <p className="mt-1 text-xs text-muted-foreground">
                  {upstreamTlsExplanation(tlsState)}
                </p>
              )}
            </div>
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Grant</CardTitle>
        </CardHeader>
        <CardContent>
          {connection.grant ? (
            <dl
              className="grid gap-4 sm:grid-cols-2 md:grid-cols-4"
              data-testid="connection-grant-summary"
            >
              <div>
                <dt className="text-sm font-medium text-muted-foreground mb-1">
                  Access
                </dt>
                <dd className="flex flex-wrap gap-1">
                  {connection.grant.controls.length === 0 ? (
                    <Badge variant="default">Full Access</Badge>
                  ) : (
                    connection.grant.controls.map((control) => (
                      <Badge key={control} variant="secondary">
                        {formatControlName(control)}
                      </Badge>
                    ))
                  )}
                  {connection.grant.revoked && (
                    <Badge variant="destructive">Revoked</Badge>
                  )}
                </dd>
              </div>
              <div>
                <dt className="text-sm font-medium text-muted-foreground mb-1">
                  Valid
                </dt>
                <dd className="text-sm">
                  {formatDateTime(connection.grant.starts_at)} to{" "}
                  {formatDateTime(connection.grant.expires_at)}
                </dd>
              </div>
              <div>
                <dt className="text-sm font-medium text-muted-foreground mb-1">
                  Priority
                </dt>
                <dd
                  className="font-mono text-sm tabular-nums"
                  data-testid="connection-grant-priority"
                >
                  {connection.grant.priority}
                </dd>
              </div>
              <div>
                <dt className="text-sm font-medium text-muted-foreground mb-1">
                  &nbsp;
                </dt>
                <dd className="flex flex-col gap-1">
                  <Link
                    to="/grants"
                    className="text-sm underline hover:text-foreground"
                    data-testid="connection-grant-link"
                  >
                    View grants
                  </Link>
                  {/* The exact-instance grant filter has no dropdown — a busy
                      instance has many time-boxed grants per user — so this
                      deep-link is how it is reached. */}
                  <Link
                    to="/connections"
                    search={{ grant_uid: connection.grant.uid, size: 50 }}
                    className="text-sm underline hover:text-foreground"
                    data-testid="connection-grant-sessions-link"
                  >
                    Sessions under this grant
                  </Link>
                  <Link
                    to="/queries"
                    search={{ grant_uid: connection.grant.uid, size: 50 }}
                    className="text-sm underline hover:text-foreground"
                    data-testid="connection-grant-queries-link"
                  >
                    Queries under this grant
                  </Link>
                </dd>
              </div>
            </dl>
          ) : (
            <p
              className="text-sm text-muted-foreground"
              data-testid="connection-grant-unavailable"
            >
              {connection.grant_uid
                ? "The grant this session ran under is no longer available."
                : "No grant on record — this connection predates grant tracking."}
            </p>
          )}
        </CardContent>
      </Card>

      {/* History, the live stream and any approval hold, in one table. Live
          from mount while the connection is open — see ConnectionQueryFeed. */}
      <ConnectionQueryFeed
        connectionUid={uid}
        active={!connection.disconnected_at}
      />
    </div>
  );
}

function formatDuration(durationMs: number): string {
  const seconds = Math.floor(durationMs / 1000);
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);

  if (hours > 0) {
    return `${hours}h ${minutes % 60}m`;
  }
  if (minutes > 0) {
    return `${minutes}m ${seconds % 60}s`;
  }
  return `${seconds}s`;
}
