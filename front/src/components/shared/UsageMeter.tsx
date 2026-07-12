import { cn } from "@/lib/utils";

// Warning / destructive thresholds (as a fraction of the limit).
const WARN_RATIO = 0.8;

export interface UsageMeterProps {
  /** Current consumption. */
  used: number;
  /** Configured limit; null/undefined means unlimited. */
  limit?: number | null;
  /** How to render the numeric values (defaults to plain integers). */
  format?: (n: number) => string;
  /** Suffix label, e.g. "queries". */
  unit?: string;
  className?: string;
}

// UsageMeter renders "used / limit unit" with a thin progress bar. It always
// shows the limit state: an explicit "unlimited" marker when no limit is set,
// a warning color at >=80% and a destructive color when the quota is
// met/exceeded, so an over-quota grant is visible at a glance.
export function UsageMeter({
  used,
  limit,
  format = (n) => String(n),
  unit,
  className,
}: UsageMeterProps) {
  const suffix = unit ? ` ${unit}` : "";

  if (limit == null) {
    return (
      <div className={cn("text-sm", className)}>
        <div>
          {format(used)}
          {suffix}
        </div>
        <div className="text-xs text-muted-foreground italic">unlimited</div>
      </div>
    );
  }

  const ratio = limit > 0 ? used / limit : used > 0 ? Infinity : 0;
  const pct = Math.min(100, Math.max(0, ratio * 100));
  const over = ratio >= 1;
  const warn = ratio >= WARN_RATIO;

  const textColor = over
    ? "text-destructive"
    : warn
      ? "text-amber-600 dark:text-amber-500"
      : "";
  const barColor = over
    ? "bg-destructive"
    : warn
      ? "bg-amber-500"
      : "bg-primary";

  return (
    <div className={cn("text-sm space-y-1", className)}>
      <div className={cn("tabular-nums", textColor)}>
        {format(used)} / {format(limit)}
        {suffix}
        {over && (
          <span className="ml-1 font-medium">
            ({limit > 0 ? Math.round(ratio * 100) : "∞"}%)
          </span>
        )}
      </div>
      <div
        className="h-1.5 w-full max-w-[8rem] overflow-hidden rounded-full bg-muted"
        role="progressbar"
        aria-valuenow={Math.round(pct)}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        <div
          className={cn("h-full rounded-full transition-all", barColor)}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}

// UsageLimit renders just a configured limit (no live usage), used where only
// the quota is known — e.g. grant definitions. Shows an explicit "unlimited"
// marker when unset so it is never blank.
export function UsageLimit({
  limit,
  format = (n) => String(n),
  unit,
  className,
}: {
  limit?: number | null;
  format?: (n: number) => string;
  unit?: string;
  className?: string;
}) {
  if (limit == null) {
    return (
      <span className={cn("text-xs text-muted-foreground italic", className)}>
        unlimited
      </span>
    );
  }
  return (
    <span className={cn("tabular-nums", className)}>
      {format(limit)}
      {unit ? ` ${unit}` : ""}
    </span>
  );
}
