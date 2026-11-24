export type SortOrder = "asc" | "desc";

export interface SortState {
  sort?: string;
  order?: SortOrder;
}

/**
 * Click cycle of a sortable title: desc, then asc, then back to the default
 * (no sort in the URL). Returned as a search patch.
 */
export function nextSort(current: SortState, key: string): SortState {
  if (current.sort !== key) return { sort: key, order: "desc" };
  if (current.order !== "asc") return { sort: key, order: "asc" };
  return { sort: undefined, order: undefined };
}

/** Parses the `sort` / `order` search params, dropping anything unknown. */
export function parseSortSearch(
  search: Record<string, unknown>,
  allowed: readonly string[],
): SortState {
  const sort =
    typeof search.sort === "string" && allowed.includes(search.sort)
      ? search.sort
      : undefined;
  if (!sort) return {};
  return { sort, order: search.order === "asc" ? "asc" : "desc" };
}
