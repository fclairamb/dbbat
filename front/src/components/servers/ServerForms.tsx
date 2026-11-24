import {
  useState,
} from "react";
import {
  useNavigate,
} from "@tanstack/react-router";
import {
  FormPage,
  FormSection,
} from "@/components/shared/FormPage";
import {
  type DatabaseItem,
  isFullDatabase,
  PROTOCOL_LABEL,
  PROTOCOL_DEFAULT_PORT,
  PROTOCOL_USERNAME_PLACEHOLDER,
  type Protocol,
  ServerRenameField,
  describeTestResult,
  reportTestWarnings,
  SERVER_NAME_PATTERN_SOURCE,
} from "@/components/servers/serverShared";
import {
  useDatabaseConnection,
  useCreateDatabase,
  useUpdateDatabase,
  useInstance,
  useDatabase as useDatabaseDetail,
  useTunnelServers,
  useServerReferences,
  type Database,
  type ServerReferences,
  type UpdateDatabaseRequest,
} from "@/api";
import {
  RetentionFields,
  formatSeconds,
  type RetentionSeconds,
} from "@/components/shared/RetentionDuration";
import {
  Button,
} from "@/components/ui/button";
import {
  Input,
} from "@/components/ui/input";
import {
  Label,
} from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Switch,
} from "@/components/ui/switch";
import {
  Checkbox,
} from "@/components/ui/checkbox";
import {
  AlertCircle,
  AlertTriangle,
} from "lucide-react";
import {
  toast,
} from "sonner";
import {
  CopyableField,
} from "@/components/shared/CopyableField";
import {
  ApproverGroupPickers,
} from "@/components/shared/ApproverGroupPickers";
import {
  Alert,
  AlertDescription,
} from "@/components/ui/alert";


/** Add-server form (the `new` route). */
export function CreateServerForm() {
  const navigate = useNavigate();
  const onClose = () => navigate({ to: "/servers" });
  const [protocol, setProtocol] = useState<Protocol>("postgresql");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [host, setHost] = useState("");
  const [port, setPort] = useState(PROTOCOL_DEFAULT_PORT.postgresql);
  const [databaseName, setDatabaseName] = useState("");
  const [oracleServiceName, setOracleServiceName] = useState("");
  const [mongoAuthSource, setMongoAuthSource] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [sslMode, setSslMode] = useState("prefer");
  const [listable, setListable] = useState(true);
  const [viaUid, setViaUid] = useState<string>("");
  const [sshPrivateKey, setSshPrivateKey] = useState("");
  const [sshPassphrase, setSshPassphrase] = useState("");
  const [k8sCaCert, setK8sCaCert] = useState("");
  const [k8sNamespace, setK8sNamespace] = useState("");
  const [k8sInsecure, setK8sInsecure] = useState(false);
  const [accessApproverUids, setAccessApproverUids] = useState<string[]>([]);
  const [queryApproverUids, setQueryApproverUids] = useState<string[]>([]);

  const isSSH = protocol === "ssh";
  const isKubernetes = protocol === "kubernetes";
  // Both are dial paths rather than database targets, so they share every
  // "this row has no database behind it" branch below.
  const isTunnel = isSSH || isKubernetes;
  const { data: tunnelServers } = useTunnelServers();
  // A cluster row may itself sit behind an SSH bastion; the reverse nesting is
  // unsupported, so a cluster is never offered as a cluster's own via.
  const viaOptions = (tunnelServers ?? []).filter((srv) =>
    isKubernetes ? srv.protocol === "ssh" : true,
  );

  const createDb = useCreateDatabase({
    onSuccess: () => {
      toast.success(
        isKubernetes
          ? "Kubernetes cluster created"
          : isSSH
            ? "SSH server created"
            : "Database created successfully",
      );
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (isKubernetes) {
      createDb.mutate({
        name,
        description: description || undefined,
        host,
        port: parseInt(port, 10),
        username,
        // The service account bearer token rides in the password field: it is
        // this row's one secret, encrypted exactly like a database password.
        password,
        protocol,
        ssl_mode: "",
        listable: false,
        k8s_ca_cert: k8sCaCert || undefined,
        k8s_namespace: k8sNamespace,
        k8s_insecure_skip_tls_verify: k8sInsecure || undefined,
        via_uid: viaUid || undefined,
      });
      return;
    }
    if (isSSH) {
      createDb.mutate({
        name,
        description: description || undefined,
        host,
        port: parseInt(port, 10),
        username,
        password: password || undefined,
        protocol,
        ssl_mode: "",
        listable: false,
        ssh_private_key: sshPrivateKey || undefined,
        ssh_passphrase: sshPassphrase || undefined,
      });
      return;
    }
    createDb.mutate({
      name,
      description: description || undefined,
      host,
      port: parseInt(port, 10),
      database_name: protocol === "oracle" ? oracleServiceName : databaseName,
      username,
      password,
      ssl_mode: protocol === "oracle" ? "" : sslMode,
      protocol,
      oracle_service_name:
        protocol === "oracle" ? oracleServiceName : undefined,
      mongo_auth_source:
        protocol === "mongodb" && mongoAuthSource ? mongoAuthSource : undefined,
      listable,
      via_uid: viaUid || undefined,
      access_approver_user_group_uids: accessApproverUids,
      query_approver_user_group_uids: queryApproverUids,
    });
  };

  return (
    <FormPage
      testId="create-server-page"
      title={<>Add Server</>}
      description={<>Configure a new target database connection.</>}
      backTo="/servers"
      backLabel="Servers"
      onCancel={onClose}
      onSubmit={handleSubmit}
      submitLabel="Create"
      submitTestId="database-create-submit"
      submitDisabled={createDb.isPending}
    >
      <div className="max-w-2xl space-y-4">
          <div className="space-y-2">
            <Label htmlFor="protocol">Protocol</Label>
            <Select
              value={protocol}
              onValueChange={(val) => {
                const next = val as Protocol;
                setProtocol(next);
                // Auto-cycle the port when the user hasn't customised it away
                // from one of the conventional defaults.
                if (Object.values(PROTOCOL_DEFAULT_PORT).includes(port)) {
                  setPort(PROTOCOL_DEFAULT_PORT[next]);
                }
              }}
            >
              <SelectTrigger data-testid="protocol-select">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="postgresql">PostgreSQL</SelectItem>
                <SelectItem value="oracle" data-testid="protocol-option-oracle">
                  Oracle
                </SelectItem>
                <SelectItem value="mysql">MySQL</SelectItem>
                <SelectItem value="mariadb">MariaDB</SelectItem>
                <SelectItem value="mongodb">MongoDB</SelectItem>
                <SelectItem value="mssql">SQL Server</SelectItem>
                <SelectItem value="ssh" data-testid="protocol-option-ssh">
                  SSH Bastion
                </SelectItem>
                <SelectItem
                  value="kubernetes"
                  data-testid="protocol-option-kubernetes"
                >
                  Kubernetes cluster
                </SelectItem>
              </SelectContent>
            </Select>
            {isSSH && (
              <p className="text-xs text-muted-foreground">
                An SSH bastion is a dial path, not a database. Other databases
                can be reached "via" it. It never appears in access-request
                lists.
              </p>
            )}
            {isKubernetes && (
              <p className="text-xs text-muted-foreground">
                A Kubernetes cluster is a dial path, not a database. Databases
                reached "via" it must <strong>run as pods</strong> in the
                namespace below: a port-forward reaches a pod's own port, so a
                database merely routable from the cluster network (an RDS in the
                same VPC) is out of scope.
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="name">Name</Label>
            <Input
              id="name"
              data-testid="database-name-input"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="production_db"
              maxLength={63}
              pattern={SERVER_NAME_PATTERN_SOURCE}
              title="Lowercase letters, numbers, underscores and hyphens only (no leading/trailing hyphen, no spaces or dots)"
              required
            />
            <p className="text-xs text-muted-foreground">
              This is the client-facing selector every protocol uses — the
              "database name" typed in a connection string — so it must be a
              slug: lowercase letters, numbers, underscores and hyphens (not
              leading or trailing).
            </p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="description">Description</Label>
            <Input
              id="description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Production database"
            />
          </div>
          {!isTunnel && (
            <div className="flex items-center justify-between rounded-lg border p-3">
              <div className="space-y-0.5">
                <Label htmlFor="listable">Listable</Label>
                <p className="text-sm text-muted-foreground">
                  Show in the access-request dropdown for non-admin users
                </p>
              </div>
              <Switch
                id="listable"
                checked={listable}
                onCheckedChange={setListable}
              />
            </div>
          )}
          {/* Tunnel rows are dial paths — nothing is ever granted or held on
              them, so neither approver kind applies. */}
          {!isTunnel && (
            <ApproverGroupPickers
              scope="server"
              accessSelected={accessApproverUids}
              onAccessChange={setAccessApproverUids}
              querySelected={queryApproverUids}
              onQueryChange={setQueryApproverUids}
              testIdPrefix="database"
            />
          )}
          <div className="grid grid-cols-3 gap-2">
            <div className="col-span-2 space-y-2">
              <Label htmlFor="host">
                {isKubernetes ? "API server" : "Host"}
              </Label>
              <Input
                id="host"
                value={host}
                onChange={(e) => setHost(e.target.value)}
                placeholder={
                  isKubernetes ? "https://api.cluster.example.com" : "localhost"
                }
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="port">Port</Label>
              <Input
                id="port"
                type="number"
                value={port}
                onChange={(e) => setPort(e.target.value)}
                required
              />
            </div>
          </div>
          {!isTunnel && protocol !== "oracle" && (
            <div className="space-y-2">
              <Label htmlFor="databaseName">Database Name</Label>
              <Input
                id="databaseName"
                value={databaseName}
                onChange={(e) => setDatabaseName(e.target.value)}
                placeholder={
                  protocol === "mysql" ||
                  protocol === "mariadb" ||
                  protocol === "mongodb"
                    ? "mydb"
                    : "myapp"
                }
                required
              />
            </div>
          )}
          {protocol === "oracle" && (
            <div className="space-y-2">
              <Label htmlFor="oracleServiceName">Service Name</Label>
              <Input
                id="oracleServiceName"
                value={oracleServiceName}
                onChange={(e) => setOracleServiceName(e.target.value)}
                placeholder="ORCL"
                required
              />
            </div>
          )}
          {protocol === "mongodb" && (
            <div className="space-y-2">
              <Label htmlFor="mongoAuthSource">Auth Source</Label>
              <Input
                id="mongoAuthSource"
                value={mongoAuthSource}
                onChange={(e) => setMongoAuthSource(e.target.value)}
                placeholder="admin"
              />
              <p className="text-xs text-muted-foreground">
                Upstream MongoDB database where the proxy user's credentials
                live. Defaults to <code className="font-mono">admin</code>.
              </p>
            </div>
          )}
          <div className="space-y-2">
            <Label htmlFor="username">
              {isKubernetes ? "Service account name" : "Username"}
            </Label>
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder={PROTOCOL_USERNAME_PLACEHOLDER[protocol]}
              required
            />
            {isKubernetes && (
              <p className="text-xs text-muted-foreground">
                Informational: it names the account in the UI and the logs. The
                credential is the token below.
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="password">
              {isKubernetes
                ? "Service account token"
                : `Password${isSSH ? " (optional if using a key)" : ""}`}
            </Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required={!isSSH}
            />
            {isKubernetes && (
              <p className="text-xs text-muted-foreground">
                A long-lived ServiceAccount bearer token. Kubeconfig files are
                not accepted: managed clusters authenticate through exec
                credential plugins, which a server daemon cannot run.
              </p>
            )}
          </div>
          {isSSH && (
            <>
              <div className="space-y-2">
                <Label htmlFor="sshPrivateKey">SSH Private Key (PEM)</Label>
                <textarea
                  id="sshPrivateKey"
                  className="flex min-h-[96px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                  value={sshPrivateKey}
                  onChange={(e) => setSshPrivateKey(e.target.value)}
                  placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                />
                <p className="text-xs text-muted-foreground">
                  Write-only: the stored key is never shown again.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="sshPassphrase">Key Passphrase (optional)</Label>
                <Input
                  id="sshPassphrase"
                  type="password"
                  value={sshPassphrase}
                  onChange={(e) => setSshPassphrase(e.target.value)}
                />
              </div>
            </>
          )}
          {isKubernetes && (
            <>
              <div className="space-y-2">
                <Label htmlFor="k8sNamespace">Namespace</Label>
                <Input
                  id="k8sNamespace"
                  data-testid="k8s-namespace-input"
                  value={k8sNamespace}
                  onChange={(e) => setK8sNamespace(e.target.value)}
                  placeholder="data"
                  required
                />
                <p className="text-xs text-muted-foreground">
                  Every pod lookup and every port-forward is scoped to it: it is
                  the namespace the Role and RoleBinding cover.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="k8sCaCert">
                  CA certificate (PEM){" "}
                  <span className="font-normal text-muted-foreground">
                    — optional
                  </span>
                </Label>
                <textarea
                  id="k8sCaCert"
                  data-testid="k8s-ca-cert-input"
                  className="flex min-h-[96px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                  value={k8sCaCert}
                  onChange={(e) => setK8sCaCert(e.target.value)}
                  placeholder="-----BEGIN CERTIFICATE-----"
                />
                {/* Optional, but the recommendation: a pasted bundle is a CA
                    you verified, where a pin is only a CA we happened to meet
                    first. Say so at the point of choosing. */}
                <p className="text-xs text-muted-foreground">
                  From the token Secret&apos;s{" "}
                  <code className="font-mono">ca.crt</code> key. Leave it blank
                  and dbbat pins whatever the API server presents on the first
                  connect, then refuses anything else — safer than skipping
                  verification, weaker than a bundle you checked yourself.
                </p>
              </div>
              <div className="flex items-center justify-between rounded-lg border p-3">
                <div className="space-y-0.5">
                  <Label htmlFor="k8sInsecure">Skip TLS verification</Label>
                  <p className="text-sm text-muted-foreground">
                    Throwaway clusters only: anything that can intercept the API
                    server connection can then read the token.
                  </p>
                </div>
                <Switch
                  id="k8sInsecure"
                  checked={k8sInsecure}
                  onCheckedChange={setK8sInsecure}
                />
              </div>
            </>
          )}
          {!isTunnel && protocol !== "oracle" && (
            <div className="space-y-2">
              <Label htmlFor="sslMode">SSL Mode</Label>
              <Select value={sslMode} onValueChange={setSslMode}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="disable">Disable</SelectItem>
                  <SelectItem value="prefer">Prefer</SelectItem>
                  <SelectItem value="require">Require</SelectItem>
                  <SelectItem value="verify-ca">Verify CA</SelectItem>
                  <SelectItem value="verify-full">Verify Full</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}
          {!isSSH && (
            <div className="space-y-2">
              <Label htmlFor="viaUid">
                {isKubernetes ? "Via SSH bastion" : "Via tunnel server"}
              </Label>
              <Select
                value={viaUid || "none"}
                onValueChange={(v) => setViaUid(v === "none" ? "" : v)}
              >
                <SelectTrigger data-testid="via-select">
                  <SelectValue placeholder="Direct (no tunnel)" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">Direct (no tunnel)</SelectItem>
                  {viaOptions.map((srv) => (
                    <SelectItem key={srv.uid} value={srv.uid}>
                      {srv.name} (
                      {PROTOCOL_LABEL[srv.protocol as Protocol] ?? srv.protocol}
                      )
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                {isKubernetes
                  ? "Only needed when the API server itself is reachable only through a jump host."
                  : 'Dial this database through an SSH bastion or a Kubernetes cluster. Through a cluster, Host is a pod name or "svc/<name>" in that cluster\'s namespace and Port is the container port.'}
              </p>
            </div>
          )}
      </div>
    </FormPage>
  );
}


/**
 * Read-only view of a server, for every role: the connection URL card is for
 * connectors, so non-admins see this section and nothing else.
 */
export function ServerDetailsSection({ db }: { db: DatabaseItem }) {
  const { data: connInfo, error: connError } = useDatabaseConnection(db.uid);

  const isProxyDisabled =
    connError &&
    "status" in connError &&
    (connError as { status?: number }).status === 409;
  const noGrant =
    connError &&
    "status" in connError &&
    (connError as { status?: number }).status === 404;

  return (
    <section
      className="rounded-lg border bg-card p-6"
      data-testid="database-details-section"
    >
      <div className="max-w-2xl space-y-4">
        <div>
          <h2 className="text-lg font-semibold">{db.name}</h2>
          {db.description && (
            <p className="text-sm text-muted-foreground">{db.description}</p>
          )}
        </div>
          {db && isFullDatabase(db) && (
            <div className="space-y-2 text-sm">
              <div className="grid grid-cols-3 gap-1">
                <span className="text-muted-foreground">Protocol</span>
                <span className="col-span-2 font-medium">
                  {PROTOCOL_LABEL[(db as Database).protocol as Protocol] ??
                    (db as Database).protocol}
                </span>
              </div>
              <div className="grid grid-cols-3 gap-1">
                <span className="text-muted-foreground">Target</span>
                <span className="col-span-2 font-mono">
                  {(db as Database).host}:{(db as Database).port} /{" "}
                  {(db as Database).database_name}
                </span>
              </div>
              {(db as Database).protocol !== "oracle" && (
                <div className="grid grid-cols-3 gap-1">
                  <span className="text-muted-foreground">SSL mode</span>
                  <span className="col-span-2">
                    {(db as Database).ssl_mode ?? "-"}
                  </span>
                </div>
              )}
            </div>
          )}

          {!noGrant && (
            <div className="space-y-2">
              <h3 className="text-sm font-medium">Connection URL</h3>
              <p className="text-xs text-muted-foreground">
                Replace{" "}
                <code className="rounded bg-muted px-1 py-0.5 font-mono">
                  {"{DBBAT_KEY}"}
                </code>{" "}
                with one of your dbbat API keys (the{" "}
                <code className="font-mono">dbb_…</code> token).
              </p>
              {db &&
                isFullDatabase(db) &&
                (db as Database).protocol !== "oracle" &&
                (db as Database).protocol !== "mongodb" && (
                  <p
                    className="text-xs text-muted-foreground"
                    data-testid="database-connection-url-hint"
                  >
                    Database is the real upstream name; the{" "}
                    <code className="font-mono">#{(db as Database).name}</code>{" "}
                    suffix selects the dbbat server.
                  </p>
                )}
              {isProxyDisabled ? (
                <Alert>
                  <AlertCircle className="h-4 w-4" />
                  <AlertDescription>
                    The proxy for this protocol is currently disabled.
                  </AlertDescription>
                </Alert>
              ) : connInfo ? (
                <CopyableField
                  value={connInfo.url}
                  testId="database-connection-url"
                  toastMessage="Connection URL copied"
                />
              ) : null}
            </div>
          )}
      </div>
    </section>
  );
}

/**
 * Editing the two approver lists on one server.
 *
 * Kept out of the create dialog's edit counterpart because there isn't one:
 * databases are otherwise edited nowhere in this UI, and who may approve for a
 * server is exactly the field an operator revisits after the row was created —
 * on a rotation change, a team split, a departure.
 */
export function EditServerApproversForm({
  server,
  onClose,
}: {
  server: Database;
  onClose: () => void;
}) {
  const [accessApproverUids, setAccessApproverUids] = useState<string[]>(
    server.access_approver_user_group_uids ?? [],
  );
  const [queryApproverUids, setQueryApproverUids] = useState<string[]>(
    server.query_approver_user_group_uids ?? [],
  );

  const updateServer = useUpdateDatabase(server.uid, {
    onSuccess: () => {
      toast.success("Approvers updated");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    // Both sent unconditionally, empty included: clearing a list is a real
    // policy change (the decision falls back to the server groups, then to
    // admins), so it has to be expressible from this form.
    updateServer.mutate({
      access_approver_user_group_uids: accessApproverUids,
      query_approver_user_group_uids: queryApproverUids,
    });
  };

  return (
    <FormSection
      testId="server-approvers-section"
      title={<>Approvers for {server.name}</>}
      description={<>Who, besides admins, may decide access requests and release approval
            holds on this server.</>}
      onSubmit={handleSubmit}
      submitLabel="Save"
      submitTestId="server-approvers-submit"
      submitDisabled={updateServer.isPending}
    >
      <div className="max-w-2xl space-y-4">
          <ApproverGroupPickers
            scope="server"
            accessSelected={accessApproverUids}
            onAccessChange={setAccessApproverUids}
            querySelected={queryApproverUids}
            onQueryChange={setQueryApproverUids}
            testIdPrefix="server-approvers"
          />
      </div>
    </FormSection>
  );
}


export function RenameServerForm({
  server,
  onClose,
}: {
  server: DatabaseItem;
  onClose: () => void;
}) {
  const [name, setName] = useState(server.name);

  const renameServer = useUpdateDatabase(server.uid, {
    onSuccess: () => {
      toast.success(`Renamed to "${name}"`);
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    renameServer.mutate({ name });
  };

  return (
    <FormSection
      testId="database-rename-section"
      title={<>Rename server</>}
      description={<>"{server.name}" keeps its grants, history and query chains — only
            the name clients connect with changes.</>}
      onSubmit={handleSubmit}
      submitLabel="Rename"
      submitTestId="database-rename-submit"
      submitDisabled={renameServer.isPending || name === server.name}
    >
      <div className="max-w-2xl space-y-4">
          <ServerRenameField
            id="rename-server-name"
            testId="database-rename-input"
            value={name}
            originalName={server.name}
            isTunnel={false}
            onChange={setName}
          />
      </div>
    </FormSection>
  );
}


/**
 * TargetMoveWarning is the blast radius of re-pointing a row.
 *
 * Editing host, port or the database/service name does not re-issue anything:
 * every grant already covering the row keeps covering it and simply reaches
 * whatever was typed, on the next connect, with no approval in between. That
 * is the same trade the project already took for live server-group membership,
 * and the answer is the same — say so at the point of edit rather than making
 * the row immutable.
 */
function TargetMoveWarning({
  references,
  isLoading,
}: {
  references?: ServerReferences;
  isLoading: boolean;
}) {
  return (
    <Alert data-testid="database-edit-target-warning">
      <AlertTriangle className="h-4 w-4" />
      <AlertDescription className="text-xs space-y-1">
        <p>
          <strong>This moves where the row points.</strong> Nothing is
          re-issued: every grant already covering this server reaches the new
          target on the next connect. Sessions already open stay on the upstream
          they dialed.
        </p>
        {isLoading ? (
          <p className="text-muted-foreground">Counting what follows…</p>
        ) : (
          references && (
            <p data-testid="database-edit-target-counts">
              {references.active_grants} active{" "}
              {references.active_grants === 1 ? "grant" : "grants"} and{" "}
              {references.server_groups}{" "}
              {references.server_groups === 1
                ? "server group"
                : "server groups"}{" "}
              reference this row.
            </p>
          )
        )}
      </AlertDescription>
    </Alert>
  );
}

/**
 * Editing one database row's connection details.
 *
 * The counterpart of EditSSHServerForm, which tunnel rows have had all along.
 * Every field here is a *correction* — a host that moved, a port that changed,
 * a password that rotated — and correcting one used to mean either an UPDATE
 * against the storage database or the far worse workaround, delete and
 * re-create, which drops the grants, the session ledger and the query chains
 * hanging off the row.
 *
 * Two fields are deliberately absent:
 *
 *   - `name`, which lives in RenameServerDialog: it changes what every client
 *     types, so it carries a warning of its own rather than sharing this one.
 *   - `protocol`, which is the single edit that genuinely makes the row a
 *     *different server*. The API refuses it with a 409 once any grant or
 *     connection references the row, so this is not the UI hiding something
 *     the API allows — it is the same invariant, stated twice.
 */
export function EditDatabaseForm({
  server,
  onClose,
}: {
  server: Database;
  onClose: () => void;
}) {
  const isOracle = server.protocol === "oracle";
  const isMongo = server.protocol === "mongodb";

  // The one name that identifies the upstream schema for this protocol. Oracle
  // keeps it in oracle_service_name; every other protocol in database_name.
  const originalTargetName =
    (isOracle ? server.oracle_service_name : server.database_name) || "";

  const [description, setDescription] = useState(server.description || "");
  const [host, setHost] = useState(server.host || "");
  const [port, setPort] = useState(String(server.port ?? ""));
  const [targetName, setTargetName] = useState(originalTargetName);
  const [mongoAuthSource, setMongoAuthSource] = useState(
    server.mongo_auth_source || "",
  );
  const [username, setUsername] = useState(server.username || "");
  const [password, setPassword] = useState("");
  const [sslMode, setSslMode] = useState(server.ssl_mode || "prefer");
  const [listable, setListable] = useState(server.listable ?? true);
  const [viaUid, setViaUid] = useState(server.via_uid || "");
  const [testAfterSave, setTestAfterSave] = useState(false);
  const { data: instance } = useInstance();
  const { data: detail } = useDatabaseDetail(server.uid);
  const [archiveAfter, setArchiveAfter] = useState<RetentionSeconds>(
    server.archive_after_seconds ?? null,
  );
  const [deleteAfter, setDeleteAfter] = useState<RetentionSeconds>(
    server.delete_after_seconds ?? null,
  );
  const eff =
    detail && "effective_retention" in detail
      ? detail.effective_retention
      : undefined;
  const inheritNote = eff
    ? `Inherited today: archive ${formatSeconds(eff.archive.seconds)} (${eff.archive.source || "unset"}), delete ${formatSeconds(eff.delete.seconds)} (${eff.delete.source || "unset"}).`
    : undefined;

  const { data: tunnelServers } = useTunnelServers();

  // Only host/port/target-name move the *target*; a description or a listable
  // toggle moves nothing, so they raise no warning.
  const targetMoved =
    host !== (server.host || "") ||
    port !== String(server.port ?? "") ||
    targetName !== originalTargetName;

  // Fetched only once the form says something moved: an admin opening the
  // dialog to fix a typo in the description should not cost a query.
  const { data: references, isLoading: referencesLoading } =
    useServerReferences(server.uid, targetMoved);

  const updateServer = useUpdateDatabase(server.uid, {
    onSuccess: (test) => {
      toast.success(`"${server.name}" updated`);

      if (test) {
        reportTestWarnings(test);

        if (test.ok) {
          toast.success(test.message ?? "Connection OK");
        } else {
          toast.error(describeTestResult(test));
        }
      }

      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    // Only what actually changed goes on the wire, so the `database.updated`
    // audit entry reads as the edit that was made rather than a dump of every
    // field the form happens to render.
    const update: UpdateDatabaseRequest = { test_connection: testAfterSave };

    if (description !== (server.description || "")) {
      update.description = description;
    }
    if (host !== (server.host || "")) {
      update.host = host;
    }
    if (port !== String(server.port ?? "")) {
      update.port = parseInt(port, 10);
    }
    if (targetName !== originalTargetName) {
      // Oracle carries the same value in both columns — the create dialog
      // writes them together, so an edit keeps them in step.
      update.database_name = targetName;
      if (isOracle) {
        update.oracle_service_name = targetName;
      }
    }
    if (isMongo && mongoAuthSource !== (server.mongo_auth_source || "")) {
      update.mongo_auth_source = mongoAuthSource;
    }
    if (username !== (server.username || "")) {
      update.username = username;
    }
    // Blank means "keep the stored password", exactly like the SSH secrets.
    if (password) {
      update.password = password;
    }
    if (!isOracle && sslMode !== (server.ssl_mode || "prefer")) {
      update.ssl_mode = sslMode;
    }
    if (listable !== (server.listable ?? true)) {
      update.listable = listable;
    }
    if (viaUid !== (server.via_uid || "")) {
      if (viaUid) {
        update.via_uid = viaUid;
      } else {
        // Distinct from an omitted via_uid, which would leave the tunnel alone.
        update.clear_via_uid = true;
      }
    }

    if (archiveAfter !== (server.archive_after_seconds ?? null)) {
      update.archive_after_seconds = archiveAfter;
    }
    if (deleteAfter !== (server.delete_after_seconds ?? null)) {
      update.delete_after_seconds = deleteAfter;
    }

    updateServer.mutate(update);
  };

  return (
    <FormSection
      testId="database-edit-section"
      title={<>Edit server</>}
      description={<>Correct "{server.name}"'s connection details. Its grants, history
            and query chains stay with it. The name is changed from the rename
            dialog, and the protocol cannot change once the row has been used.</>}
      onSubmit={handleSubmit}
      submitLabel="Save"
      submitTestId="database-edit-submit"
      submitDisabled={updateServer.isPending}
    >
      <div className="max-w-2xl space-y-4">
          <div className="space-y-2">
            <Label htmlFor="edit-db-description">Description</Label>
            <Input
              id="edit-db-description"
              data-testid="database-edit-description-input"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </div>
          <div className="grid grid-cols-3 gap-2">
            <div className="col-span-2 space-y-2">
              <Label htmlFor="edit-db-host">Host</Label>
              <Input
                id="edit-db-host"
                data-testid="database-edit-host-input"
                value={host}
                onChange={(e) => setHost(e.target.value)}
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="edit-db-port">Port</Label>
              <Input
                id="edit-db-port"
                data-testid="database-edit-port-input"
                type="number"
                value={port}
                onChange={(e) => setPort(e.target.value)}
                required
              />
            </div>
          </div>
          <div className="space-y-2">
            <Label htmlFor="edit-db-target">
              {isOracle ? "Service Name" : "Database Name"}
            </Label>
            <Input
              id="edit-db-target"
              data-testid="database-edit-target-input"
              value={targetName}
              onChange={(e) => setTargetName(e.target.value)}
              required
            />
            <p className="text-xs text-muted-foreground">
              The name on the <em>upstream</em> server. What clients type is
              this row&apos;s name ("{server.name}"), which the rename dialog
              owns.
            </p>
          </div>
          {targetMoved && (
            <TargetMoveWarning
              references={references}
              isLoading={referencesLoading}
            />
          )}
          {isMongo && (
            <div className="space-y-2">
              <Label htmlFor="edit-db-mongo-auth-source">Auth Source</Label>
              <Input
                id="edit-db-mongo-auth-source"
                data-testid="database-edit-mongo-auth-source-input"
                value={mongoAuthSource}
                onChange={(e) => setMongoAuthSource(e.target.value)}
                placeholder="admin"
              />
              <p className="text-xs text-muted-foreground">
                Upstream MongoDB database where the proxy user&apos;s
                credentials live. Blank defaults to{" "}
                <code className="font-mono">admin</code>.
              </p>
            </div>
          )}
          <div className="space-y-2">
            <Label htmlFor="edit-db-username">Username</Label>
            <Input
              id="edit-db-username"
              data-testid="database-edit-username-input"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="edit-db-password">
              Password (leave blank to keep unchanged)
            </Label>
            <Input
              id="edit-db-password"
              data-testid="database-edit-password-input"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>
          {!isOracle && (
            <div className="space-y-2">
              <Label htmlFor="edit-db-ssl-mode">SSL Mode</Label>
              <Select value={sslMode} onValueChange={setSslMode}>
                <SelectTrigger data-testid="database-edit-ssl-mode-select">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="disable">Disable</SelectItem>
                  <SelectItem value="prefer">Prefer</SelectItem>
                  <SelectItem value="require">Require</SelectItem>
                  <SelectItem value="verify-ca">Verify CA</SelectItem>
                  <SelectItem value="verify-full">Verify Full</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}
          <div className="space-y-2">
            <Label htmlFor="edit-db-via">Via tunnel server</Label>
            <Select
              value={viaUid || "none"}
              onValueChange={(v) => setViaUid(v === "none" ? "" : v)}
            >
              <SelectTrigger data-testid="database-edit-via-select">
                <SelectValue placeholder="Direct (no tunnel)" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none">Direct (no tunnel)</SelectItem>
                {(tunnelServers ?? []).map((srv) => (
                  <SelectItem key={srv.uid} value={srv.uid}>
                    {srv.name} (
                    {PROTOCOL_LABEL[srv.protocol as Protocol] ?? srv.protocol})
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="flex items-center justify-between rounded-lg border p-3">
            <div className="space-y-0.5">
              <Label htmlFor="edit-db-listable">Listable</Label>
              <p className="text-sm text-muted-foreground">
                Show in the access-request dropdown for non-admin users
              </p>
            </div>
            <Switch
              id="edit-db-listable"
              data-testid="database-edit-listable-switch"
              checked={listable}
              onCheckedChange={setListable}
            />
          </div>
          <RetentionFields
            archive={archiveAfter}
            onArchiveChange={setArchiveAfter}
            del={deleteAfter}
            onDeleteChange={setDeleteAfter}
            originalDelete={server.delete_after_seconds}
            archiveEnabled={instance?.resolved_retention?.archive_enabled ?? false}
            testIdPrefix="database-edit"
            inheritNote={inheritNote}
          />
          {/* The check runs against the row *as saved*, which is the only
              version worth checking — hence a flag on the PUT rather than a
              second call the admin has to remember to make. */}
          <div className="flex items-center gap-2 rounded-lg border p-3">
            <Checkbox
              id="edit-db-test-after-save"
              data-testid="database-edit-test-after-save"
              checked={testAfterSave}
              onCheckedChange={(checked) => setTestAfterSave(checked === true)}
            />
            <Label
              htmlFor="edit-db-test-after-save"
              className="text-sm font-normal"
            >
              Test the connection after saving
            </Label>
          </div>
      </div>
    </FormSection>
  );
}

export function EditSSHServerForm({
  server,
  onClose,
}: {
  server: Database;
  onClose: () => void;
}) {
  const [name, setName] = useState(server.name);
  const [description, setDescription] = useState(server.description || "");
  const [host, setHost] = useState(server.host || "");
  const [port, setPort] = useState(String(server.port ?? ""));
  const [username, setUsername] = useState(server.username || "");
  const [password, setPassword] = useState("");
  const [sshPrivateKey, setSshPrivateKey] = useState("");
  const [sshPassphrase, setSshPassphrase] = useState("");
  const [k8sCaCert, setK8sCaCert] = useState(server.k8s_ca_cert || "");
  const [k8sNamespace, setK8sNamespace] = useState(server.k8s_namespace || "");
  const [k8sInsecure, setK8sInsecure] = useState(
    server.k8s_insecure_skip_tls_verify ?? false,
  );
  const [viaUid, setViaUid] = useState(server.via_uid || "");
  // Read-only: the learned pin is the dialer's to write, never a form field.
  const learnedCaCert = server.k8s_learned_ca_cert || "";
  const [resetLearnedCa, setResetLearnedCa] = useState(false);

  // One form for both dial-path kinds: they share host/port/credential and
  // differ only in the extra material they carry.
  const isKubernetes = server.protocol === "kubernetes";
  const kindLabel = isKubernetes ? "Kubernetes cluster" : "SSH server";

  const { data: tunnelServers } = useTunnelServers(isKubernetes);
  // Same filter the create dialog applies: only an SSH bastion can sit in
  // front of a cluster (a bastion reached through a port-forward is not wired
  // up in the dialer), and a cluster is never dialed through itself.
  const viaOptions = (tunnelServers ?? []).filter(
    (srv) => srv.protocol === "ssh" && srv.uid !== server.uid,
  );

  const updateServer = useUpdateDatabase(server.uid, {
    onSuccess: () => {
      toast.success(`${kindLabel} updated successfully`);
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    // Only sent when actually changed: a PUT that repeats the current name
    // would still be a rename as far as the uniqueness check is concerned, and
    // it keeps the audit entry free of a "renamed to itself" line.
    const renamed = name !== server.name ? name : undefined;
    if (isKubernetes) {
      // Only sent when actually changed, so the `database.updated` audit entry
      // stays a list of real edits. An omitted via_uid leaves the bastion
      // alone; clearing it is its own flag.
      const via: Pick<UpdateDatabaseRequest, "via_uid" | "clear_via_uid"> = {};
      if (viaUid !== (server.via_uid || "")) {
        if (viaUid) {
          via.via_uid = viaUid;
        } else {
          via.clear_via_uid = true;
        }
      }
      updateServer.mutate({
        name: renamed,
        description: description || undefined,
        host,
        port: parseInt(port, 10),
        username,
        // Blank means "keep the stored token", exactly like the SSH secrets.
        password: password || undefined,
        ...via,
        // Sent unconditionally, unlike the CA: the whole point is that the
        // flag can be turned back off from here.
        k8s_ca_cert: k8sCaCert,
        k8s_namespace: k8sNamespace || undefined,
        k8s_insecure_skip_tls_verify: k8sInsecure,
        // Only ever sent when explicitly asked for: forgetting a pin is a
        // decision, not a side effect of opening the dialog.
        k8s_reset_learned_ca_cert: resetLearnedCa || undefined,
      });
      return;
    }
    updateServer.mutate({
      name: renamed,
      description: description || undefined,
      host,
      port: parseInt(port, 10),
      username,
      password: password || undefined,
      ssh_private_key: sshPrivateKey || undefined,
      ssh_passphrase: sshPassphrase || undefined,
    });
  };

  return (
    <FormSection
      testId="ssh-server-edit-section"
      title={<>Edit {kindLabel}</>}
      description={<>Update "{server.name}"'s connection details.</>}
      onSubmit={handleSubmit}
      submitLabel="Save"
      submitTestId="ssh-server-edit-submit"
      submitDisabled={updateServer.isPending}
    >
      <div className="max-w-2xl space-y-4">
          <ServerRenameField
            id="edit-ssh-name"
            testId="ssh-server-edit-name-input"
            value={name}
            originalName={server.name}
            isTunnel
            onChange={setName}
          />
          <div className="space-y-2">
            <Label htmlFor="edit-ssh-description">Description</Label>
            <Input
              id="edit-ssh-description"
              data-testid="ssh-server-edit-description-input"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </div>
          <div className="grid grid-cols-3 gap-2">
            <div className="col-span-2 space-y-2">
              <Label htmlFor="edit-ssh-host">Host</Label>
              <Input
                id="edit-ssh-host"
                value={host}
                onChange={(e) => setHost(e.target.value)}
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="edit-ssh-port">Port</Label>
              <Input
                id="edit-ssh-port"
                type="number"
                value={port}
                onChange={(e) => setPort(e.target.value)}
                required
              />
            </div>
          </div>
          <div className="space-y-2">
            <Label htmlFor="edit-ssh-username">
              {isKubernetes ? "Service account name" : "Username"}
            </Label>
            <Input
              id="edit-ssh-username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="edit-ssh-password">
              {isKubernetes ? "Service account token" : "Password"} (leave blank
              to keep unchanged)
            </Label>
            <Input
              id="edit-ssh-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>
          {isKubernetes && (
            <>
              <div className="space-y-2">
                <Label htmlFor="edit-k8s-namespace">Namespace</Label>
                <Input
                  id="edit-k8s-namespace"
                  data-testid="k8s-server-edit-namespace-input"
                  value={k8sNamespace}
                  onChange={(e) => setK8sNamespace(e.target.value)}
                  required
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="edit-k8s-ca-cert">
                  CA certificate (PEM){" "}
                  <span className="font-normal text-muted-foreground">
                    — optional
                  </span>
                </Label>
                <textarea
                  id="edit-k8s-ca-cert"
                  data-testid="k8s-server-edit-ca-cert-input"
                  className="flex min-h-[96px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                  value={k8sCaCert}
                  onChange={(e) => setK8sCaCert(e.target.value)}
                />
                <p className="text-xs text-muted-foreground">
                  Public challenge material, so unlike the token it is shown
                  back to you. Blank means the CA pinned on first connect is
                  what verifies the API server.
                </p>
              </div>
              {/* Which CA is actually in force is the question an operator
                    asks when a connection starts failing, so answer it here
                    rather than leaving them to infer it from two fields. */}
              {learnedCaCert && !k8sCaCert && (
                <div
                  data-testid="k8s-server-edit-learned-ca"
                  className="space-y-2 rounded-lg border p-3"
                >
                  <p className="text-sm font-medium">Pinned on first connect</p>
                  <p className="text-xs text-muted-foreground">
                    dbbat learned this CA itself and refuses anything else. If
                    the cluster&apos;s CA has rotated, paste the new bundle
                    above — or forget the pin and let the next connect pin
                    afresh, which only makes sense once you know why it changed.
                  </p>
                  <pre className="max-h-24 overflow-auto rounded bg-muted p-2 font-mono text-[10px] leading-tight">
                    {learnedCaCert}
                  </pre>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    data-testid="k8s-server-edit-reset-learned-ca"
                    onClick={() => setResetLearnedCa(true)}
                    disabled={resetLearnedCa}
                  >
                    {resetLearnedCa
                      ? "Will be forgotten on save"
                      : "Forget the learned CA"}
                  </Button>
                </div>
              )}
              <div className="flex items-center justify-between rounded-lg border p-3">
                <div className="space-y-0.5">
                  <Label htmlFor="edit-k8s-insecure">
                    Skip TLS verification
                  </Label>
                  <p className="text-sm text-muted-foreground">
                    Anything that can intercept the API server connection can
                    read the service account token. Throwaway clusters only.
                  </p>
                </div>
                <Switch
                  id="edit-k8s-insecure"
                  data-testid="k8s-server-edit-insecure-switch"
                  checked={k8sInsecure}
                  onCheckedChange={setK8sInsecure}
                />
              </div>
              {/* The bastion in front of the API server is not set-once: the
                  create dialog offers it, so the edit dialog has to be able to
                  change it. SSH rows get no such field — chaining a bastion
                  behind another is configured on the row that dials. */}
              <div className="space-y-2">
                <Label htmlFor="edit-k8s-via">Via SSH bastion</Label>
                <Select
                  value={viaUid || "none"}
                  onValueChange={(v) => setViaUid(v === "none" ? "" : v)}
                >
                  <SelectTrigger
                    id="edit-k8s-via"
                    data-testid="k8s-server-edit-via-select"
                  >
                    <SelectValue placeholder="Direct (no tunnel)" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="none">Direct (no tunnel)</SelectItem>
                    {viaOptions.map((srv) => (
                      <SelectItem key={srv.uid} value={srv.uid}>
                        {srv.name} (
                        {PROTOCOL_LABEL[srv.protocol as Protocol] ??
                          srv.protocol}
                        )
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-xs text-muted-foreground">
                  Only needed when the API server itself is reachable only
                  through a jump host.
                </p>
              </div>
            </>
          )}
          {!isKubernetes && (
            <>
              <div className="space-y-2">
                <Label htmlFor="edit-ssh-private-key">
                  SSH Private Key (PEM, leave blank to keep unchanged)
                </Label>
                <textarea
                  id="edit-ssh-private-key"
                  className="flex min-h-[96px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                  value={sshPrivateKey}
                  onChange={(e) => setSshPrivateKey(e.target.value)}
                  placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                />
                <p className="text-xs text-muted-foreground">
                  Write-only: the stored key is never shown again.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="edit-ssh-passphrase">Key Passphrase</Label>
                <Input
                  id="edit-ssh-passphrase"
                  type="password"
                  value={sshPassphrase}
                  onChange={(e) => setSshPassphrase(e.target.value)}
                />
              </div>
            </>
          )}
      </div>
    </FormSection>
  );
}
