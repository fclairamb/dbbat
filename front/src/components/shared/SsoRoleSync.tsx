/**
 * UI for roles the identity provider owns.
 *
 * With `DBB_OIDC_ROLE_MAPPING` configured, the directory is authoritative for
 * the roles it names and re-applies them on *every* login. Without a visible
 * marker an admin ticks `admin`, saves, and watches it disappear at the user's
 * next sign-in — or unticks it to revoke access and it comes straight back.
 * These pieces make that ownership visible before the save, and show the last
 * sync so "why did this change?" has an answer on the page.
 */
import { ShieldCheck } from "lucide-react";
import { formatDistanceToNow } from "date-fns";
import type { UserRoleSync } from "@/api";
import { Badge } from "@/components/ui/badge";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

/** The `user.roles_synced` audit payload, as written by auditRoleSync. */
export type RoleSyncDetails = {
  provider?: string;
  groups: string[];
  granted: string[];
  revoked: string[];
};

const asStringArray = (value: unknown): string[] =>
  Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];

/** Read a role-sync audit entry's details defensively — it is free-form JSON. */
export function parseRoleSyncDetails(sync: UserRoleSync): RoleSyncDetails {
  const details = sync.details ?? {};
  return {
    provider:
      typeof details.provider === "string" ? details.provider : undefined,
    groups: asStringArray(details.groups),
    granted: asStringArray(details.granted),
    revoked: asStringArray(details.revoked),
  };
}

/**
 * Why a role edit will not stick: shown next to a role the mapping governs.
 */
export const MANAGED_ROLE_EXPLANATION =
  "Managed by SSO: your identity provider decides this role from directory " +
  "group membership, and re-applies it at every login. Changes made here are " +
  "undone the next time the user signs in.";

/** Inline "managed by SSO" marker, sized to sit beside a role badge. */
export function ManagedRoleMarker({ role }: { role: string }) {
  return (
    <Tooltip delayDuration={0}>
      <TooltipTrigger asChild>
        <span
          className="inline-flex items-center gap-1 text-xs text-muted-foreground"
          data-testid={`managed-role-${role}`}
          aria-label={`${role} role is managed by SSO`}
        >
          <ShieldCheck className="h-3 w-3" />
          SSO
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-xs">
        {MANAGED_ROLE_EXPLANATION}
      </TooltipContent>
    </Tooltip>
  );
}

/** Full "managed by SSO" badge, for the role checkboxes on the edit form. */
export function ManagedRoleBadge({ role }: { role: string }) {
  return (
    <Tooltip delayDuration={0}>
      <TooltipTrigger asChild>
        <Badge
          variant="outline"
          className="gap-1 text-xs font-normal"
          data-testid={`managed-role-badge-${role}`}
        >
          <ShieldCheck className="h-3 w-3" />
          Managed by SSO
        </Badge>
      </TooltipTrigger>
      <TooltipContent className="max-w-xs">
        {MANAGED_ROLE_EXPLANATION}
      </TooltipContent>
    </Tooltip>
  );
}

/**
 * The last time the directory changed this user's roles, and what it changed.
 *
 * The groups come from the audit row itself — `GET /users/role-syncs` hands
 * back the entry verbatim precisely so they survive: they are the reason the
 * change happened, and reconstructing it later without them is impossible.
 */
export function LastRoleSync({ sync }: { sync: UserRoleSync }) {
  const { provider, groups, granted, revoked } = parseRoleSyncDetails(sync);

  return (
    <div
      className="rounded-md border bg-muted/40 p-3 space-y-2 text-sm"
      data-testid="last-role-sync"
    >
      <div className="flex items-center gap-2">
        <ShieldCheck className="h-4 w-4 text-muted-foreground" />
        <span className="font-medium">Last synced from SSO</span>
        <span className="text-muted-foreground">
          {formatDistanceToNow(new Date(sync.created_at), {
            addSuffix: true,
          })}
          {provider ? ` · ${provider}` : ""}
        </span>
      </div>
      {(granted.length > 0 || revoked.length > 0) && (
        <div className="flex flex-wrap gap-1" data-testid="last-role-sync-changes">
          {granted.map((role) => (
            <Badge key={`granted-${role}`} variant="secondary">
              +{role}
            </Badge>
          ))}
          {revoked.map((role) => (
            <Badge key={`revoked-${role}`} variant="outline">
              −{role}
            </Badge>
          ))}
        </div>
      )}
      <p className="text-xs text-muted-foreground">
        {groups.length > 0
          ? `Directory groups at that login: ${groups.join(", ")}`
          : "That login carried no directory groups."}
      </p>
    </div>
  );
}
