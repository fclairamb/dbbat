import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useQueryDetails, useQueryRows } from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { PageLoader, LoadingSpinner } from "@/components/shared/LoadingSpinner";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ChevronLeft, ChevronRight, ChevronsLeft } from "lucide-react";
import { format } from "date-fns";
import { useAuth } from "@/contexts/AuthContext";
import { canViewQueries } from "@/lib/permissions";
import { AccessDenied } from "@/components/shared/AccessDenied";

const DEFAULT_PAGE_SIZE = 100;
const PAGE_SIZE_OPTIONS = [50, 100, 500];

export const Route = createFileRoute("/_authenticated/queries/$uid")({
  component: QueryDetailPage,
});

function QueryDetailPage() {
  const { user } = useAuth();
  const { uid } = Route.useParams();
  const { data: query, isLoading: isLoadingQuery } = useQueryDetails(uid);

  // Rows pagination state (local, not URL-based)
  const [cursor, setCursor] = useState<string | undefined>();
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);

  const { data: rowsData, isLoading: isLoadingRows } = useQueryRows(uid, {
    cursor,
    limit: pageSize,
  });

  const goNext = () => {
    if (rowsData?.next_cursor) {
      setCursorStack((prev) => [...prev, cursor ?? ""]);
      setCursor(rowsData.next_cursor);
    }
  };

  const goPrevious = () => {
    setCursorStack((prev) => {
      const next = [...prev];
      const prevCursor = next.pop();
      setCursor(prevCursor || undefined);
      return next;
    });
  };

  const goFirst = () => {
    setCursorStack([]);
    setCursor(undefined);
  };

  const changePageSize = (newSize: number) => {
    setPageSize(newSize);
    setCursor(undefined);
    setCursorStack([]);
  };

  if (!canViewQueries(user?.roles)) {
    return <AccessDenied requiredRole="viewer" />;
  }

  if (isLoadingQuery) {
    return <PageLoader />;
  }

  if (!query) {
    return (
      <div className="text-center text-muted-foreground py-12">
        Query not found
      </div>
    );
  }

  // Compute row position indicator
  const firstRowNum = rowsData?.rows?.[0]?.row_number;
  const lastRowNum = rowsData?.rows?.[rowsData.rows.length - 1]?.row_number;
  const totalRows = rowsData?.total_rows ?? 0;
  const hasPrevious = cursorStack.length > 0;
  const hasNext = rowsData?.has_more ?? false;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Query Details"
        description={`Executed ${format(new Date(query.executed_at), "PPpp")}`}
      />

      <div className="grid gap-6 md:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Query Information</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <div>
              <div className="text-sm font-medium text-muted-foreground mb-1">
                Status
              </div>
              {query.error ? (
                <Badge variant="destructive">Error</Badge>
              ) : (
                <Badge variant="secondary">Success</Badge>
              )}
            </div>
            <div>
              <div className="text-sm font-medium text-muted-foreground mb-1">
                Duration
              </div>
              <div>
                {query.duration_ms != null
                  ? `${query.duration_ms.toFixed(2)}ms`
                  : "-"}
              </div>
            </div>
            <div>
              <div className="text-sm font-medium text-muted-foreground mb-1">
                Rows Affected
              </div>
              <div>{query.rows_affected ?? "-"}</div>
            </div>
            {query.error && (
              <div>
                <div className="text-sm font-medium text-muted-foreground mb-1">
                  Error
                </div>
                <div className="text-destructive text-sm">{query.error}</div>
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>SQL</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="bg-muted p-4 rounded-md overflow-x-auto text-sm font-mono whitespace-pre-wrap">
              {query.sql_text}
            </pre>
          </CardContent>
        </Card>
      </div>

      {query.parameters?.values && query.parameters.values.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle>Parameters</CardTitle>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-20">Index</TableHead>
                  <TableHead>Value</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {query.parameters.values.map((value, i) => (
                  <TableRow key={i}>
                    <TableCell className="font-mono">${i + 1}</TableCell>
                    <TableCell className="font-mono">{value}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>
            Result Rows
            {totalRows > 0 && ` (${totalRows})`}
          </CardTitle>
        </CardHeader>
        <CardContent>
          {isLoadingRows ? (
            <div className="flex justify-center py-8">
              <LoadingSpinner />
            </div>
          ) : rowsData && rowsData.rows.length > 0 ? (
            <div className="space-y-4">
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-16">#</TableHead>
                      {Object.keys(rowsData.rows[0].row_data).map((key) => (
                        <TableHead key={key}>{key}</TableHead>
                      ))}
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {rowsData.rows.map((row) => (
                      <TableRow key={row.row_number}>
                        <TableCell className="text-muted-foreground">
                          {row.row_number}
                        </TableCell>
                        {Object.values(row.row_data).map((value, i) => (
                          <TableCell key={i} className="font-mono text-sm">
                            {formatValue(value)}
                          </TableCell>
                        ))}
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>

              {/* Pagination controls */}
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2 text-sm text-muted-foreground">
                  <span>Rows per page:</span>
                  {PAGE_SIZE_OPTIONS.map((opt) => (
                    <Button
                      key={opt}
                      variant={opt === pageSize ? "secondary" : "ghost"}
                      size="sm"
                      className="h-7 px-2"
                      onClick={() => changePageSize(opt)}
                    >
                      {opt}
                    </Button>
                  ))}
                </div>

                <div className="flex items-center gap-4">
                  {firstRowNum != null && lastRowNum != null && totalRows > 0 && (
                    <span className="text-sm text-muted-foreground">
                      Rows {firstRowNum}-{lastRowNum} of {totalRows}
                    </span>
                  )}

                  <div className="flex items-center gap-1">
                    {hasPrevious && (
                      <>
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={goFirst}
                          title="First page"
                        >
                          <ChevronsLeft className="h-4 w-4" />
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={goPrevious}
                        >
                          <ChevronLeft className="h-4 w-4 mr-1" />
                          Previous
                        </Button>
                      </>
                    )}
                    {hasNext && (
                      <Button variant="outline" size="sm" onClick={goNext}>
                        Next
                        <ChevronRight className="h-4 w-4 ml-1" />
                      </Button>
                    )}
                  </div>
                </div>
              </div>
            </div>
          ) : (
            <div className="text-center text-muted-foreground py-4">
              No result rows
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function formatValue(value: unknown): string {
  if (value === null) return "NULL";
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}
