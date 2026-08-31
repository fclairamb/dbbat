import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { MultiSelect } from "@/components/shared/MultiSelect";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { X } from "lucide-react";

/**
 * The bar only ever needs a uid and a display label from each option list, so
 * it asks for exactly that rather than for the full API types. `useDatabases`
 * in particular returns a union — a non-admin sees a narrower server payload —
 * and demanding the wide member here would make the bar unusable for them.
 */
type NamedOption = { uid: string; name: string };
type UserOption = { uid: string; username: string };

/**
 * The filter state both observability listings carry in their URL search
 * params. Every field maps 1:1 to a query parameter the API applies
 * **server-side** — nothing here narrows an already-fetched page.
 */
export type ObservabilityFilters = {
  user_id?: string;
  database_id?: string;
  server_group_uid?: string;
  grant_definition_uid?: string;
  /**
   * The exact grant *instance*. Deliberately has no dropdown — a busy instance
   * has many time-boxed instances per user — and is reached by deep-link from
   * a connection row. It still round-trips through the URL, and the bar shows
   * a clearable chip for it.
   */
  grant_uid?: string;
  /** Comma-separated `approved` / `auto` / `direct`. */
  grant_provenance?: string;
  /** `/queries` only: the per-statement approval-hold outcome. */
  approval_status?: string;
  /**
   * `/connections` only: still-open sessions. `true` or absent — `false` and
   * "no filter" are the same request, so the off state drops the parameter
   * rather than spelling it out.
   */
  active?: true;
};

/** Radix Select has no empty value, so "no filter" needs a sentinel. */
const ANY = "__any__";

const PROVENANCE_OPTIONS = [
  { value: "approved", label: "Approved by a human" },
  { value: "auto", label: "Auto-approved" },
  { value: "direct", label: "Directly issued" },
];

const APPROVAL_STATUS_OPTIONS = [
  { value: "pending", label: "Pending" },
  { value: "approved", label: "Approved" },
  { value: "denied", label: "Denied" },
  { value: "abandoned", label: "Abandoned" },
];

function FilterSelect({
  id,
  label,
  value,
  onValueChange,
  placeholder,
  options,
  testId,
}: {
  id: string;
  label: string;
  value?: string;
  onValueChange: (next: string | undefined) => void;
  placeholder: string;
  options: { value: string; label: string }[];
  testId: string;
}) {
  return (
    <div className="space-y-1">
      <Label htmlFor={id} className="text-xs text-muted-foreground">
        {label}
      </Label>
      <Select
        value={value ?? ANY}
        onValueChange={(next) => onValueChange(next === ANY ? undefined : next)}
      >
        <SelectTrigger id={id} className="w-[180px]" data-testid={testId}>
          <SelectValue placeholder={placeholder} />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={ANY}>{placeholder}</SelectItem>
          {options.map((option) => (
            <SelectItem key={option.value} value={option.value}>
              {option.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

/**
 * The filter bar shared by `/app/connections` and `/app/queries`.
 *
 * It is a pure controlled component: it never fetches and never holds state.
 * The route owns the values (they live in the URL search params so a filtered
 * view is shareable and the back button works) and is responsible for clearing
 * the pagination cursor on every change — a cursor taken from one result set is
 * meaningless against another and would silently hide the head of the list.
 *
 * Grant provenance and approval status are kept visually apart on purpose:
 * provenance says which *access* the session ran under, approval status says
 * which individual *statements* a second human released. Conflating them is
 * the single most likely misreading of this screen.
 */
export function ObservabilityFilterBar({
  filters,
  onChange,
  users,
  databases,
  serverGroups,
  grantDefinitions,
  showApprovalStatus = false,
  showActive = false,
}: {
  filters: ObservabilityFilters;
  /** Applies a partial change. The route merges it and resets the cursor. */
  onChange: (patch: ObservabilityFilters) => void;
  users?: UserOption[];
  databases?: NamedOption[];
  serverGroups?: NamedOption[];
  grantDefinitions?: NamedOption[];
  showApprovalStatus?: boolean;
  /** `/connections` only: a statement has no open/closed state. */
  showActive?: boolean;
}) {
  const provenance = filters.grant_provenance
    ? filters.grant_provenance.split(",").filter(Boolean)
    : [];

  const hasAny =
    Boolean(filters.active) ||
    Boolean(filters.user_id) ||
    Boolean(filters.database_id) ||
    Boolean(filters.server_group_uid) ||
    Boolean(filters.grant_definition_uid) ||
    Boolean(filters.grant_uid) ||
    provenance.length > 0 ||
    Boolean(filters.approval_status);

  const clearAll = () =>
    onChange({
      active: undefined,
      user_id: undefined,
      database_id: undefined,
      server_group_uid: undefined,
      grant_definition_uid: undefined,
      grant_uid: undefined,
      grant_provenance: undefined,
      approval_status: undefined,
    });

  return (
    <div
      className="rounded-lg border bg-card p-4 space-y-3"
      data-testid="observability-filters"
    >
      <div className="flex flex-wrap items-end gap-4">
        <FilterSelect
          id="filter-user"
          label="User"
          testId="filter-user"
          placeholder="Any user"
          value={filters.user_id}
          onValueChange={(user_id) => onChange({ user_id })}
          options={(users ?? []).map((u) => ({
            value: u.uid,
            label: u.username,
          }))}
        />

        {/* Labelled "Server": the API parameter is `database_id` for
            historical reasons, but the thing being picked is a server. */}
        <FilterSelect
          id="filter-server"
          label="Server"
          testId="filter-server"
          placeholder="Any server"
          value={filters.database_id}
          onValueChange={(database_id) => onChange({ database_id })}
          options={(databases ?? []).map((d) => ({
            value: d.uid,
            label: d.name,
          }))}
        />

        <FilterSelect
          id="filter-server-group"
          label="Server group"
          testId="filter-server-group"
          placeholder="Any group"
          value={filters.server_group_uid}
          onValueChange={(server_group_uid) => onChange({ server_group_uid })}
          options={(serverGroups ?? []).map((g) => ({
            value: g.uid,
            label: g.name,
          }))}
        />

        {/* The *policy*, not the instance: the server matches it across the
            definition's whole lineage, so picking today's version still finds
            sessions that ran under an earlier one. */}
        <FilterSelect
          id="filter-grant-definition"
          label="Grant policy"
          testId="filter-grant-definition"
          placeholder="Any policy"
          value={filters.grant_definition_uid}
          onValueChange={(grant_definition_uid) =>
            onChange({ grant_definition_uid })
          }
          options={(grantDefinitions ?? []).map((d) => ({
            value: d.uid,
            label: d.name,
          }))}
        />

        {showApprovalStatus && (
          <FilterSelect
            id="filter-approval-status"
            label="Approval hold"
            testId="filter-approval-status"
            placeholder="Any statement"
            value={filters.approval_status}
            onValueChange={(approval_status) => onChange({ approval_status })}
            options={APPROVAL_STATUS_OPTIONS}
          />
        )}

        <div className="space-y-1">
          <Label className="text-xs text-muted-foreground">
            Grant provenance
          </Label>
          <MultiSelect
            options={PROVENANCE_OPTIONS}
            selected={provenance}
            onChange={(next) =>
              onChange({
                grant_provenance: next.length > 0 ? next.join(",") : undefined,
              })
            }
            testId="filter-grant-provenance"
          />
        </div>

        {/* Lives here rather than beside the page title because it is a
            server-side filter exactly like the selects next to it — the page
            used to have two filtering mechanisms that looked unrelated and,
            worse, behaved differently. The id is kept as `showActive`: it is
            what the URL-round-trip tests locate. */}
        {showActive && (
          <div className="space-y-1">
            <Label className="text-xs text-muted-foreground">
              Session state
            </Label>
            <div className="flex h-9 items-center gap-2">
              <Switch
                id="showActive"
                data-testid="filter-active"
                checked={Boolean(filters.active)}
                onCheckedChange={(checked) =>
                  onChange({ active: checked ? true : undefined })
                }
              />
              <Label htmlFor="showActive" className="text-sm font-normal">
                Active only
              </Label>
            </div>
          </div>
        )}
      </div>

      {(filters.grant_uid || hasAny) && (
        <div className="flex flex-wrap items-center gap-2">
          {filters.grant_uid && (
            <Badge variant="secondary" className="gap-1">
              Grant {filters.grant_uid.slice(0, 8)}
              <Button
                variant="ghost"
                size="sm"
                className="h-4 w-4 p-0 hover:bg-transparent"
                onClick={() => onChange({ grant_uid: undefined })}
                data-testid="filter-grant-uid-clear"
                aria-label="Clear grant filter"
              >
                <X className="h-3 w-3" />
              </Button>
            </Badge>
          )}
          {hasAny && (
            <Button
              variant="ghost"
              size="sm"
              onClick={clearAll}
              data-testid="filter-clear-all"
            >
              Clear filters
            </Button>
          )}
        </div>
      )}
    </div>
  );
}
