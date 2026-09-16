import { createFileRoute, Link } from "@tanstack/react-router";
import { useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  useConnection,
  useUsers,
  useDatabases,
  useDownloadConnectionDump,
  useTerminateConnection,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { Download, Ban } from "lucide-react";
import { toast } from "sonner";
import { format } from "date-fns";
import { useBreadcrumbTitle } from "@/contexts/BreadcrumbContext";
import { useAuth } from "@/contexts/AuthContext";
import { useEventStream } from "@/hooks/use-event-stream";
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

// terminationReasonLabel renders connections.termination_reason. The stored
// vocabulary is a small closed set; an unknown value is shown verbatim rather
// than hidden, so a reason added on the backend is visible before the UI knows
// about it.
function terminationReasonLabel(reason: string): string {
  switch (reason) {
    case "statement_timeout":
      return "Statement exceeded the per-statement time limit";
    case "grant_expired":
      return "The grant expired mid-session";
    case "quota_exceeded":
      return "The grant's quota was exhausted mid-session";
    case "grant_revoked":
      return "The grant was revoked mid-session";
    case "admin_terminated":
      return "An administrator ended this session";
    case "instance_lost":
      return "The dbbat process serving this session stopped; the row was closed by the crash reconcile";
    default:
      return reason;
  }
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
  const queryClient = useQueryClient();
  const [isTerminateOpen, setIsTerminateOpen] = useState(false);
  const downloadDump = useDownloadConnectionDump(uid, {
    onError: (error) => toast.error(error.message),
  });

  const isLive = !!connection && !connection.disconnected_at;

  // A terminate is asynchronous — the replica that owns the session may be a
  // different one — so the page learns the session ended the same way it learns
  // anything else about a live session: from the stream. The connections topic
  // is admin-only, which is also exactly who can terminate.
  const onConnectionEvent = useCallback(
    (event: { event: string; data: { connection_uid?: string } }) => {
      if (event.event !== "connection" || event.data.connection_uid !== uid) {
        return;
      }

      void queryClient.invalidateQueries({ queryKey: ["connections", uid] });
    },
    [queryClient, uid],
  );

  const onConnectionGap = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["connections", uid] });
  }, [queryClient, uid]);

  useEventStream({
    topics: ["connections"],
    onEvent: onConnectionEvent,
    onGap: onConnectionGap,
    enabled: isAdmin && isLive,
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
            {isAdmin && isLive && (
              <Button
                variant="destructive"
                size="sm"
                data-testid="terminate-session-button"
                onClick={() => setIsTerminateOpen(true)}
              >
                <Ban className="h-4 w-4 mr-1.5" />
                Terminate session
              </Button>
            )}
            {connection.disconnected_at ? (
              connection.termination_reason ? (
                // A session dbbat ended is not the same fact as one that ended
                // — the badge says which, so the page does not read as an
                // ordinary disconnect when it was a kill.
                <Badge
                  variant="destructive"
                  data-testid="connection-terminated-badge"
                >
                  Terminated
                </Badge>
              ) : (
                <Badge variant="secondary">Disconnected</Badge>
              )
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
            {connection.termination_reason && (
              <div>
                <dt className="text-sm font-medium text-muted-foreground mb-1">
                  Ended by dbbat
                </dt>
                <dd data-testid="connection-termination-reason">
                  {terminationReasonLabel(connection.termination_reason)}
                  {connection.terminated_by && (
                    <span data-testid="connection-terminated-by">
                      {" — "}
                      {connection.terminated_by.username}
                    </span>
                  )}
                </dd>
                {connection.terminate_reason && (
                  <dd
                    className="mt-1 text-xs text-muted-foreground"
                    data-testid="connection-terminate-reason"
                  >
                    “{connection.terminate_reason}”
                  </dd>
                )}
              </div>
            )}
            {/* A request that has not been acted on yet: the owning replica
                polls every couple of seconds, so this state is brief — but a
                session that closed on its own in between keeps it forever, and
                showing it is the only way that reads as what it was. */}
            {!connection.termination_reason &&
              connection.terminate_requested_at && (
                <div>
                  <dt className="text-sm font-medium text-muted-foreground mb-1">
                    Termination requested
                  </dt>
                  <dd data-testid="connection-terminate-requested">
                    {format(
                      new Date(connection.terminate_requested_at),
                      "PPpp",
                    )}
                    {connection.terminated_by &&
                      ` by ${connection.terminated_by.username}`}
                  </dd>
                </div>
              )}
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
        statementsRetained={connection.statements_retained}
      />

      <TerminateSessionDialog
        uid={uid}
        open={isTerminateOpen}
        onOpenChange={setIsTerminateOpen}
        username={getUserName(connection.user_id)}
        databaseName={getDbName(connection.database_id)}
        grantUid={connection.grant?.uid}
      />
    </div>
  );
}

// TerminateSessionDialog confirms ending one live session.
//
// It says the two things an admin gets wrong otherwise. First, that the running
// statement is cancelled upstream — this is not a polite "please stop", the
// backend's work is killed. Second, that terminating is *not* revoking: the
// grant is untouched, so the same user can reconnect a second later, and if
// that is not what was wanted the grants page is one click away.
function TerminateSessionDialog({
  uid,
  open,
  onOpenChange,
  username,
  databaseName,
  grantUid,
}: {
  uid: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  username: string;
  databaseName: string;
  grantUid?: string;
}) {
  const [reason, setReason] = useState("");

  const terminate = useTerminateConnection(uid, {
    onSuccess: ({ local }) => {
      toast.success(
        local
          ? "Session terminated"
          : "Termination requested — the replica serving this session will end it within a few seconds",
      );
      setReason("");
      onOpenChange(false);
    },
    onError: (error) => toast.error(error.message),
  });

  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent data-testid="terminate-session-dialog">
        <AlertDialogHeader>
          <AlertDialogTitle>Terminate session</AlertDialogTitle>
          <AlertDialogDescription asChild>
            <div className="space-y-2">
              <p>
                End {username}'s session on{" "}
                <span className="font-mono">{databaseName}</span>? Whatever
                statement is running will be cancelled on the database itself,
                then both legs of the connection are dropped.
              </p>
              <p>
                This does not revoke access — {username} can reconnect
                immediately under the same grant.
                {grantUid && (
                  <>
                    {" "}
                    To withdraw the access itself,{" "}
                    <Link
                      to="/grants"
                      className="underline hover:text-foreground"
                      data-testid="terminate-revoke-grant-link"
                    >
                      revoke the grant
                    </Link>{" "}
                    instead.
                  </>
                )}
              </p>
            </div>
          </AlertDialogDescription>
        </AlertDialogHeader>
        <div className="space-y-2">
          <Label htmlFor="terminate-reason">Reason (optional)</Label>
          <Textarea
            id="terminate-reason"
            data-testid="terminate-reason-input"
            value={reason}
            maxLength={1000}
            onChange={(e) => setReason(e.target.value)}
            placeholder="Recorded in the audit log; never shown to the user whose session ends."
          />
        </div>
        <AlertDialogFooter>
          <AlertDialogCancel data-testid="terminate-cancel">
            Cancel
          </AlertDialogCancel>
          <AlertDialogAction
            data-testid="terminate-confirm"
            disabled={terminate.isPending}
            onClick={(e) => {
              // The dialog would otherwise close on click, unmounting the
              // mutation before its toast can say whether the session was ended
              // here or handed to another replica.
              e.preventDefault();
              terminate.mutate(reason);
            }}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            {terminate.isPending ? "Terminating..." : "Terminate"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
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
