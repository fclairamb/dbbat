import { createFileRoute, Outlet, redirect, useLocation, useNavigate } from "@tanstack/react-router";
import { useEffect } from "react";
import { SidebarProvider, SidebarInset } from "@/components/ui/sidebar";
import { AppSidebar } from "@/components/layout/AppSidebar";
import { Header } from "@/components/layout/Header";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { useAuth } from "@/contexts/AuthContext";
import { BreadcrumbProvider } from "@/contexts/BreadcrumbContext";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";

export const Route = createFileRoute("/_authenticated")({
  beforeLoad: ({ context, location }) => {
    // Only redirect if we're definitely not authenticated: not loading, not
    // authenticated, AND the stored-token check wasn't merely blocked by a
    // rate limit / transient failure (context.auth.sessionRateLimit) — a
    // token that couldn't be *confirmed* must not be treated the same as
    // one that was confirmed invalid.
    if (
      !context.auth.isAuthenticated &&
      !context.auth.isLoading &&
      !context.auth.sessionRateLimit
    ) {
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
    if (!auth.isLoading && !auth.isAuthenticated && !auth.sessionRateLimit) {
      navigate({ to: "/login", search: { redirect: location.href } });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [auth.isLoading, auth.isAuthenticated, auth.sessionRateLimit, navigate]);

  if (auth.isLoading) {
    return <PageLoader />;
  }

  // The stored token couldn't be confirmed (rate limited / transient
  // failure) — it has NOT been cleared, so stay put instead of bouncing to
  // /login. A single automatic re-validate is already scheduled when a
  // Retry-After was given; this also offers a manual retry.
  if (!auth.isAuthenticated && auth.sessionRateLimit) {
    return (
      <div className="flex h-screen w-screen items-center justify-center bg-gradient-to-br from-background to-muted p-4">
        <Alert className="max-w-md" data-testid="session-check-rate-limited">
          <AlertDescription className="flex flex-col gap-3">
            <span>{auth.sessionRateLimit.message}</span>
            <Button
              size="sm"
              variant="outline"
              className="self-start"
              onClick={auth.retrySessionCheck}
              data-testid="session-check-retry"
            >
              Retry now
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
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
