import { Lock, ShieldAlert } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import {
  UPSTREAM_TLS_LABELS,
  upstreamTlsExplanation,
  upstreamTlsState,
} from "@/lib/upstream-tls";

interface UpstreamTlsIndicatorProps {
  /** `connections.upstream_tls` for this session. */
  upstreamTls: boolean | undefined;
  /** Protocol of the target server, when the caller's role can see it. */
  protocol?: string;
  /** Server ssl_mode policy, used only for the hover text. */
  sslMode?: string;
  className?: string;
}

/**
 * Renders whether a session's upstream leg was actually encrypted.
 *
 * Quiet when encrypted (a muted lock), loud only for an unexplained plaintext
 * leg — see `UpstreamTlsState` for why the other cases stay muted.
 *
 * No tooltip: in the connections list the row-wide link overlay sits on top
 * of every cell, so a hover card would never open. `title` is the best-effort
 * hover text, and the detail page spells the sentence out in visible copy.
 */
export function UpstreamTlsIndicator({
  upstreamTls,
  protocol,
  sslMode,
  className,
}: UpstreamTlsIndicatorProps) {
  const state = upstreamTlsState(upstreamTls, protocol);
  const label = UPSTREAM_TLS_LABELS[state];
  const explanation = upstreamTlsExplanation(state, sslMode);

  if (state === "plaintext") {
    return (
      <Badge
        variant="outline"
        data-testid="upstream-tls-indicator"
        data-state={state}
        title={explanation}
        aria-label={explanation}
        className={cn(
          "gap-1 border-amber-300 bg-amber-50 text-amber-700",
          "dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-400",
          className,
        )}
      >
        <ShieldAlert className="h-3 w-3" aria-hidden="true" />
        {label}
      </Badge>
    );
  }

  return (
    <span
      data-testid="upstream-tls-indicator"
      data-state={state}
      title={explanation}
      aria-label={explanation}
      className={cn(
        "inline-flex items-center gap-1 text-sm text-muted-foreground",
        className,
      )}
    >
      {state === "encrypted" && (
        <Lock className="h-3.5 w-3.5" aria-hidden="true" />
      )}
      {label}
    </span>
  );
}
