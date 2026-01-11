import { RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import {
  useAdaptiveRefresh,
  type RefreshResult,
} from "@/hooks/use-adaptive-refresh";

export interface AdaptiveRefreshProps {
  onRefresh: () => Promise<RefreshResult>;
  storageKey?: string;
  minInterval?: number;
  maxInterval?: number;
  initialInterval?: number;
  className?: string;
}

export function AdaptiveRefresh({
  onRefresh,
  storageKey,
  minInterval,
  maxInterval,
  initialInterval,
  className,
}: AdaptiveRefreshProps) {
  const {
    refresh,
    isRefreshing,
    secondsUntilRefresh,
    autoRefreshEnabled,
    setAutoRefreshEnabled,
  } = useAdaptiveRefresh({
    onRefresh,
    storageKey,
    minInterval,
    maxInterval,
    initialInterval,
  });

  const handleManualRefresh = () => {
    refresh(true); // Manual refresh
  };

  const toggleAutoRefresh = () => {
    setAutoRefreshEnabled(!autoRefreshEnabled);
  };

  return (
    <div className={cn("flex items-center gap-2", className)}>
      <Button
        variant="outline"
        size="sm"
        onClick={handleManualRefresh}
        disabled={isRefreshing}
        className="gap-2"
        data-testid="refresh-button"
      >
        <RefreshCw
          className={cn("h-4 w-4", isRefreshing && "animate-spin")}
        />
        <span>Refresh</span>
      </Button>

      <Badge
        variant="outline"
        className={cn(
          "cursor-pointer select-none transition-colors hover:bg-accent",
          autoRefreshEnabled ? "text-foreground" : "text-muted-foreground"
        )}
        onClick={toggleAutoRefresh}
      >
        {autoRefreshEnabled && !isRefreshing
          ? `Next: ${secondsUntilRefresh}s`
          : "Auto-refresh: OFF"}
      </Badge>
    </div>
  );
}
