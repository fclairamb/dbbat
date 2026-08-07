import { Badge } from "@/components/ui/badge";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

// formatControlName mirrors the helper on the Grants page: "block_ddl" ->
// "Block Ddl". Duplicated rather than imported because the Grants page's
// version lives in a route file, not a shared module.
function formatControlName(control: string): string {
  return control
    .replace(/_/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

/**
 * Compact R/W vs R/O indicator derived from a grant's controls, for dense
 * rows (the connections list) where the full per-control badges used on the
 * Grants page would be too wide. Hovering reveals the actual controls.
 */
export function GrantAccessChip({ controls }: { controls: string[] }) {
  const isReadOnly = controls.includes("read_only");

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge
          variant={isReadOnly ? "secondary" : "default"}
          className="font-mono"
          data-testid="grant-access-chip"
        >
          {isReadOnly ? "R/O" : "R/W"}
        </Badge>
      </TooltipTrigger>
      <TooltipContent>
        {controls.length === 0
          ? "Full access (no controls)"
          : controls.map(formatControlName).join(", ")}
      </TooltipContent>
    </Tooltip>
  );
}
