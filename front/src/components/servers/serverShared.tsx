import {
  useTestServerConnection,
  type ConnectionTestResult,
  type Database,
  type DatabaseLimited,
  type OracleServiceNameConflict,
} from "@/api";
import {
  PermissionButton,
} from "@/components/shared/PermissionButton";
import {
  Input,
} from "@/components/ui/input";
import {
  Label,
} from "@/components/ui/label";
import {
  AlertTriangle,
  PlugZap,
  Loader2,
} from "lucide-react";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  toast,
} from "sonner";
import {
  Alert,
  AlertDescription,
} from "@/components/ui/alert";


export type DatabaseItem = Database | DatabaseLimited;

export function isFullDatabase(db: DatabaseItem): db is Database {
  return "host" in db;
}

// SERVER_NAME_PATTERN mirrors the store-level slug check
// (internal/store.IsValidServerName): creation is gated on it, but rows
// created before that gate existed are grandfathered — this is what lets the
// admin UI flag those rows instead of hiding the drift. A hyphen is allowed
// in the interior (e.g. "prod-eu-1") but not leading or trailing; `.` is
// still rejected.
//
// The `-` is escaped (`\-`) inside the character class. A browser compiles
// an HTML `pattern=` attribute under the `v` (unicodeSets) regex flag, where
// an unescaped `-` in a class position like `[a-z0-9_-]` is a compile error
// — and per the HTML spec, a `pattern` that fails to compile is silently
// ignored rather than rejected, so client-side validation goes dead with no
// visible error. Do not "clean up" the backslash; it is load-bearing. Keep
// this in step with serverNamePattern in internal/store/servers.go and the
// `pattern:` fields in internal/api/openapi.yml.
//
// It lives in a *string* rather than a regex literal for two reasons: the
// two `pattern=` attributes below are fed from this one constant, so they
// cannot drift from it, and `no-useless-escape` would (correctly) flag `\-`
// in a literal — a regex literal is compiled without the `v` flag, where the
// escape really is redundant. The attribute is not, which is the whole point.
export const SERVER_NAME_PATTERN_SOURCE =
  "^[a-z0-9_][a-z0-9_\\-]{0,61}[a-z0-9_]$|^[a-z0-9_]$";

export const SERVER_NAME_PATTERN = new RegExp(SERVER_NAME_PATTERN_SOURCE);

// NonSlugNameWarning flags a server row whose name predates the slug gate
// (or was created directly against the store/API). The row itself works, but
// the name is the client-facing selector on every protocol, so a stray space,
// dot or uppercase letter costs reachability somewhere down the line —
// see OracleServiceConflictWarning below, whose shape this mirrors.
export function NonSlugNameWarning({ uid, name }: { uid: string; name: string }) {
  return (
    <Tooltip delayDuration={100}>
      <TooltipTrigger asChild>
        <span
          data-testid={`database-name-warning-${uid}`}
          title="Not a valid slug"
          className="inline-flex cursor-help text-amber-600"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span className="sr-only">Name is not a slug</span>
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm space-y-1">
        <p className="font-medium">Not a valid slug</p>
        <p>
          "{name}" predates the naming rule and was grandfathered in. New
          servers must be lowercase letters, numbers, underscores or hyphens (a
          hyphen may not lead or trail) — this is the name every client types as
          the database name in its connection string, so anything else costs
          reachability on some protocol. Rename it from the pencil action on
          this row; every client config using the old name has to be updated to
          match.
        </p>
      </TooltipContent>
    </Tooltip>
  );
}

// ServerRenameField is the name input as it appears in the two *edit* forms —
// never in the create dialog, whose copy talks about choosing a name rather
// than moving one. The warning is the point of the component: on a database row
// the name is the connection target, so changing it invalidates every saved
// connection string, and that has to be said at the moment of the edit.
export function ServerRenameField({
  id,
  testId,
  value,
  originalName,
  isTunnel,
  onChange,
}: {
  id: string;
  testId: string;
  value: string;
  originalName: string;
  isTunnel: boolean;
  onChange: (next: string) => void;
}) {
  const changed = value !== originalName;

  return (
    <div className="space-y-2">
      <Label htmlFor={id}>Name</Label>
      <Input
        id={id}
        data-testid={testId}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        maxLength={63}
        pattern={SERVER_NAME_PATTERN_SOURCE}
        title="Lowercase letters, numbers, underscores and hyphens only (no leading/trailing hyphen, no spaces or dots)"
        required
      />
      <p className="text-xs text-muted-foreground">
        Lowercase letters, numbers, underscores and hyphens (not leading or
        trailing). Names are unique across every server, deleted ones included.
      </p>
      {changed && (
        <Alert data-testid="server-rename-warning">
          <AlertTriangle className="h-4 w-4" />
          <AlertDescription className="text-xs">
            {isTunnel ? (
              <>
                Renaming a tunnel row is safe: the servers that dial through it
                reference it by id, not by name. Only what you read in this UI
                changes.
              </>
            ) : (
              <>
                <strong>This moves the connection target.</strong> "
                {originalName}" is what every client sends as the database name
                — the Oracle SERVICE_NAME included — so every saved connection
                string, client config and script still using it stops resolving.
                Sessions already authenticated keep running; new connects must
                use "{value}".
              </>
            )}
          </AlertDescription>
        </Alert>
      )}
    </div>
  );
}

export type Protocol =
  | "postgresql"
  | "oracle"
  | "mysql"
  | "mariadb"
  | "mongodb"
  | "mssql"
  | "ssh"
  | "kubernetes";

export const PROTOCOL_LABEL: Record<Protocol, string> = {
  postgresql: "PostgreSQL",
  oracle: "Oracle",
  mysql: "MySQL",
  mariadb: "MariaDB",
  mongodb: "MongoDB",
  mssql: "SQL Server",
  ssh: "SSH Bastion",
  kubernetes: "Kubernetes cluster",
};

export const PROTOCOL_DEFAULT_PORT: Record<Protocol, string> = {
  postgresql: "5432",
  oracle: "1521",
  mysql: "3306",
  mariadb: "3306",
  mongodb: "27017",
  mssql: "1433",
  ssh: "22",
  kubernetes: "6443",
};

export const PROTOCOL_BADGE_CLASS: Record<Protocol, string> = {
  postgresql:
    "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400",
  oracle: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400",
  mysql: "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400",
  mariadb: "bg-teal-100 text-teal-700 dark:bg-teal-900/30 dark:text-teal-400",
  mongodb:
    "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400",
  mssql:
    "bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-400",
  ssh: "bg-slate-100 text-slate-700 dark:bg-slate-800/50 dark:text-slate-300",
  kubernetes:
    "bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-400",
};

export const PROTOCOL_USERNAME_PLACEHOLDER: Record<Protocol, string> = {
  postgresql: "postgres",
  oracle: "SYSTEM",
  mysql: "root",
  mariadb: "root",
  mongodb: "admin",
  mssql: "sa",
  ssh: "www-data",
  kubernetes: "dbbat",
};


// STAGE_LABEL turns a failed check's stage into the thing the admin should go
// look at. The stage — not the message — is what identifies the wrong field.
export const STAGE_LABEL: Record<string, string> = {
  config: "Configuration",
  bastion_dial: "Reaching the SSH bastion",
  bastion_auth: "SSH authentication",
  target_dial: "Reaching the database",
  target_auth: "Database authentication",
  cluster_api: "Reaching the Kubernetes API server",
  cluster_auth: "Kubernetes service account token",
  cluster_rbac: "Kubernetes RBAC (pods/portforward)",
  cluster_target: "Resolving the target pod",
};

export function describeTestResult(result: ConnectionTestResult): string {
  const stage = STAGE_LABEL[result.stage ?? ""] ?? result.stage ?? "";
  return stage ? `${stage}: ${result.message}` : (result.message ?? "");
}

// reportTestWarnings surfaces the advisory half of a check. It is reported
// whether the check passed or failed: a warning is about how the row sits next
// to its siblings, so a green check can perfectly well carry one.
export function reportTestWarnings(result: ConnectionTestResult) {
  for (const warning of result.warnings ?? []) {
    toast.warning(warning.message);
  }
}

// OracleServiceConflictWarning marks an Oracle row whose upstream service name
// is also claimed by rows pointing at a different host:port.
//
// The row itself is fine. What is broken is connecting with the *shared service
// name* rather than the dbbat server name: the proxy compares candidate
// upstreams as text, so a CNAME here and the A-record there read as two
// machines and the connect is refused ORA-12514. Showing it here is what makes
// that visible before a user hits it — the whole reason the textual compare can
// stay as it is.
export function OracleServiceConflictWarning({
  uid,
  conflict,
}: {
  uid: string;
  conflict: OracleServiceNameConflict;
}) {
  return (
    <Tooltip delayDuration={100}>
      <TooltipTrigger asChild>
        <span
          data-testid={`database-oracle-conflict-${uid}`}
          title={conflict.message}
          className="inline-flex cursor-help text-amber-600"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span className="sr-only">Conflicting Oracle service name</span>
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm space-y-1">
        <p className="font-medium">Shared Oracle service name</p>
        <p>{conflict.message}</p>
        <ul className="font-mono text-xs">
          {conflict.servers?.map((srv) => (
            <li key={srv.uid}>
              {srv.name} → {srv.host}:{srv.port}
            </li>
          ))}
        </ul>
      </TooltipContent>
    </Tooltip>
  );
}

// TestConnectionButton dials the server for real and reports the staged
// outcome. Rendered per row, so each button owns its own mutation state.
export function TestConnectionButton({
  uid,
  testId,
  canTest,
  disabledReason,
}: {
  uid: string;
  testId: string;
  canTest: boolean;
  disabledReason?: string;
}) {
  const testConnection = useTestServerConnection(uid);

  return (
    <PermissionButton
      data-testid={testId}
      variant="ghost"
      size="icon"
      disabled={!canTest || testConnection.isPending}
      disabledReason={disabledReason}
      enabledTooltip="Test this server's connectivity"
      onClick={(e) => {
        e.stopPropagation();
        testConnection.mutate(undefined, {
          onSuccess: (result) => {
            reportTestWarnings(result);
            if (result.ok) {
              toast.success(
                result.host_key_pinned
                  ? `${result.message} (host key pinned)`
                  : result.message,
              );
              return;
            }
            toast.error(describeTestResult(result));
          },
          onError: (error: Error) => toast.error(error.message),
        });
      }}
    >
      {testConnection.isPending ? (
        <Loader2 className="h-4 w-4 animate-spin" />
      ) : (
        <PlugZap className="h-4 w-4" />
      )}
    </PermissionButton>
  );
}

