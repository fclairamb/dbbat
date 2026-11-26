import { Fragment } from "react";
import { Link, useRouterState } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";

import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";
import {
  useBreadcrumbItems,
  useBreadcrumbTitle,
  type BreadcrumbExtraItem,
} from "@/contexts/BreadcrumbContext";
import { cn } from "@/lib/utils";

interface DetailHeaderProps {
  /** The list this record belongs to: the way back. */
  backTo: string;
  backLabel: string;
  /** Crumbs between the list and the record (e.g. a query's connection). */
  parents?: BreadcrumbExtraItem[];
  /** The record's name. Never a uid: pick the name, or a readable label. */
  title: string;
  /** Status pill, type badge: shown next to the title. */
  badges?: React.ReactNode;
  /** Muted line of facts under the title. */
  meta?: React.ReactNode;
  /** At most two visible buttons; anything more goes in a ⋯ menu. */
  actions?: React.ReactNode;
  className?: string;
}

/** The deepest matched pathname, the key the top-bar breadcrumb reads. */
export function useCurrentPathname(): string {
  return useRouterState({
    select: (s) => s.matches[s.matches.length - 1]?.pathname ?? "/",
  });
}

/**
 * Header of every detail and edit page: a breadcrumb line ("← Users › alice")
 * that is the way back, then the title row with the record's name, its badges
 * and actions. It also publishes the name (and parents) to the top-bar
 * breadcrumb, so the top bar never shows a uid. See /design-reference.
 */
export function DetailHeader(props: DetailHeaderProps) {
  const pathname = useCurrentPathname();
  useBreadcrumbTitle(pathname, props.title);
  useBreadcrumbItems(
    pathname,
    props.parents?.length ? props.parents : undefined,
  );
  return <DetailHeaderView {...props} />;
}

/**
 * The markup alone, publishing nothing: for the design reference, whose own
 * breadcrumb must not take the example's name.
 */
export function DetailHeaderView({
  backTo,
  backLabel,
  parents,
  title,
  badges,
  meta,
  actions,
  className,
}: DetailHeaderProps) {
  return (
    <div className={cn("space-y-3", className)} data-testid="detail-header">
      {/* Its own label: the top bar's trail is the one named "breadcrumb". */}
      <Breadcrumb aria-label="Back to list">
        <BreadcrumbList className="flex-nowrap gap-1.5 sm:gap-1.5">
          <BreadcrumbItem className="shrink-0">
            <BreadcrumbLink asChild>
              <Link
                to={backTo}
                className="inline-flex items-center gap-1"
                data-testid="back-to-list"
              >
                <ArrowLeft className="h-3.5 w-3.5" aria-hidden="true" />
                {backLabel}
              </Link>
            </BreadcrumbLink>
          </BreadcrumbItem>
          {parents?.map((p) => (
            <Fragment key={p.href ?? p.title}>
              <BreadcrumbSeparator className="hidden sm:block" />
              <BreadcrumbItem className="hidden min-w-0 sm:inline-flex">
                {p.href ? (
                  <BreadcrumbLink asChild>
                    <Link to={p.href} className="truncate">
                      {p.title}
                    </Link>
                  </BreadcrumbLink>
                ) : (
                  <span className="truncate">{p.title}</span>
                )}
              </BreadcrumbItem>
            </Fragment>
          ))}
          <BreadcrumbSeparator />
          <BreadcrumbItem className="min-w-0">
            <BreadcrumbPage className="truncate text-foreground/70">
              {title}
            </BreadcrumbPage>
          </BreadcrumbItem>
        </BreadcrumbList>
      </Breadcrumb>
      <div className="flex flex-wrap items-start gap-3">
        <div className="min-w-0 flex-1 basis-64">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
            <h1
              className="min-w-0 max-w-full truncate text-xl font-semibold tracking-tight sm:text-2xl"
              title={title}
            >
              {title}
            </h1>
            {badges}
          </div>
          {meta && (
            <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-muted-foreground">
              {meta}
            </div>
          )}
        </div>
        {actions && (
          <div className="flex shrink-0 flex-wrap items-center gap-2">
            {actions}
          </div>
        )}
      </div>
    </div>
  );
}
