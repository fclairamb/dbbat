/**
 * Maximum number of characters shown in a SQL preview (e.g. the query-detail
 * breadcrumb). Tweak this single constant to make previews longer or shorter.
 */
export const SQL_PREVIEW_MAX_LENGTH = 40;

/**
 * Build a compact single-line preview of a SQL statement: collapse any run of
 * whitespace (including newlines) to a single space, trim, and truncate to
 * {@link SQL_PREVIEW_MAX_LENGTH} characters, appending an ellipsis when the
 * text was cut off.
 *
 * e.g. `sqlPreview("SELECT id, name, created_at\n  FROM users WHERE ...")`
 *      → `"SELECT id, name, created_at FROM users …"`
 */
export function sqlPreview(
  sql: string | null | undefined,
  maxLength: number = SQL_PREVIEW_MAX_LENGTH
): string {
  const normalized = (sql ?? "").replace(/\s+/g, " ").trim();
  if (normalized.length <= maxLength) {
    return normalized;
  }
  return `${normalized.slice(0, maxLength).trimEnd()}…`;
}
