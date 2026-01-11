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

interface BreadcrumbItem {
  title: string;
  href?: string;
}

export function Header() {
  const matches = useMatches();

  // Build breadcrumbs from route matches
  const breadcrumbs: BreadcrumbItem[] = [];

  for (const match of matches) {
    // Skip root and layout routes
    if (match.pathname === "/" && matches.length > 1) continue;

    // Get route meta for title
    const routeMeta = match.context as { title?: string } | undefined;
    const title = routeMeta?.title || formatPathname(match.pathname);

    if (title) {
      breadcrumbs.push({
        title,
        href: match.pathname,
      });
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
                  <Link to={crumb.href || "/"}>{crumb.title}</Link>
                </BreadcrumbLink>
              )}
            </BreadcrumbItem>
          ))}
        </BreadcrumbList>
      </Breadcrumb>
    </header>
  );
}

function formatPathname(pathname: string): string {
  if (pathname === "/") return "Dashboard";

  const segments = pathname.split("/").filter(Boolean);
  const lastSegment = segments[segments.length - 1];

  // Check if it's a UUID (detail page)
  if (
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
      lastSegment
    )
  ) {
    return "Details";
  }

  // Capitalize and format
  return lastSegment
    .split("-")
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(" ");
}
