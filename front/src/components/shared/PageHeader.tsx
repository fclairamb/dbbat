import type { LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";

interface PageHeaderProps {
  /** The section's sidebar icon (`navItem(href).icon`), shown in the tile. */
  icon: LucideIcon;
  title: string;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
}

/**
 * Header of every list and section page: icon tile, title, one-line
 * description, and the page-level actions on the right. Detail and edit pages
 * use `DetailHeader` instead. See /design-reference.
 */
export function PageHeader({
  icon: Icon,
  title,
  description,
  actions,
  className,
}: PageHeaderProps) {
  return (
    // flex-wrap: on a phone the title block takes the whole row next to the
    // tile, so the actions drop below it instead of crushing the title. From
    // sm up they share the row, pushed right.
    <div
      className={cn("flex flex-wrap items-start gap-3", className)}
      data-testid="page-header"
    >
      <div
        className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-primary text-primary-foreground shadow-xs"
        data-testid="page-header-tile"
      >
        <Icon className="h-[18px] w-[18px]" />
      </div>
      <div className="min-w-0 flex-1 basis-[calc(100%-3rem)] sm:basis-40">
        <h1 className="text-xl font-semibold tracking-tight sm:text-2xl">
          {title}
        </h1>
        {description && (
          <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {actions && (
        <div className="flex max-w-full shrink-0 flex-wrap items-center gap-2 sm:ml-auto">
          {actions}
        </div>
      )}
    </div>
  );
}
