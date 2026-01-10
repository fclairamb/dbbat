import { useState, useEffect, useCallback, useRef } from "react";

export interface RefreshResult {
  hasNewData: boolean;
}

export interface UseAdaptiveRefreshOptions {
  onRefresh: () => Promise<RefreshResult>;
  storageKey?: string;
  minInterval?: number;
  maxInterval?: number;
  initialInterval?: number;
}

export interface UseAdaptiveRefreshResult {
  refresh: (isManual?: boolean) => Promise<void>;
  isRefreshing: boolean;
  secondsUntilRefresh: number;
  autoRefreshEnabled: boolean;
  setAutoRefreshEnabled: (enabled: boolean) => void;
  currentInterval: number;
}

const DEFAULT_MIN_INTERVAL = 5000; // 5 seconds
const DEFAULT_MAX_INTERVAL = 60000; // 60 seconds
const DEFAULT_INITIAL_INTERVAL = 10000; // 10 seconds

export function useAdaptiveRefresh({
  onRefresh,
  storageKey,
  minInterval = DEFAULT_MIN_INTERVAL,
  maxInterval = DEFAULT_MAX_INTERVAL,
  initialInterval = DEFAULT_INITIAL_INTERVAL,
}: UseAdaptiveRefreshOptions): UseAdaptiveRefreshResult {
  const [currentInterval, setCurrentInterval] = useState(initialInterval);
  const [secondsUntilRefresh, setSecondsUntilRefresh] = useState(
    Math.ceil(initialInterval / 1000)
  );
  const [autoRefreshEnabled, setAutoRefreshEnabledState] = useState(() => {
    // Load from localStorage on initialization
    if (storageKey) {
      try {
        const stored = localStorage.getItem(storageKey);
        if (stored) {
          const parsed = JSON.parse(stored);
          return parsed.enabled ?? true;
        }
      } catch (error) {
        console.error("Failed to load auto-refresh preference:", error);
      }
    }
    return true;
  });
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [isTabVisible, setIsTabVisible] = useState(!document.hidden);

  // Track if component is mounted to avoid state updates after unmount
  const isMountedRef = useRef(true);
  // Store current interval in ref for use in callbacks without stale closure
  const currentIntervalRef = useRef(currentInterval);
  // Track refreshing state in ref to avoid recreating callback when isRefreshing changes
  const isRefreshingRef = useRef(false);
  // Store callbacks in refs to avoid recreating refresh when they change
  const onRefreshRef = useRef(onRefresh);
  const resetIntervalRef = useRef<() => void>(() => {});
  const adjustIntervalRef = useRef<(hasNewData: boolean) => void>(() => {});

  // Keep refs in sync with state
  useEffect(() => {
    currentIntervalRef.current = currentInterval;
  }, [currentInterval]);

  useEffect(() => {
    onRefreshRef.current = onRefresh;
  }, [onRefresh]);

  // Save preference to localStorage when changed
  useEffect(() => {
    if (storageKey) {
      try {
        localStorage.setItem(
          storageKey,
          JSON.stringify({ enabled: autoRefreshEnabled })
        );
      } catch (error) {
        console.error("Failed to save auto-refresh preference:", error);
      }
    }
  }, [storageKey, autoRefreshEnabled]);

  // Listen for page visibility changes
  useEffect(() => {
    const handleVisibilityChange = () => {
      setIsTabVisible(!document.hidden);
    };

    document.addEventListener("visibilitychange", handleVisibilityChange);

    return () => {
      document.removeEventListener("visibilitychange", handleVisibilityChange);
    };
  }, []);

  // Adjust interval based on whether new data was detected
  const adjustInterval = useCallback(
    (hasNewData: boolean) => {
      const current = currentIntervalRef.current;
      let newInterval: number;

      if (hasNewData) {
        // New data detected: reduce interval by 50%
        newInterval = Math.max(current * 0.5, minInterval);
      } else {
        // No new data: increase interval by 10%
        newInterval = Math.min(current * 1.1, maxInterval);
      }

      setCurrentInterval(newInterval);
      setSecondsUntilRefresh(Math.ceil(newInterval / 1000));
    },
    [minInterval, maxInterval]
  );

  // Reset interval to initial (for manual refresh)
  const resetInterval = useCallback(() => {
    setCurrentInterval(initialInterval);
    setSecondsUntilRefresh(Math.ceil(initialInterval / 1000));
  }, [initialInterval]);

  // Keep refs in sync with callbacks
  useEffect(() => {
    resetIntervalRef.current = resetInterval;
  }, [resetInterval]);

  useEffect(() => {
    adjustIntervalRef.current = adjustInterval;
  }, [adjustInterval]);

  // Refresh function - handles both manual and auto refresh
  // Use refs for all dependencies to ensure callback stability
  const refresh = useCallback(async (isManual: boolean = true) => {
    // Use ref for guard to avoid recreating callback when isRefreshing changes
    if (isRefreshingRef.current) {
      return;
    }

    isRefreshingRef.current = true;
    setIsRefreshing(true);

    try {
      const result = await onRefreshRef.current();

      if (isMountedRef.current) {
        if (isManual) {
          // Manual refresh: reset interval to initial
          resetIntervalRef.current();
        } else {
          // Auto refresh: adjust interval based on data activity
          adjustIntervalRef.current(result.hasNewData);
        }
      }
    } catch (error) {
      console.error("Refresh failed:", error);
      // Network error: keep current interval, don't penalize
      // Just reset the countdown without changing interval
      if (isMountedRef.current) {
        setSecondsUntilRefresh(Math.ceil(currentIntervalRef.current / 1000));
      }
    } finally {
      isRefreshingRef.current = false;
      if (isMountedRef.current) {
        setIsRefreshing(false);
      }
    }
  }, []); // Empty deps - all dependencies are refs now

  // Countdown timer (1 second interval)
  useEffect(() => {
    if (!autoRefreshEnabled || !isTabVisible) {
      return;
    }

    const intervalId = setInterval(() => {
      setSecondsUntilRefresh((prev) => {
        // Count down to 0 (don't reset here - let refresh effect handle it)
        return Math.max(0, prev - 1);
      });
    }, 1000);

    return () => {
      clearInterval(intervalId);
    };
  }, [autoRefreshEnabled, isTabVisible]);

  // Trigger refresh when countdown reaches 0
  useEffect(() => {
    if (
      autoRefreshEnabled &&
      isTabVisible &&
      secondsUntilRefresh <= 0 &&
      !isRefreshing
    ) {
      // Auto refresh (not manual)
      refresh(false);
    }
  }, [secondsUntilRefresh, autoRefreshEnabled, isTabVisible, isRefreshing, refresh]);

  // Track mount/unmount
  useEffect(() => {
    isMountedRef.current = true;
    return () => {
      isMountedRef.current = false;
    };
  }, []);

  const setAutoRefreshEnabled = useCallback((enabled: boolean) => {
    setAutoRefreshEnabledState(enabled);
  }, []);

  return {
    refresh,
    isRefreshing,
    secondsUntilRefresh,
    autoRefreshEnabled,
    setAutoRefreshEnabled,
    currentInterval,
  };
}
