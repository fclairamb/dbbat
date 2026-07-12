import { SidebarTrigger } from "@/components/ui/sidebar";
import { Separator } from "@/components/ui/separator";
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";
import { Link, useMatches } from "@tanstack/react-router";
import { useBreadcrumbContext } from "@/contexts/BreadcrumbContext";

interface BreadcrumbItem {
  title: string;
  href?: string;
}

export function Header() {
  const matches = useMatches();
  const { titles } = useBreadcrumbContext();

  // Build breadcrumbs from the deepest matched route's pathname, split into
  // cumulative segments. Building from segments (rather than from the router
  // matches) guarantees a crumb for every path level — including parent
  // segments like "/queries" that have no layout route of their own.
  const breadcrumbs: BreadcrumbItem[] = [];
  const deepest = matches[matches.length - 1];
  const pathname = deepest?.pathname ?? "/";
  const segments = pathname.split("/").filter(Boolean);

  if (segments.length === 0) {
    breadcrumbs.push({ title: "Dashboard", href: "/" });
  } else {
    let href = "";
    for (const segment of segments) {
      href += `/${segment}`;
      // A page may publish a friendlier crumb (e.g. a SQL preview) for its own
      // path via the breadcrumb context; fall back to a formatted segment.
      const title = titles[href] || formatSegment(segment);
      if (title) {
        breadcrumbs.push({ title, href });
      }
    }
  }

  return (
    <header className="flex h-14 shrink-0 items-center gap-2 border-b px-4">
      <SidebarTrigger className="-ml-1" />
      <Separator orientation="vertical" className="mr-2 h-4" />
      <Breadcrumb>
        <BreadcrumbList>
          {breadcrumbs.map((crumb, index) => (
            <BreadcrumbItem key={crumb.href || index}>
              {index > 0 && <BreadcrumbSeparator />}
              {index === breadcrumbs.length - 1 ? (
                <BreadcrumbPage>{crumb.title}</BreadcrumbPage>
              ) : (
                <BreadcrumbLink asChild>
                  {/* exact match only: a parent crumb (e.g. "Queries" on a
                      /queries/:id detail page) must not be marked
                      aria-current="page" — only the leaf crumb is. */}
                  <Link to={crumb.href || "/"} activeOptions={{ exact: true }}>
                    {crumb.title}
                  </Link>
                </BreadcrumbLink>
              )}
            </BreadcrumbItem>
          ))}
        </BreadcrumbList>
      </Breadcrumb>
    </header>
  );
}

function formatSegment(segment: string): string {
  // For a UUID detail segment with no published override, fall back to a short
  // id rather than the literal "Details" (which made every detail page's
  // breadcrumb look identical).
  if (
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
      segment
    )
  ) {
    return segment.slice(0, 8);
  }

  // Capitalize and format
  return segment
    .split("-")
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(" ");
}
