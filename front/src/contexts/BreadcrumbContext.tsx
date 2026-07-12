import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

interface BreadcrumbContextType {
  /** Map of route pathname → override title for that crumb. */
  titles: Record<string, string>;
  /**
   * Set (or, when `title` is undefined, clear) the breadcrumb title override
   * for a given pathname. Used by detail pages to publish a human-friendly
   * crumb (e.g. a SQL preview) that isn't known at route-context time.
   */
  setBreadcrumbTitle: (pathname: string, title: string | undefined) => void;
}

const BreadcrumbContext = createContext<BreadcrumbContextType | undefined>(
  undefined
);

export function BreadcrumbProvider({ children }: { children: ReactNode }) {
  const [titles, setTitles] = useState<Record<string, string>>({});

  const setBreadcrumbTitle = useCallback(
    (pathname: string, title: string | undefined) => {
      setTitles((prev) => {
        if (title === undefined) {
          if (!(pathname in prev)) return prev;
          const next = { ...prev };
          delete next[pathname];
          return next;
        }
        if (prev[pathname] === title) return prev;
        return { ...prev, [pathname]: title };
      });
    },
    []
  );

  const value = useMemo(
    () => ({ titles, setBreadcrumbTitle }),
    [titles, setBreadcrumbTitle]
  );

  return (
    <BreadcrumbContext.Provider value={value}>
      {children}
    </BreadcrumbContext.Provider>
  );
}

export function useBreadcrumbContext(): BreadcrumbContextType {
  const ctx = useContext(BreadcrumbContext);
  if (!ctx) {
    throw new Error(
      "useBreadcrumbContext must be used within a BreadcrumbProvider"
    );
  }
  return ctx;
}

/**
 * Publish a breadcrumb title override for `pathname` while the calling
 * component is mounted, clearing it automatically on unmount. Pass
 * `title === undefined` (e.g. while data is loading) to leave the default
 * crumb in place.
 */
export function useBreadcrumbTitle(
  pathname: string,
  title: string | undefined
): void {
  const { setBreadcrumbTitle } = useBreadcrumbContext();
  useEffect(() => {
    setBreadcrumbTitle(pathname, title);
    return () => setBreadcrumbTitle(pathname, undefined);
  }, [pathname, title, setBreadcrumbTitle]);
}
