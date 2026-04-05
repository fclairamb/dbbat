# Adaptive Refresh Component

## Overview

Implement a reusable refresh-controlling component that provides both manual refresh and adaptive auto-refresh functionality. The component dynamically adjusts refresh intervals based on data changes to optimize network usage while maintaining responsiveness.

## Goals

- Allow users to instantly refresh the current view
- Automatically adjust refresh intervals based on data activity
- Display countdown to next refresh
- Reduce unnecessary network requests during idle periods
- Quickly increase refresh rate when new data is detected

## Component Design

### AdaptiveRefresh Component

```tsx
interface AdaptiveRefreshProps {
  onRefresh: () => Promise<void>;
  hasNewData: boolean;           // Signal from parent when new data detected
  minInterval?: number;          // Minimum refresh interval (default: 5s)
  maxInterval?: number;          // Maximum refresh interval (default: 60s)
  initialInterval?: number;      // Starting interval (default: 10s)
  enabled?: boolean;             // Enable/disable auto-refresh (default: true)
}
```

### Adaptive Algorithm

1. **When new data is detected**: Reduce interval by 50% (but not below `minInterval`)
2. **When no new data**: Increase interval by 10% (but not above `maxInterval`)
3. **Manual refresh**: Reset interval to `initialInterval`

```
Example flow (initialInterval: 10s, min: 5s, max: 60s):

Refresh #1: 10s interval → no new data → next: 11s
Refresh #2: 11s interval → no new data → next: 12.1s
Refresh #3: 12.1s interval → NEW DATA → next: 6.05s
Refresh #4: 6.05s interval → NEW DATA → next: 5s (capped at min)
Refresh #5: 5s interval → no new data → next: 5.5s
...
```

### Visual Design

```
┌─────────────────────────────────────────────────┐
│  ↻ Refresh    Auto-refresh: ON ▼    Next: 8s   │
└─────────────────────────────────────────────────┘
```

Components:
- **Refresh button**: Manual instant refresh (↻ icon)
- **Auto-refresh toggle**: Dropdown or toggle to enable/disable
- **Countdown display**: Shows seconds until next refresh (updates every second)

When auto-refresh is disabled:
```
┌─────────────────────────────────────────────────┐
│  ↻ Refresh    Auto-refresh: OFF ▼              │
└─────────────────────────────────────────────────┘
```

During refresh:
```
┌─────────────────────────────────────────────────┐
│  ⟳ Refreshing...    Auto-refresh: ON ▼         │
└─────────────────────────────────────────────────┘
```

## Implementation

### Hook: useAdaptiveRefresh

```tsx
interface UseAdaptiveRefreshOptions {
  onRefresh: () => Promise<void>;
  minInterval?: number;
  maxInterval?: number;
  initialInterval?: number;
}

interface UseAdaptiveRefreshResult {
  refresh: () => Promise<void>;        // Trigger manual refresh
  isRefreshing: boolean;               // Loading state
  secondsUntilRefresh: number;         // Countdown value
  autoRefreshEnabled: boolean;         // Current auto-refresh state
  setAutoRefreshEnabled: (enabled: boolean) => void;
  reportNewData: (hasNew: boolean) => void;  // Signal new data detected
  currentInterval: number;             // Current interval for debugging
}

function useAdaptiveRefresh(options: UseAdaptiveRefreshOptions): UseAdaptiveRefreshResult;
```

### Integration Points

The component should integrate with:

1. **Connections list** (`/app/connections`)
2. **Queries list** (`/app/queries`)
3. **Audit logs list** (`/app/audit`)

### Detecting New Data

Each view determines "new data" differently:

- **Connections**: Compare latest `connected_at` timestamp or total count
- **Queries**: Compare latest `started_at` timestamp or total count
- **Audit logs**: Compare latest `created_at` timestamp or total count

Implementation approach:
```tsx
// In the parent component
const previousLatestId = useRef<string | null>(null);

const handleRefresh = async () => {
  const data = await fetchData();
  const hasNewData = data[0]?.uid !== previousLatestId.current;
  previousLatestId.current = data[0]?.uid ?? null;
  adaptiveRefresh.reportNewData(hasNewData);
};
```

## User Preferences

Store auto-refresh preference in localStorage per view:
- `dbbat.autoRefresh.connections`
- `dbbat.autoRefresh.queries`
- `dbbat.autoRefresh.audit`

Default: enabled

## Default Values

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| `minInterval` | 5s | Fast enough for active monitoring |
| `maxInterval` | 60s | Reasonable cap for idle periods |
| `initialInterval` | 10s | Balanced starting point |

## Edge Cases

1. **Tab not visible**: Pause auto-refresh when tab is hidden (use `document.hidden`)
2. **Network error**: Keep current interval, don't penalize for network issues
3. **Rapid new data**: Cap at `minInterval` to prevent overwhelming the server
4. **Manual refresh during countdown**: Reset countdown and interval to initial

## Accessibility

- Refresh button should be keyboard accessible
- Countdown should be announced to screen readers on significant changes
- Loading state should be indicated with `aria-busy`

## Future Considerations

- Per-user server-side preference storage
- WebSocket-based push notifications (eliminates polling entirely)
- Configurable intervals via UI settings
