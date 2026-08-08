import { Link } from "@tanstack/react-router";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

export interface Column<T> {
  key: string;
  header: string;
  cell: (item: T) => React.ReactNode;
  className?: string;
}

/**
 * A consecutive `<tbody>` section of the table.
 *
 * Sections exist so a subset of rows can carry a container of its own — the
 * connection feed uses one for the statements currently held for approval, so
 * that container is present exactly while something is held. The rows stay in
 * one table, with one header and one set of columns.
 */
export interface RowGroup<T> {
  id: string;
  testId?: string;
  rows: T[];
}

interface DataTableProps<T> {
  columns: Column<T>[];
  /**
   * Every row, used for the loading/empty decision and rendered as a single
   * body unless `rowGroups` splits it.
   */
  data: T[];
  isLoading?: boolean;
  emptyMessage?: string;
  onRowClick?: (item: T) => void;
  /** Row link target; return undefined for a row that has nothing to link to. */
  rowHref?: (item: T) => string | undefined;
  rowKey: (item: T) => string;
  /** Extra classes for one row — highlighting, emphasis, state. */
  rowClassName?: (item: T) => string | undefined;
  /** Per-row `data-testid`, for rows the E2E suite addresses individually. */
  rowTestId?: (item: T) => string | undefined;
  /** Optional split of `data` into consecutive `<tbody>` sections. */
  rowGroups?: RowGroup<T>[];
  className?: string;
}

export function DataTable<T>({
  columns,
  data,
  isLoading,
  emptyMessage = "No data found.",
  onRowClick,
  rowHref,
  rowKey,
  rowClassName,
  rowTestId,
  rowGroups,
  className,
}: DataTableProps<T>) {
  if (isLoading) {
    return (
      <div className={cn("rounded-md border", className)}>
        <Table>
          <TableHeader>
            <TableRow>
              {columns.map((col) => (
                <TableHead key={col.key} className={col.className}>
                  {col.header}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {Array.from({ length: 5 }).map((_, i) => (
              <TableRow key={i}>
                {columns.map((col) => (
                  <TableCell key={col.key} className={col.className}>
                    <Skeleton className="h-4 w-full" />
                  </TableCell>
                ))}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    );
  }

  if (data.length === 0) {
    return (
      <div className={cn("rounded-md border", className)}>
        <Table>
          <TableHeader>
            <TableRow>
              {columns.map((col) => (
                <TableHead key={col.key} className={col.className}>
                  {col.header}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableRow>
              <TableCell
                colSpan={columns.length}
                className="h-24 text-center text-muted-foreground"
              >
                {emptyMessage}
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </div>
    );
  }

  const groups: RowGroup<T>[] = rowGroups ?? [{ id: "rows", rows: data }];

  const renderRow = (item: T) => {
    const href = rowHref?.(item);
    const isClickable = !!href || !!onRowClick;
    return (
      <TableRow
        key={rowKey(item)}
        data-testid={rowTestId?.(item)}
        className={cn(
          isClickable && "cursor-pointer",
          href && "relative",
          rowClassName?.(item),
        )}
        onClick={!href && onRowClick ? () => onRowClick(item) : undefined}
      >
        {columns.map((col, colIdx) => (
          <TableCell key={col.key} className={col.className}>
            {colIdx === 0 && href ? (
              <Link to={href} className="after:absolute after:inset-0">
                {col.cell(item)}
              </Link>
            ) : (
              col.cell(item)
            )}
          </TableCell>
        ))}
      </TableRow>
    );
  };

  return (
    <div className={cn("rounded-md border", className)}>
      <Table>
        <TableHeader>
          <TableRow>
            {columns.map((col) => (
              <TableHead key={col.key} className={col.className}>
                {col.header}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        {groups.map((group) => (
          <TableBody key={group.id} data-testid={group.testId}>
            {group.rows.map(renderRow)}
          </TableBody>
        ))}
      </Table>
    </div>
  );
}
