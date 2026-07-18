import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

/** An extra breadcrumb crumb a page can inject ahead of its own leaf crumb. */
export interface BreadcrumbExtraItem {
  title: string;
  href?: string;
}

interface BreadcrumbContextType {
  /** Map of route pathname → override title for that crumb. */
  titles: Record<string, string>;
  /**
   * Set (or, when `title` is undefined, clear) the breadcrumb title override
   * for a given pathname. Used by detail pages to publish a human-friendly
   * crumb (e.g. a SQL preview) that isn't known at route-context time.
   */
  setBreadcrumbTitle: (pathname: string, title: string | undefined) => void;
  /** Map of route pathname → extra crumbs to insert before that path's leaf crumb. */
  items: Record<string, BreadcrumbExtraItem[]>;
  /**
   * Set (or, when `items` is undefined/empty, clear) extra breadcrumb items
   * for a given pathname. Used by detail pages to inject a parent crumb that
   * isn't derivable from the URL (e.g. a query's owning connection).
   */
  setBreadcrumbItems: (
    pathname: string,
    items: BreadcrumbExtraItem[] | undefined,
  ) => void;
}

const BreadcrumbContext = createContext<BreadcrumbContextType | undefined>(
  undefined,
);

function itemsEqual(
  a: BreadcrumbExtraItem[] | undefined,
  b: BreadcrumbExtraItem[] | undefined,
): boolean {
  if (a === b) return true;
  if (!a || !b) return (a?.length ?? 0) === (b?.length ?? 0);
  if (a.length !== b.length) return false;
  return a.every(
    (item, i) => item.title === b[i].title && item.href === b[i].href,
  );
}

export function BreadcrumbProvider({ children }: { children: ReactNode }) {
  const [titles, setTitles] = useState<Record<string, string>>({});
  const [items, setItems] = useState<Record<string, BreadcrumbExtraItem[]>>({});

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
    [],
  );

  const setBreadcrumbItems = useCallback(
    (pathname: string, newItems: BreadcrumbExtraItem[] | undefined) => {
      setItems((prev) => {
        const hasNew = newItems !== undefined && newItems.length > 0;
        if (!hasNew) {
          if (!(pathname in prev)) return prev;
          const next = { ...prev };
          delete next[pathname];
          return next;
        }
        if (itemsEqual(prev[pathname], newItems)) return prev;
        return { ...prev, [pathname]: newItems };
      });
    },
    [],
  );

  const value = useMemo(
    () => ({ titles, setBreadcrumbTitle, items, setBreadcrumbItems }),
    [titles, setBreadcrumbTitle, items, setBreadcrumbItems],
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
      "useBreadcrumbContext must be used within a BreadcrumbProvider",
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
  title: string | undefined,
): void {
  const { setBreadcrumbTitle } = useBreadcrumbContext();
  useEffect(() => {
    setBreadcrumbTitle(pathname, title);
    return () => setBreadcrumbTitle(pathname, undefined);
  }, [pathname, title, setBreadcrumbTitle]);
}

/**
 * Publish extra breadcrumb crumbs for `pathname` while the calling component
 * is mounted, clearing them automatically on unmount. `Header.tsx` inserts
 * these items before the leaf crumb for `pathname`. Pass `undefined` (or an
 * empty array) while data is loading to leave the default crumbs in place.
 *
 * Callers may pass a fresh array literal each render — items are compared by
 * value, not reference, so this does not cause an update loop.
 */
export function useBreadcrumbItems(
  pathname: string,
  items: BreadcrumbExtraItem[] | undefined,
): void {
  const { setBreadcrumbItems } = useBreadcrumbContext();
  useEffect(() => {
    setBreadcrumbItems(pathname, items);
    return () => setBreadcrumbItems(pathname, undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- items compared by value via JSON.stringify, not reference
  }, [pathname, JSON.stringify(items), setBreadcrumbItems]);
}
