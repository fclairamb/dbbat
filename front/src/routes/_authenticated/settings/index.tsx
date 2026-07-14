import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  useInstance,
  useUpdateInstancePublic,
  useParameters,
  useUpdateParameter,
  useDeleteParameter,
  type GlobalParameter,
  type InstanceInfo,
  type PublicEndpoints,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { useAuth } from "@/contexts/AuthContext";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
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
import { Textarea } from "@/components/ui/textarea";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { ChevronDown, Pencil, Trash2 } from "lucide-react";
import { toast } from "sonner";

export const Route = createFileRoute("/_authenticated/settings/")({
  component: SettingsPage,
});

function SettingsPage() {
  const { user } = useAuth();
  const isAdmin = user?.roles?.includes("admin") ?? false;

  if (!isAdmin) {
    return (
      <div className="space-y-6">
        <PageHeader
          title="Settings"
          description="Instance configuration and public endpoint advertisement"
        />
        <Card>
          <CardContent className="pt-6">
            <p className="text-muted-foreground">
              Settings are only available to administrators.
            </p>
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Settings"
        description="Instance configuration and public endpoint advertisement"
      />
      <LocalListenersSection />
      <PublicAdvertisementSection />
      <RawParametersSection />
    </div>
  );
}

interface ListenerRow {
  protocol: string;
  address: string;
}

function ListenerTable({ rows }: { rows: ListenerRow[] }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Protocol</TableHead>
          <TableHead>Bound address</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={row.protocol}>
            <TableCell>{row.protocol}</TableCell>
            <TableCell className="font-mono text-sm">
              {row.address || <span className="text-muted-foreground italic">disabled</span>}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function LocalListenersSection() {
  const { data: instance } = useInstance();

  const httpRows: ListenerRow[] = [
    { protocol: "API / Web UI", address: instance?.listen.api ?? "" },
  ];
  const tcpRows: ListenerRow[] = [
    { protocol: "PostgreSQL", address: instance?.listen.pg ?? "" },
    { protocol: "Oracle", address: instance?.listen.ora ?? "" },
    { protocol: "MySQL", address: instance?.listen.mysql ?? "" },
  ];

  return (
    <Card data-testid="local-listeners-section" className="bg-muted/40">
      <CardHeader>
        <CardTitle>Local listeners</CardTitle>
        <CardDescription>
          These are the addresses this server process is bound to. Change
          them via <code className="text-xs">DBB_LISTEN_*</code> environment
          variables and restart.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-6">
        <div data-testid="http-listener-group" className="space-y-2">
          <div>
            <h4 className="text-sm font-medium">HTTP — API / Web UI</h4>
            <p className="text-xs text-muted-foreground">
              Put an HTTP reverse proxy / ingress in front of this port for
              the browser and REST API.
            </p>
          </div>
          <ListenerTable rows={httpRows} />
        </div>
        <div data-testid="tcp-listener-group" className="space-y-2">
          <div>
            <h4 className="text-sm font-medium">TCP — SQL proxies</h4>
            <p className="text-xs text-muted-foreground">
              Put a TCP load balancer in front of these ports for SQL
              clients connecting through dbbat.
            </p>
          </div>
          <ListenerTable rows={tcpRows} />
        </div>
      </CardContent>
    </Card>
  );
}

function resolveHost(protoHost: string | undefined, defaultHost: string): string {
  return protoHost || defaultHost || "";
}

function resolvePort(override: number | null | undefined, listenAddr: string | undefined): string {
  if (override != null) return String(override);
  if (!listenAddr) return "disabled";
  const parts = listenAddr.split(":");
  return parts[parts.length - 1] ?? "";
}

function PublicAdvertisementSection() {
  const { data: instance } = useInstance();
  const pub = instance?.public;
  const listen = instance?.listen;

  return (
    <PublicAdvertisementForm key={pub ? "loaded" : "init"} pub={pub} listen={listen} />
  );
}

function PublicAdvertisementForm({
  pub,
  listen,
}: {
  pub: PublicEndpoints | undefined;
  listen: InstanceInfo["listen"] | undefined;
}) {
  const updatePublic = useUpdateInstancePublic({
    onSuccess: () => toast.success("Settings saved"),
    onError: (e) => toast.error(e.message),
  });

  const [webUIURL, setWebUIURL] = useState(pub?.web_ui_url ?? "");
  const [host, setHost] = useState(pub?.host ?? "");
  const [pgHostOverride, setPgHostOverride] = useState(pub?.pg_host ?? "");
  const [pgPortOverride, setPgPortOverride] = useState(
    pub?.pg_port != null ? String(pub.pg_port) : ""
  );
  const [pgOverrideEnabled, setPgOverrideEnabled] = useState(
    !!(pub?.pg_host || pub?.pg_port != null)
  );
  const [oraHostOverride, setOraHostOverride] = useState(pub?.ora_host ?? "");
  const [oraPortOverride, setOraPortOverride] = useState(
    pub?.ora_port != null ? String(pub.ora_port) : ""
  );
  const [oraOverrideEnabled, setOraOverrideEnabled] = useState(
    !!(pub?.ora_host || pub?.ora_port != null)
  );
  const [mysqlHostOverride, setMysqlHostOverride] = useState(pub?.mysql_host ?? "");
  const [mysqlPortOverride, setMysqlPortOverride] = useState(
    pub?.mysql_port != null ? String(pub.mysql_port) : ""
  );
  const [mysqlOverrideEnabled, setMysqlOverrideEnabled] = useState(
    !!(pub?.mysql_host || pub?.mysql_port != null)
  );

  const handleSave = () => {
    const body: PublicEndpoints = {
      host,
      pg_host: pgOverrideEnabled ? pgHostOverride : "",
      pg_port: pgOverrideEnabled && pgPortOverride ? parseInt(pgPortOverride, 10) : null,
      ora_host: oraOverrideEnabled ? oraHostOverride : "",
      ora_port: oraOverrideEnabled && oraPortOverride ? parseInt(oraPortOverride, 10) : null,
      mysql_host: mysqlOverrideEnabled ? mysqlHostOverride : "",
      mysql_port: mysqlOverrideEnabled && mysqlPortOverride ? parseInt(mysqlPortOverride, 10) : null,
      web_ui_url: webUIURL,
    };
    updatePublic.mutate(body);
  };

  return (
    <div className="space-y-6">
      <Card data-testid="web-ui-host-section">
        <CardHeader>
          <CardTitle>Web UI host</CardTitle>
          <CardDescription>
            Where the browser and REST API are reached, behind an HTTP
            ingress / reverse proxy — e.g.{" "}
            <code className="text-xs">https://dbbat.company.com</code>. Falls
            back to the <code className="text-xs">DBB_PUBLIC_URL</code>{" "}
            environment variable when unset. Used for Slack deep-links and
            other absolute links dbbat generates. Distinct from the
            connection host below — these are two separate network paths.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="space-y-2">
            <Label htmlFor="web-ui-url">Web UI host / public base URL</Label>
            <Input
              id="web-ui-url"
              data-testid="public-web-ui-url-input"
              placeholder="e.g. https://dbbat.company.com"
              value={webUIURL}
              onChange={(e) => setWebUIURL(e.target.value)}
            />
          </div>
        </CardContent>
      </Card>

      <Card data-testid="connection-host-section">
        <CardHeader>
          <CardTitle>Connection host</CardTitle>
          <CardDescription>
            Where SQL clients connect to the PostgreSQL / Oracle / MySQL
            proxies, via direct access or a TCP load balancer — e.g.{" "}
            <code className="text-xs">db.company.com</code>. Configure the
            host and ports that clients should use to connect through the
            proxy. Distinct from the Web UI host above.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-6">
          <div className="space-y-2">
            <Label htmlFor="public-host">Default connection host</Label>
            <Input
              id="public-host"
              data-testid="public-host-input"
              placeholder="e.g. db.company.com"
              value={host}
              onChange={(e) => setHost(e.target.value)}
            />
          </div>

          <ProtocolOverrideRow
            protocol="PostgreSQL"
            listenAddr={listen?.pg}
            defaultHost={host}
            enabled={pgOverrideEnabled}
            onEnabledChange={setPgOverrideEnabled}
            hostValue={pgHostOverride}
            onHostChange={setPgHostOverride}
            portValue={pgPortOverride}
            onPortChange={setPgPortOverride}
            hostTestId="public-pg-host-input"
            portTestId="public-pg-port-input"
          />

          <ProtocolOverrideRow
            protocol="Oracle"
            listenAddr={listen?.ora}
            defaultHost={host}
            enabled={oraOverrideEnabled}
            onEnabledChange={setOraOverrideEnabled}
            hostValue={oraHostOverride}
            onHostChange={setOraHostOverride}
            portValue={oraPortOverride}
            onPortChange={setOraPortOverride}
            hostTestId="public-ora-host-input"
            portTestId="public-ora-port-input"
          />

          <ProtocolOverrideRow
            protocol="MySQL"
            listenAddr={listen?.mysql}
            defaultHost={host}
            enabled={mysqlOverrideEnabled}
            onEnabledChange={setMysqlOverrideEnabled}
            hostValue={mysqlHostOverride}
            onHostChange={setMysqlHostOverride}
            portValue={mysqlPortOverride}
            onPortChange={setMysqlPortOverride}
            hostTestId="public-mysql-host-input"
            portTestId="public-mysql-port-input"
          />
        </CardContent>
      </Card>

      <Button
        data-testid="save-public-settings-btn"
        onClick={handleSave}
        disabled={updatePublic.isPending}
      >
        {updatePublic.isPending ? "Saving…" : "Save"}
      </Button>
    </div>
  );
}

interface ProtocolOverrideRowProps {
  protocol: string;
  listenAddr: string | undefined;
  defaultHost: string;
  enabled: boolean;
  onEnabledChange: (v: boolean) => void;
  hostValue: string;
  onHostChange: (v: string) => void;
  portValue: string;
  onPortChange: (v: string) => void;
  hostTestId: string;
  portTestId: string;
}

function ProtocolOverrideRow({
  protocol,
  listenAddr,
  defaultHost,
  enabled,
  onEnabledChange,
  hostValue,
  onHostChange,
  portValue,
  onPortChange,
  hostTestId,
  portTestId,
}: ProtocolOverrideRowProps) {
  const resolvedHost = resolveHost(enabled ? hostValue : undefined, defaultHost);
  const resolvedPort = resolvePort(
    enabled && portValue ? parseInt(portValue, 10) : null,
    listenAddr
  );
  const resolvedLabel =
    resolvedHost ? `${resolvedHost}:${resolvedPort}` : resolvedPort;

  return (
    <div className="rounded-md border p-4 space-y-3">
      <div className="flex items-center justify-between">
        <span className="font-medium">{protocol}</span>
        <div className="flex items-center gap-2">
          <span className="text-sm text-muted-foreground">Override</span>
          <Switch checked={enabled} onCheckedChange={onEnabledChange} />
        </div>
      </div>
      {enabled && (
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1">
            <Label className="text-xs">Host override</Label>
            <Input
              data-testid={hostTestId}
              placeholder="(use default host)"
              value={hostValue}
              onChange={(e) => onHostChange(e.target.value)}
            />
          </div>
          <div className="space-y-1">
            <Label className="text-xs">Port override</Label>
            <Input
              data-testid={portTestId}
              placeholder="(use local port)"
              type="number"
              value={portValue}
              onChange={(e) => onPortChange(e.target.value)}
            />
          </div>
        </div>
      )}
      <div className="text-xs text-muted-foreground flex items-center gap-1">
        Resolved:
        <Badge variant="secondary" className="font-mono">
          {resolvedLabel || "—"}
        </Badge>
      </div>
    </div>
  );
}

function RawParametersSection() {
  const { data: params, isLoading } = useParameters();
  const updateParam = useUpdateParameter({
    onSuccess: () => toast.success("Parameter updated"),
    onError: (e) => toast.error(e.message),
  });
  const deleteParam = useDeleteParameter({
    onSuccess: () => toast.success("Parameter deleted"),
    onError: (e) => toast.error(e.message),
  });

  const [editTarget, setEditTarget] = useState<GlobalParameter | null>(null);
  const [editValue, setEditValue] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<GlobalParameter | null>(null);

  const openEdit = (p: GlobalParameter) => {
    setEditTarget(p);
    setEditValue(p.value);
  };

  const handleEdit = () => {
    if (!editTarget) return;
    updateParam.mutate(
      { group: editTarget.group_key, key: editTarget.key, value: editValue },
      { onSuccess: () => setEditTarget(null) }
    );
  };

  return (
    <Collapsible data-testid="raw-parameters-section">
      <Card>
        <CardHeader>
          <CollapsibleTrigger asChild>
            <div className="flex items-center justify-between cursor-pointer select-none">
              <div>
                <CardTitle>Raw parameters</CardTitle>
                <CardDescription>
                  All stored global parameters across all groups.
                </CardDescription>
              </div>
              <ChevronDown className="h-4 w-4 text-muted-foreground transition-transform [[data-state=open]_&]:rotate-180" />
            </div>
          </CollapsibleTrigger>
        </CardHeader>
        <CollapsibleContent>
          <CardContent>
            {isLoading ? (
              <p className="text-sm text-muted-foreground">Loading…</p>
            ) : !params?.length ? (
              <p className="text-sm text-muted-foreground">
                No parameters stored yet.
              </p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Group</TableHead>
                    <TableHead>Key</TableHead>
                    <TableHead>Value</TableHead>
                    <TableHead className="w-20">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {params.map((p) => (
                    <TableRow key={p.uid}>
                      <TableCell className="font-mono text-xs">
                        {p.group_key}
                      </TableCell>
                      <TableCell className="font-mono text-xs">
                        {p.key}
                      </TableCell>
                      <TableCell className="font-mono text-xs max-w-xs truncate">
                        {p.value}
                      </TableCell>
                      <TableCell>
                        <div className="flex gap-1">
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => openEdit(p)}
                          >
                            <Pencil className="h-4 w-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => setDeleteTarget(p)}
                          >
                            <Trash2 className="h-4 w-4 text-destructive" />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </CollapsibleContent>
      </Card>

      {/* Edit dialog */}
      <Dialog open={!!editTarget} onOpenChange={(o) => !o && setEditTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Edit parameter</DialogTitle>
            <DialogDescription>
              {editTarget?.group_key} / {editTarget?.key}
            </DialogDescription>
          </DialogHeader>
          <Textarea
            value={editValue}
            onChange={(e) => setEditValue(e.target.value)}
            rows={4}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditTarget(null)}>
              Cancel
            </Button>
            <Button onClick={handleEdit} disabled={updateParam.isPending}>
              Save
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete confirmation dialog */}
      <AlertDialog
        open={!!deleteTarget}
        onOpenChange={(o) => !o && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete parameter?</AlertDialogTitle>
            <AlertDialogDescription>
              This will soft-delete{" "}
              <code>
                {deleteTarget?.group_key}/{deleteTarget?.key}
              </code>
              . It can be re-created with the same key.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (!deleteTarget) return;
                deleteParam.mutate(
                  { group: deleteTarget.group_key, key: deleteTarget.key },
                  { onSuccess: () => setDeleteTarget(null) }
                );
              }}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Collapsible>
  );
}
