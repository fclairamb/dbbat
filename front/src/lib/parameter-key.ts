import type { GlobalParameter } from "@/api";

/**
 * The `$key` segment of `/settings/parameters/$key`: a parameter is identified
 * by its group and key together, so the route carries both.
 */
export function parameterRouteKey(p: Pick<GlobalParameter, "group_key" | "key">) {
  return `${p.group_key}:${p.key}`;
}
