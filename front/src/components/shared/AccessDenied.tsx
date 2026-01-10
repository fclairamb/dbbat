import { AlertTriangle } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useNavigate } from "@tanstack/react-router";
import type { UserRole } from "@/lib/permissions";

interface AccessDeniedProps {
  requiredRole?: UserRole;
  message?: string;
}

export function AccessDenied({ requiredRole, message }: AccessDeniedProps) {
  const navigate = useNavigate();

  const defaultMessage = requiredRole
    ? `This page requires the ${requiredRole} role.`
    : "You do not have permission to access this page.";

  return (
    <div className="flex items-center justify-center min-h-[50vh]">
      <Alert className="max-w-md" variant="destructive">
        <AlertTriangle className="h-4 w-4" />
        <AlertTitle>Access Denied</AlertTitle>
        <AlertDescription className="mt-2">
          <p className="mb-4">{message || defaultMessage}</p>
          <p className="text-sm text-muted-foreground mb-4">
            Contact your administrator if you need access to this resource.
          </p>
          <Button
            variant="outline"
            size="sm"
            onClick={() => navigate({ to: "/" })}
          >
            Return to Dashboard
          </Button>
        </AlertDescription>
      </Alert>
    </div>
  );
}
