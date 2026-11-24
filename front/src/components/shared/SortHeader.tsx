import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";

import { nextSort, type SortState } from "@/lib/sort";

/** A column title that sorts the list server-side when clicked. */
export function SortHeader({
  label,
  sortKey,
  current,
  onSort,
}: {
  label: string;
  sortKey: string;
  current: SortState;
  onSort: (next: SortState) => void;
}) {
  const active = current.sort === sortKey;
  const Icon = !active ? ArrowUpDown : current.order === "asc" ? ArrowUp : ArrowDown;
  return (
    <button
      type="button"
      className="inline-flex items-center gap-1 font-medium hover:text-foreground"
      data-testid={`sort-${sortKey}`}
      data-sort-state={active ? current.order : "none"}
      onClick={() => onSort(nextSort(current, sortKey))}
    >
      {label}
      <Icon className={active ? "h-3.5 w-3.5" : "h-3.5 w-3.5 opacity-40"} />
    </button>
  );
}
