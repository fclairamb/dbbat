import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useCLIAuthRequest, useRespondToCLIAuthRequest } from "@/api";
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
import { Check, KeyRound, ShieldAlert, X } from "lucide-react";
import { formatDistanceToNow } from "date-fns";

export const Route = createFileRoute("/_authenticated/cli-auth/$uid")({
  component: CLIAuthApprovalPage,
});

type LocalOutcome = "approved" | "denied" | null;

function CLIAuthApprovalPage() {
  const { uid } = Route.useParams();
  const { data: request, isLoading, error } = useCLIAuthRequest(uid);
  const [localOutcome, setLocalOutcome] = useState<LocalOutcome>(null);

  const respond = useRespondToCLIAuthRequest(uid);

  if (isLoading) {
    return <PageLoader />;
  }

  if (error || !request) {
    return (
      <div className="flex justify-center pt-12">
        <Card className="w-full max-w-md" data-testid="cli-auth-not-found">
          <CardHeader>
            <CardTitle>Request not found</CardTitle>
            <CardDescription>
              This CLI authorization request doesn't exist anymore, or has
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
      <Card className="w-full max-w-md" data-testid="cli-auth-card">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-2">
            <KeyRound className="h-10 w-10 text-primary" />
          </div>
          <CardTitle data-testid="cli-auth-title">
            {resolved
              ? status === "approved"
                ? "Access granted"
                : "Access denied"
              : "CLI authorization request"}
          </CardTitle>
          {!resolved && (
            <CardDescription>
              A command-line tool is requesting an API key on your behalf.
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
                <div className="font-medium" data-testid="cli-auth-name">
                  {request.name}
                </div>
              </div>

              <div className="space-y-1">
                <div className="text-sm font-medium text-muted-foreground">
                  Verification code
                </div>
                <div
                  className="font-mono text-2xl tracking-widest text-center py-2"
                  data-testid="cli-auth-user-code"
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
              data-testid="cli-auth-outcome"
            >
              {status === "approved" ? (
                <Check className="h-4 w-4" />
              ) : (
                <X className="h-4 w-4" />
              )}
              <AlertDescription>
                {status === "approved"
                  ? "The CLI can now continue — you can close this tab."
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
              data-testid="cli-auth-deny"
            >
              Deny
            </Button>
            <Button
              className="flex-1"
              disabled={respond.isPending}
              onClick={() => handleRespond(true)}
              data-testid="cli-auth-approve"
            >
              Approve
            </Button>
          </CardFooter>
        )}
      </Card>
    </div>
  );
}
