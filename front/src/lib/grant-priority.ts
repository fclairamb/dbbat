/**
 * Grant selection priority.
 *
 * A user can hold several active grants on the same database. The proxy admits
 * a new session under the highest-priority one (ties break on the latest
 * expiry, then the newest). The priority is auto-calculated from the grant's
 * controls unless an admin overrides it.
 *
 * This mirrors `store.AutoPriority` in `internal/store/grants.go` and the SQL
 * backfill in `20260806000000_grants_priority.up.sql`. Keep the three in sync —
 * the form previews the value the server would compute, so a drift here shows
 * up as a field that lies about what is about to be saved.
 */

/** A writable grant carrying no controls at all. */
export const PRIORITY_FULL_WRITE = 100;
/** A still-writable grant carrying controls (block_copy / block_ddl). */
export const PRIORITY_RESTRICTED_WRITE = 50;
/** A read_only grant, whatever else it carries. */
export const PRIORITY_READ_ONLY = 10;

/** Computes the default priority for a grant with the given controls. */
export function autoPriority(controls: readonly string[]): number {
  if (controls.includes("read_only")) return PRIORITY_READ_ONLY;
  if (controls.length === 0) return PRIORITY_FULL_WRITE;

  return PRIORITY_RESTRICTED_WRITE;
}
