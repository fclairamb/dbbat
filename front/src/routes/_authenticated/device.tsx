import { useState } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useDeviceConsent, useRespondToDeviceConsent } from "@/api";
import { PageLoader } from "@/components/shared/LoadingSpinner";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Check, KeyRound, ShieldAlert, X } from "lucide-react";
import { formatDistanceToNow } from "date-fns";

type DeviceSearch = { user_code?: string };

export const Route = createFileRoute("/_authenticated/device")({
  validateSearch: (search: Record<string, unknown>): DeviceSearch => ({
    user_code:
      typeof search.user_code === "string" ? search.user_code : undefined,
  }),
  component: DeviceConsentPage,
});

function DeviceConsentPage() {
  const { user_code } = Route.useSearch();
  // No code yet → show the entry field (RFC 8628 verification_uri path).
  // Code present → fetch the request and show approve/deny
  // (verification_uri_complete path).
  return user_code ? <Consent userCode={user_code} /> : <CodeEntry />;
}

function CodeEntry() {
  const navigate = useNavigate();
  const [code, setCode] = useState("");

  return (
    <div className="flex justify-center pt-12">
      <Card className="w-full max-w-md" data-testid="device-entry-card">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-2">
            <KeyRound className="h-10 w-10 text-primary" />
          </div>
          <CardTitle>Authorize a device</CardTitle>
          <CardDescription>
            Enter the code shown in your terminal or app.
          </CardDescription>
        </CardHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            const trimmed = code.trim();
            if (trimmed) {
              navigate({ to: "/device", search: { user_code: trimmed } });
            }
          }}
        >
          <CardContent>
            <div className="space-y-2">
              <Label htmlFor="user_code">Code</Label>
              <Input
                id="user_code"
                data-testid="device-code-input"
                value={code}
                onChange={(e) => setCode(e.target.value)}
                placeholder="WDJP-4KXR"
                autoFocus
                autoComplete="off"
                className="font-mono tracking-widest text-center uppercase"
              />
            </div>
          </CardContent>
          <CardFooter>
            <Button
              type="submit"
              className="w-full"
              disabled={!code.trim()}
              data-testid="device-code-continue"
            >
              Continue
            </Button>
          </CardFooter>
        </form>
      </Card>
    </div>
  );
}

type LocalOutcome = "approved" | "denied" | null;

function Consent({ userCode }: { userCode: string }) {
  const { data: request, isLoading, error } = useDeviceConsent(userCode);
  const [localOutcome, setLocalOutcome] = useState<LocalOutcome>(null);
  const respond = useRespondToDeviceConsent(userCode);

  if (isLoading) {
    return <PageLoader />;
  }

  if (error || !request) {
    return (
      <div className="flex justify-center pt-12">
        <Card className="w-full max-w-md" data-testid="device-not-found">
          <CardHeader>
            <CardTitle>Request not found</CardTitle>
            <CardDescription>
              This device authorization request doesn't exist anymore, or has
              expired. Try running the command again.
            </CardDescription>
          </CardHeader>
        </Card>
      </div>
    );
  }

  const status = localOutcome ?? request.status;
  const resolved = status !== "pending";

  const handleRespond = (approve: boolean) => {
    respond.mutate(approve, {
      onSuccess: () => setLocalOutcome(approve ? "approved" : "denied"),
    });
  };

  return (
    <div className="flex justify-center pt-12">
      <Card className="w-full max-w-md" data-testid="device-consent-card">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-2">
            <KeyRound className="h-10 w-10 text-primary" />
          </div>
          <CardTitle data-testid="device-consent-title">
            {resolved
              ? status === "approved"
                ? "Access granted"
                : "Access denied"
              : "Device authorization request"}
          </CardTitle>
          {!resolved && (
            <CardDescription>
              An application is requesting an API key on your behalf.
            </CardDescription>
          )}
        </CardHeader>
        <CardContent className="space-y-4">
          {!resolved && (
            <>
              <div className="space-y-1">
                <div className="text-sm font-medium text-muted-foreground">
                  Requesting application
                </div>
                <div className="font-medium" data-testid="device-client-name">
                  {request.client_name}
                </div>
              </div>

              <div className="space-y-1">
                <div className="text-sm font-medium text-muted-foreground">
                  Verification code
                </div>
                <div
                  className="font-mono text-2xl tracking-widest text-center py-2"
                  data-testid="device-user-code"
                >
                  {request.user_code}
                </div>
              </div>

              <Alert>
                <ShieldAlert className="h-4 w-4" />
                <AlertTitle>Check this code</AlertTitle>
                <AlertDescription>
                  Make sure this code matches what's shown in your terminal
                  before approving. Approving mints a new API key for your
                  account.
                </AlertDescription>
              </Alert>

              <div className="text-xs text-muted-foreground text-center">
                Expires{" "}
                {formatDistanceToNow(new Date(request.expires_at), {
                  addSuffix: true,
                })}
              </div>
            </>
          )}

          {resolved && (
            <Alert
              variant={status === "approved" ? "default" : "destructive"}
              data-testid="device-outcome"
            >
              {status === "approved" ? (
                <Check className="h-4 w-4" />
              ) : (
                <X className="h-4 w-4" />
              )}
              <AlertDescription>
                {status === "approved"
                  ? "The application can now continue — you can close this tab."
                  : "The request was denied. You can close this tab."}
              </AlertDescription>
            </Alert>
          )}
        </CardContent>
        {!resolved && (
          <CardFooter className="flex gap-2">
            <Button
              variant="outline"
              className="flex-1"
              disabled={respond.isPending}
              onClick={() => handleRespond(false)}
              data-testid="device-deny"
            >
              Deny
            </Button>
            <Button
              className="flex-1"
              disabled={respond.isPending}
              onClick={() => handleRespond(true)}
              data-testid="device-approve"
            >
              Approve
            </Button>
          </CardFooter>
        )}
      </Card>
    </div>
  );
}
