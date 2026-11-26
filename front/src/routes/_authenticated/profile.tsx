import { useEffect } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { toast } from "sonner";
import {
  useAuthProviders,
  useLinkIdentity,
  useMyIdentities,
  useUnlinkIdentity,
} from "@/api";
import { PageHeader } from "@/components/shared/PageHeader";
import { navItem } from "@/lib/navigation";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useAuth } from "@/contexts/AuthContext";

type ProfileSearch = { linked?: string; link_error?: string };

export const Route = createFileRoute("/_authenticated/profile")({
  validateSearch: (search: Record<string, unknown>): ProfileSearch => ({
    linked: typeof search.linked === "string" ? search.linked : undefined,
    link_error:
      typeof search.link_error === "string" ? search.link_error : undefined,
  }),
  component: ProfilePage,
});

function providerLabel(type: string, displayName?: string): string {
  if (displayName) return displayName;
  return type === "slack" ? "Slack" : "SSO";
}

function ProfilePage() {
  const { user } = useAuth();
  const { linked, link_error } = Route.useSearch();
  const { data: providers } = useAuthProviders();
  const { data: identities, isLoading } = useMyIdentities();

  const link = useLinkIdentity({ onError: (e) => toast.error(e.message) });
  const unlink = useUnlinkIdentity({
    onSuccess: () => toast.success("Identity unlinked"),
    onError: (e) => toast.error(e.message),
  });

  useEffect(() => {
    if (linked) toast.success("Identity linked");
    if (link_error) toast.error("Could not link the identity");
  }, [linked, link_error]);

  const ssoProviders = (providers ?? []).flatMap((p) =>
    p.enabled === true && p.type && p.type !== "password"
      ? [{ type: p.type, display_name: p.display_name }]
      : [],
  );

  if (isLoading) return <PageLoader />;

  return (
    <div className="space-y-6" data-testid="profile-page">
      <PageHeader
        icon={navItem("/profile")!.icon}
        title="Profile"
        description={`Signed in as ${user?.username ?? ""}`}
      />
      <Card>
        <CardHeader>
          <CardTitle>Single sign-on identities</CardTitle>
          <CardDescription>
            Link your SSO identity to this account so you can sign in with it
            (and approve requests from Slack).
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {ssoProviders.length === 0 && (
            <p className="text-sm text-muted-foreground" data-testid="no-sso-providers">
              No SSO provider is configured.
            </p>
          )}
          {ssoProviders.map((p) => {
            const identity = (identities ?? []).find(
              (i) => i.provider === p.type,
            );
            const label = providerLabel(p.type, p.display_name);
            return (
              <div
                key={p.type}
                className="flex items-center justify-between rounded-md border p-3"
                data-testid={`identity-row-${p.type}`}
              >
                <div>
                  <div className="font-medium">{label}</div>
                  <div
                    className="text-sm text-muted-foreground"
                    data-testid={`identity-status-${p.type}`}
                  >
                    {identity
                      ? `Linked${identity.email ? ` as ${identity.email}` : identity.display_name ? ` as ${identity.display_name}` : ""}`
                      : "Not linked"}
                  </div>
                </div>
                {identity ? (
                  <Button
                    variant="outline"
                    data-testid={`unlink-identity-${p.type}`}
                    disabled={unlink.isPending}
                    onClick={() => unlink.mutate(p.type)}
                  >
                    Unlink
                  </Button>
                ) : (
                  <Button
                    data-testid={`link-identity-${p.type}`}
                    disabled={link.isPending}
                    onClick={() => link.mutate(p.type)}
                  >
                    Link my {label} identity
                  </Button>
                )}
              </div>
            );
          })}
        </CardContent>
      </Card>
    </div>
  );
}
