import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

/** An extra breadcrumb crumb a page can inject ahead of its own leaf crumb. */
export interface BreadcrumbExtraItem {
  title: string;
  href?: string;
}

/** Everything a page can publish for a given pathname's breadcrumb. */
export interface BreadcrumbEntry {
  /** Leaf-crumb title override (e.g. a SQL preview). */
  title?: string;
  /** Extra crumbs inserted before the leaf crumb (e.g. an owning connection). */
  parents?: BreadcrumbExtraItem[];
}

/**
 * A partial update to a pathname's `BreadcrumbEntry`. Only the keys present
 * on the patch are touched — this lets independent publishers (e.g. a title
 * override and a parents override) target the same pathname without
 * clobbering each other's contribution.
 */
type BreadcrumbPatch = Partial<BreadcrumbEntry>;

interface BreadcrumbContextType {
  /** Map of route pathname → published breadcrumb entry for that path. */
  entries: Record<string, BreadcrumbEntry>;
  /**
   * Merge `patch` into the breadcrumb entry for `pathname`, touching only
   * the keys present on `patch`. Pass `{ title: undefined }` /
   * `{ parents: undefined }` to clear just that field. Used by detail pages
   * to publish a human-friendly crumb and/or extra parent crumbs that aren't
   * known at route-context time.
   */
  setBreadcrumb: (pathname: string, patch: BreadcrumbPatch) => void;
}

const BreadcrumbContext = createContext<BreadcrumbContextType | undefined>(
  undefined,
);

function parentsEqual(
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

/** Value-compares only the keys present on `patch` against `entry`. */
function patchAppliesNoChange(
  entry: BreadcrumbEntry | undefined,
  patch: BreadcrumbPatch,
): boolean {
  if ("title" in patch && entry?.title !== patch.title) return false;
  if ("parents" in patch && !parentsEqual(entry?.parents, patch.parents))
    return false;
  return true;
}

export function BreadcrumbProvider({ children }: { children: ReactNode }) {
  const [entries, setEntries] = useState<Record<string, BreadcrumbEntry>>({});

  const setBreadcrumb = useCallback(
    (pathname: string, patch: BreadcrumbPatch) => {
      setEntries((prev) => {
        const current = prev[pathname];
        if (patchAppliesNoChange(current, patch)) return prev;

        const merged: BreadcrumbEntry = { ...current, ...patch };
        const isEmpty =
          merged.title === undefined && (merged.parents?.length ?? 0) === 0;

        if (isEmpty) {
          if (!current) return prev;
          const next = { ...prev };
          delete next[pathname];
          return next;
        }
        return { ...prev, [pathname]: merged };
      });
    },
    [],
  );

  const value = useMemo(
    () => ({ entries, setBreadcrumb }),
    [entries, setBreadcrumb],
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
 * Publish a breadcrumb patch for `pathname` while the calling component is
 * mounted, clearing the same fields automatically on unmount. `patch` must
 * consistently touch the same set of keys across renders (the wrapper hooks
 * below guarantee this). Callers may pass a fresh object/array literal each
 * render — patches are compared by value via a ref, not by reference, so
 * this does not cause an update loop (verified under React StrictMode's
 * double-invoked effects).
 */
function useBreadcrumbPatch(pathname: string, patch: BreadcrumbPatch): void {
  const { setBreadcrumb } = useBreadcrumbContext();
  const lastPublished = useRef<BreadcrumbPatch | null>(null);

  useEffect(() => {
    if (
      lastPublished.current === null ||
      !patchAppliesNoChange(
        // Compare as if `lastPublished` were a full entry containing only
        // the patched keys, against the new patch.
        lastPublished.current as BreadcrumbEntry,
        patch,
      )
    ) {
      lastPublished.current = patch;
      setBreadcrumb(pathname, patch);
    }
    return () => {
      const clear: BreadcrumbPatch = {};
      for (const key of Object.keys(patch) as (keyof BreadcrumbEntry)[]) {
        clear[key] = undefined;
      }
      lastPublished.current = null;
      setBreadcrumb(pathname, clear);
    };
  }, [pathname, patch, setBreadcrumb]);
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
  useBreadcrumbPatch(pathname, { title });
}

/**
 * Publish extra breadcrumb crumbs for `pathname` while the calling component
 * is mounted, clearing them automatically on unmount. `Header.tsx` inserts
 * these items before the leaf crumb for `pathname`. Pass `undefined` (or an
 * empty array) while data is loading to leave the default crumbs in place.
 */
export function useBreadcrumbItems(
  pathname: string,
  items: BreadcrumbExtraItem[] | undefined,
): void {
  useBreadcrumbPatch(pathname, { parents: items });
}
