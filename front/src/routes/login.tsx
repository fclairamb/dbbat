import { useState } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useAuth } from "@/contexts/AuthContext";
import { apiClient } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Loader2, CheckCircle2 } from "lucide-react";

export const Route = createFileRoute("/login")({
  component: LoginPage,
});

type ViewState = "login" | "password-change";

function LoginPage() {
  const navigate = useNavigate();
  const { login, isLoading } = useAuth();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [viewState, setViewState] = useState<ViewState>("login");

  const handleLoginSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setSuccess(null);
    setIsSubmitting(true);

    try {
      await login(username, password);
      navigate({ to: "/" });
    } catch (err) {
      const errorMessage =
        err instanceof Error ? err.message : "Login failed";
      // Check if this is a password_change_required error
      if (errorMessage === "password_change_required") {
        setViewState("password-change");
        setError(null);
      } else {
        setError(errorMessage);
      }
    } finally {
      setIsSubmitting(false);
    }
  };

  const handlePasswordChangeSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setSuccess(null);

    // Validate passwords match
    if (newPassword !== confirmPassword) {
      setError("Passwords do not match");
      return;
    }

    // Validate password length
    if (newPassword.length < 8) {
      setError("Password must be at least 8 characters");
      return;
    }

    setIsSubmitting(true);

    try {
      const response = await apiClient.PUT("/auth/password", {
        body: {
          username,
          current_password: password,
          new_password: newPassword,
        },
      });

      if (response.error) {
        const errorData = response.error as { error?: string; message?: string };
        throw new Error(errorData.message || errorData.error || "Password change failed");
      }

      // Password changed successfully, now auto-login with new password
      setSuccess("Password changed successfully! Logging in...");

      // Wait a moment to show the success message
      await new Promise((resolve) => setTimeout(resolve, 1000));

      // Login with the new password
      await login(username, newPassword);
      navigate({ to: "/" });
    } catch (err) {
      setError(err instanceof Error ? err.message : "Password change failed");
    } finally {
      setIsSubmitting(false);
    }
  };

  const handleBackToLogin = () => {
    setViewState("login");
    setNewPassword("");
    setConfirmPassword("");
    setError(null);
    setSuccess(null);
  };

  if (isLoading) {
    return (
      <div className="flex h-screen w-screen items-center justify-center bg-gradient-to-br from-background to-muted">
        <Loader2 className="h-8 w-8 animate-spin text-primary" />
      </div>
    );
  }

  return (
    <div className="flex h-screen w-screen items-center justify-center bg-gradient-to-br from-background via-background to-primary/10">
      <Card className="w-full max-w-md mx-4" data-testid="login-card">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-4">
            <img
              src={`${import.meta.env.BASE_URL}logo-text.png`}
              alt="DBBat"
              className="h-32 w-32"
              data-testid="login-logo"
            />
          </div>
          {viewState === "password-change" && (
            <>
              <CardTitle className="text-2xl" data-testid="login-title">
                Change Password
              </CardTitle>
              <CardDescription data-testid="login-description">
                You must change your password before logging in
              </CardDescription>
            </>
          )}
        </CardHeader>
        <CardContent>
          {viewState === "login" ? (
            <form onSubmit={handleLoginSubmit} className="space-y-4">
              {error && (
                <Alert variant="destructive" data-testid="login-error">
                  <AlertDescription>{error}</AlertDescription>
                </Alert>
              )}

              <div className="space-y-2">
                <Label htmlFor="username">Username</Label>
                <Input
                  id="username"
                  type="text"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  placeholder="admin"
                  required
                  autoComplete="username"
                  autoFocus
                  data-testid="login-username"
                />
              </div>

              <div className="space-y-2">
                <Label htmlFor="password">Password</Label>
                <Input
                  id="password"
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  placeholder="Enter your password"
                  required
                  autoComplete="current-password"
                  data-testid="login-password"
                />
              </div>

              <Button
                type="submit"
                className="w-full"
                disabled={isSubmitting || !username || !password}
                data-testid="login-submit"
              >
                {isSubmitting ? (
                  <>
                    <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                    Signing in...
                  </>
                ) : (
                  "Sign in"
                )}
              </Button>
            </form>
          ) : (
            <form onSubmit={handlePasswordChangeSubmit} className="space-y-4">
              {error && (
                <Alert variant="destructive" data-testid="password-change-error">
                  <AlertDescription>{error}</AlertDescription>
                </Alert>
              )}

              {success && (
                <Alert data-testid="password-change-success">
                  <CheckCircle2 className="h-4 w-4" />
                  <AlertDescription>{success}</AlertDescription>
                </Alert>
              )}

              <div className="space-y-2">
                <Label htmlFor="current-password">Current Password</Label>
                <Input
                  id="current-password"
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  placeholder="Enter your current password"
                  required
                  autoComplete="current-password"
                  data-testid="password-change-current"
                />
              </div>

              <div className="space-y-2">
                <Label htmlFor="new-password">New Password</Label>
                <Input
                  id="new-password"
                  type="password"
                  value={newPassword}
                  onChange={(e) => setNewPassword(e.target.value)}
                  placeholder="Enter your new password"
                  required
                  autoComplete="new-password"
                  autoFocus
                  data-testid="password-change-new"
                />
              </div>

              <div className="space-y-2">
                <Label htmlFor="confirm-password">Confirm New Password</Label>
                <Input
                  id="confirm-password"
                  type="password"
                  value={confirmPassword}
                  onChange={(e) => setConfirmPassword(e.target.value)}
                  placeholder="Confirm your new password"
                  required
                  autoComplete="new-password"
                  data-testid="password-change-confirm"
                />
              </div>

              <div className="flex gap-2">
                <Button
                  type="button"
                  variant="outline"
                  className="flex-1"
                  onClick={handleBackToLogin}
                  disabled={isSubmitting}
                  data-testid="password-change-back"
                >
                  Back
                </Button>
                <Button
                  type="submit"
                  className="flex-1"
                  disabled={
                    isSubmitting || !password || !newPassword || !confirmPassword
                  }
                  data-testid="password-change-submit"
                >
                  {isSubmitting ? (
                    <>
                      <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                      Changing...
                    </>
                  ) : (
                    "Change Password"
                  )}
                </Button>
              </div>
            </form>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
