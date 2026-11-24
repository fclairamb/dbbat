import { formatControlName, formatDuration } from "@/lib/grant-format";
import { useMemo, useState } from "react";
import {
  useUsers,
  useDatabases,
  useGrantDefinitions,
  useServerGroups,
  useAssignGrant,
  type GrantDefinition,
} from "@/api";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { toast } from "sonner";
import { formatDateTimeLocal } from "@/lib/date-utils";
import { formatBytes } from "@/lib/utils";
import { useNavigate } from "@tanstack/react-router";
import { FormPage } from "@/components/shared/FormPage";

/**
 * AssignGrantDialog replaced the old "Create Grant" form. An admin no longer
 * invents a grant's shape here: they pick a definition, and the controls,
 * quotas, approval gating and duration all come from it. That is what makes a
 * definition trustworthy as the policy source of truth — no grant can be an
 * unauditable one-off.
 */
/** Assign-grant form (the `new` route). */
export function AssignGrantForm() {
  const navigate = useNavigate();
  const onClose = () => navigate({ to: "/grants" });
  const { data: users = [] } = useUsers();
  const { data: databases = [] } = useDatabases();
  const { data: definitions } = useGrantDefinitions({ active_only: true });
  // The assign page is admin-only (POST /grants requires admin), so the
  // admin-gated server-group listing is always available here.
  const { data: serverGroups = [] } = useServerGroups();
  const [definitionUid, setDefinitionUid] = useState("");
  const [userId, setUserId] = useState("");
  const [databaseId, setDatabaseId] = useState("");
  const [startsAt, setStartsAt] = useState(() => {
    const now = new Date();
    now.setSeconds(0, 0);
    return formatDateTimeLocal(now);
  });

  const definition: GrantDefinition | undefined = useMemo(
    () => definitions?.find((d) => d.uid === definitionUid),
    [definitions, definitionUid],
  );

  // A definition scoped to server groups can only be assigned against the
  // databases those groups currently hold — the server enforces it, so the
  // picker shouldn't offer the rest. Membership is live, so this narrows to
  // whatever the groups contain right now.
  const selectableDatabases = useMemo(() => {
    const scope = definition?.server_group_uids ?? [];
    if (scope.length === 0) return databases;

    const inScope = new Set(
      serverGroups
        .filter((g) => scope.includes(g.uid))
        .flatMap((g) => g.member_uids),
    );

    return databases.filter((d) => inScope.has(d.uid));
  }, [databases, definition, serverGroups]);

  const durationMs = (definition?.duration_seconds ?? 0) * 1000;

  const assignGrant = useAssignGrant({
    onSuccess: () => {
      toast.success("Grant assigned successfully");
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    assignGrant.mutate({
      grant_definition_id: definitionUid,
      user_id: userId,
      database_id: databaseId,
      starts_at: new Date(startsAt).toISOString(),
    });
  };

  return (
    <FormPage
      testId="assign-grant-page"
      title={<>Assign Grant</>}
      description={<>Give a user access to a database by issuing a grant definition. The
            definition decides what the access can do and how long it lasts.</>}
      backTo="/servers"
      backLabel="Servers"
      onCancel={onClose}
      onSubmit={handleSubmit}
      submitLabel="Assign"
      submitTestId="assign-grant-submit"
      submitDisabled={
              assignGrant.isPending || !definitionUid || !userId || !databaseId
            }
    >
      <div className="max-w-2xl space-y-4">
          <div className="space-y-2">
            <Label htmlFor="definition">Grant definition</Label>
            <Select
              value={definitionUid}
              onValueChange={(value) => {
                setDefinitionUid(value);
                setDatabaseId("");
              }}
              required
            >
              <SelectTrigger data-testid="assign-grant-definition">
                <SelectValue placeholder="Select a grant definition" />
              </SelectTrigger>
              <SelectContent>
                {(definitions ?? []).map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {definitions?.length === 0 && (
              <p className="text-sm text-muted-foreground">
                No active grant definitions yet — create one first; grants can
                only be issued from a definition.
              </p>
            )}
          </div>

          {definition && (
            <div
              className="rounded-md border p-3 space-y-2 text-sm"
              data-testid="assign-grant-shape"
            >
              <div className="flex flex-wrap items-center gap-1">
                <span className="text-muted-foreground">Controls:</span>
                {(definition.controls ?? []).length === 0 ? (
                  <Badge variant="default">Full Access</Badge>
                ) : (
                  definition.controls.map((control) => (
                    <Badge key={control} variant="secondary">
                      {formatControlName(control)}
                    </Badge>
                  ))
                )}
              </div>
              <div className="text-muted-foreground">
                Duration: {formatDuration(durationMs)}
              </div>
              <div className="text-muted-foreground">
                Quotas:{" "}
                {definition.max_query_counts
                  ? `${definition.max_query_counts} queries`
                  : "unlimited queries"}
                {", "}
                {definition.max_bytes_transferred
                  ? formatBytes(definition.max_bytes_transferred)
                  : "unlimited transfer"}
              </div>
              {(definition.approval_patterns?.length ?? 0) > 0 && (
                <div className="text-muted-foreground">
                  Approval holds: {definition.approval_patterns?.length} pattern
                  {definition.approval_patterns?.length !== 1 ? "s" : ""}
                </div>
              )}
            </div>
          )}

          <div className="space-y-2">
            <Label htmlFor="user">User</Label>
            <Select value={userId} onValueChange={setUserId} required>
              <SelectTrigger data-testid="assign-grant-user">
                <SelectValue placeholder="Select user" />
              </SelectTrigger>
              <SelectContent>
                {users.map((u) => (
                  <SelectItem key={u.uid} value={u.uid}>
                    {u.username}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="database">Database</Label>
            <Select value={databaseId} onValueChange={setDatabaseId} required>
              <SelectTrigger data-testid="assign-grant-database">
                <SelectValue placeholder="Select database" />
              </SelectTrigger>
              <SelectContent>
                {selectableDatabases.map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {definition && (definition.server_group_uids ?? []).length > 0 && (
              <p className="text-xs text-muted-foreground">
                This definition is scoped to server groups — only the servers
                those groups currently hold are offered.
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="startsAt">Start Date &amp; Time</Label>
            <Input
              id="startsAt"
              type="datetime-local"
              value={startsAt}
              onChange={(e) => setStartsAt(e.target.value)}
              required
            />
            <p className="text-xs text-muted-foreground">
              The grant expires {formatDuration(durationMs)} after it starts —
              that length comes from the definition.
            </p>
          </div>
      </div>
    </FormPage>
  );
}
