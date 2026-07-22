import { createFileRoute, Outlet, redirect, useLocation, useNavigate } from "@tanstack/react-router";
import { useEffect } from "react";
import { SidebarProvider, SidebarInset } from "@/components/ui/sidebar";
import { AppSidebar } from "@/components/layout/AppSidebar";
import { Header } from "@/components/layout/Header";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { useAuth } from "@/contexts/AuthContext";
import { BreadcrumbProvider } from "@/contexts/BreadcrumbContext";

export const Route = createFileRoute("/_authenticated")({
  beforeLoad: ({ context, location }) => {
    // Only redirect if we're definitely not authenticated (not loading, not authenticated)
    if (!context.auth.isAuthenticated && !context.auth.isLoading) {
      throw redirect({ to: "/login", search: { redirect: location.href } });
    }
  },
  component: AuthenticatedLayout,
});

function AuthenticatedLayout() {
  // Use useAuth() directly for reactivity instead of Route.useRouteContext()
  const auth = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  // Redirect to login if authentication fails after loading completes.
  // location.href is deliberately NOT a dependency: navigate() changes it,
  // and depending on it would re-trigger this effect on every redirect,
  // wrapping the target in another layer of query-encoding each time.
  useEffect(() => {
    if (!auth.isLoading && !auth.isAuthenticated) {
      navigate({ to: "/login", search: { redirect: location.href } });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [auth.isLoading, auth.isAuthenticated, navigate]);

  if (auth.isLoading) {
    return <PageLoader />;
  }

  // If not authenticated after loading, show loader while redirecting
  if (!auth.isAuthenticated) {
    return <PageLoader />;
  }

  return (
    <BreadcrumbProvider>
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <Header />
          <main className="flex-1 overflow-auto p-4 md:p-6">
            <Outlet />
          </main>
        </SidebarInset>
      </SidebarProvider>
    </BreadcrumbProvider>
  );
}
