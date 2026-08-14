import { Label } from "@/components/ui/label";
import { MultiSelect } from "@/components/shared/MultiSelect";
import { useUserGroups } from "@/api";

/**
 * The two approver pickers a server and a server group both carry.
 *
 * Two deliberately separate roles, with no hierarchy between them: an access
 * approver decides grant *requests*, a query approver releases approval *holds*
 * on live statements. An organization wanting overlap picks the same user group
 * in both.
 *
 * One component for both surfaces so the copy — which is where the fallback
 * chain is actually explained to an operator — cannot drift between them. The
 * `scope` prop only changes which step of the chain the wording names.
 */
export function ApproverGroupPickers({
  scope,
  accessSelected,
  onAccessChange,
  querySelected,
  onQueryChange,
  testIdPrefix,
}: {
  /** "server" names the group fallback below it; "group" names the override above it. */
  scope: "server" | "group";
  accessSelected: string[];
  onAccessChange: (next: string[]) => void;
  querySelected: string[];
  onQueryChange: (next: string[]) => void;
  testIdPrefix: string;
}) {
  const { data: userGroups = [] } = useUserGroups();
  const options = userGroups.map((g) => ({ value: g.uid, label: g.name }));

  const accessFallback =
    scope === "server"
      ? "Empty falls back to the server groups this server belongs to, and then to admins alone."
      : "A server that names its own access approvers overrides this. Empty leaves the decision to admins.";

  const queryFallback =
    scope === "server"
      ? "Empty falls back to the server groups this server belongs to, and then to admins alone."
      : "A server that names its own query approvers overrides this. Empty leaves the decision to admins.";

  return (
    <>
      <div className="space-y-2">
        <Label>Access approvers (grant requests)</Label>
        <p className="text-xs text-muted-foreground">
          User groups whose members may approve or deny grant requests for{" "}
          {scope === "server" ? "this server" : "the servers in this group"},
          alongside admins. {accessFallback} Nobody may approve their own
          request, however they are named here.
        </p>
        <MultiSelect
          options={options}
          selected={accessSelected}
          onChange={onAccessChange}
          placeholder="Admins only"
          emptyMessage="No user groups defined yet — only admins can approve."
          testId={`${testIdPrefix}-access-approvers`}
        />
      </div>
      <div className="space-y-2">
        <Label>Query approvers (approval holds)</Label>
        <p className="text-xs text-muted-foreground">
          User groups whose members may release approval holds on statements
          against {scope === "server" ? "this server" : "these servers"}.
          Independent of access approvers — neither role implies the other.{" "}
          {queryFallback} A grant definition that names its own approvers still
          wins over this.
        </p>
        <MultiSelect
          options={options}
          selected={querySelected}
          onChange={onQueryChange}
          placeholder="Admins only"
          emptyMessage="No user groups defined yet — only admins can approve."
          testId={`${testIdPrefix}-query-approvers`}
        />
        {/*
          Both lists are resolved at decision time and never snapshotted — the
          second deliberate exception to dbbat's "a live grant's behaviour never
          changes under it" rule, after live server-group membership. Saying so
          here is the point: an operator editing this is changing who can act on
          work that is already waiting.
        */}
        <p className="text-xs text-muted-foreground">
          Both lists are read live, at decision time: saving changes who may
          decide immediately, including for requests already filed and
          statements already parked.
        </p>
      </div>
    </>
  );
}
