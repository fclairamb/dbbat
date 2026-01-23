# Admin Password Reset via Web Token

## Status: Draft

## Summary

Allow administrators to reset any user's password using only their web session token, without requiring the admin to re-enter their current password. This simplifies admin workflows for password resets while maintaining security through token-based authentication.

## Problem

Currently, the password change endpoint (`PUT /api/v1/users/:uid/password`) requires username/password re-authentication in the request body:

```json
{
  "username": "admin",
  "current_password": "admin_password",
  "new_password": "new_user_password"
}
```

This is appropriate for:
- Users changing their own password (proves they know their current password)
- API key-based automation (no session context)

However, for web-authenticated admin users, this creates friction:
- Admin must re-type their password for each password reset
- The web session already proves the admin's identity
- Common admin task (helping users who forgot passwords) becomes cumbersome

## Solution

Create a new endpoint specifically for admin password resets that:
1. Requires web session authentication (Bearer token from login)
2. Requires admin role
3. Only accepts `new_password` in the request body
4. Cannot be used with API keys (web sessions only)

## API Specification

### New Endpoint

```
POST /api/v1/users/:uid/reset-password
```

**Authorization**: Bearer token (web session only, not API keys)

**Required Role**: admin

**Request Body**:
```json
{
  "new_password": "newSecurePassword123"
}
```

**Success Response** (200 OK):
```json
{
  "message": "Password reset successfully"
}
```

**Error Responses**:

| Status | Condition |
|--------|-----------|
| 400 | Invalid request body or password too short |
| 401 | Missing or invalid Bearer token |
| 403 | Non-admin user or API key authentication |
| 404 | Target user not found |

### Endpoint Behavior

1. Validate Bearer token is from web session (not API key)
2. Verify authenticated user has admin role
3. Validate target user exists
4. Validate new password meets requirements (minimum length)
5. Hash new password with Argon2id
6. Update user's password in database
7. Clear `password_change_required` flag for target user
8. Log audit event

### Why POST instead of PUT

Using `POST` distinguishes this administrative reset action from the existing `PUT` self-service password change. The semantics are:
- `PUT /users/:uid/password` - User changes their own password (requires current password)
- `POST /users/:uid/reset-password` - Admin resets a user's password (no current password needed)

## Implementation Details

### Backend Changes

**File**: `internal/api/auth.go`

Add new handler:

```go
// ResetPasswordRequest is the request body for admin password reset
type ResetPasswordRequest struct {
    NewPassword string `json:"new_password" binding:"required"`
}

// handleResetPassword allows admins to reset any user's password via web token
func (s *Server) handleResetPassword(c *gin.Context) {
    // 1. Get current user from context
    currentUser := getCurrentUser(c)
    if currentUser == nil {
        errorResponse(c, http.StatusUnauthorized, "Authentication required")
        return
    }

    // 2. Verify web session (not API key)
    authType := c.GetString("auth_type")
    if authType != "web" {
        errorResponse(c, http.StatusForbidden, "Web session required for password reset")
        return
    }

    // 3. Verify admin role
    if !currentUser.IsAdmin() {
        errorResponse(c, http.StatusForbidden, "Admin access required")
        return
    }

    // 4. Get target user UID
    targetUID := c.Param("uid")

    // 5. Parse request body
    var req ResetPasswordRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        errorResponse(c, http.StatusBadRequest, "Invalid request body")
        return
    }

    // 6. Validate password length
    if len(req.NewPassword) < 8 {
        errorResponse(c, http.StatusBadRequest, "Password must be at least 8 characters")
        return
    }

    // 7. Get target user
    targetUser, err := s.store.GetUserByUID(c.Request.Context(), targetUID)
    if err != nil {
        errorResponse(c, http.StatusNotFound, "User not found")
        return
    }

    // 8. Hash and update password
    hashedPassword, err := crypto.HashPassword(req.NewPassword)
    if err != nil {
        errorResponse(c, http.StatusInternalServerError, "Failed to hash password")
        return
    }

    targetUser.PasswordHash = hashedPassword
    targetUser.PasswordChangeRequired = false

    if err := s.store.UpdateUser(c.Request.Context(), targetUser); err != nil {
        errorResponse(c, http.StatusInternalServerError, "Failed to update password")
        return
    }

    // 9. Audit log
    s.auditLog(c, "password_reset", map[string]any{
        "target_user_uid":      targetUID,
        "target_user_username": targetUser.Username,
        "reset_by":             currentUser.Username,
    })

    c.JSON(http.StatusOK, gin.H{"message": "Password reset successfully"})
}
```

**File**: `internal/api/routes.go`

Register the new route:

```go
users := v1.Group("/users")
{
    // ... existing routes ...
    users.POST("/:uid/reset-password", s.requireAdmin(), s.handleResetPassword)
}
```

**File**: `internal/api/middleware.go`

Ensure auth middleware sets `auth_type` in context:
- `"web"` for Bearer tokens from login endpoint
- `"api_key"` for API key authentication

### Frontend Changes

**File**: `front/src/routes/_authenticated/users/index.tsx`

Add a "Reset Password" action button in the user list:

```tsx
// Add to imports
import { ResetPasswordDialog } from "@/components/shared/ResetPasswordDialog";

// Add state in UsersPage component
const [resetPasswordUser, setResetPasswordUser] = useState<User | null>(null);

// Add column for actions (or modify existing)
{
  id: "actions",
  cell: ({ row }) => {
    const user = row.original;
    const canReset = canResetPassword(currentUser?.roles);

    return (
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="sm" data-testid={`user-actions-${user.username}`}>
            <MoreHorizontal className="h-4 w-4" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent>
          {canReset && (
            <DropdownMenuItem
              onClick={() => setResetPasswordUser(user)}
              data-testid={`reset-password-${user.username}`}
            >
              Reset Password
            </DropdownMenuItem>
          )}
          {/* ... other actions ... */}
        </DropdownMenuContent>
      </DropdownMenu>
    );
  },
}

// Add dialog at end of component JSX
{resetPasswordUser && (
  <ResetPasswordDialog
    user={resetPasswordUser}
    open={!!resetPasswordUser}
    onOpenChange={(open) => !open && setResetPasswordUser(null)}
    onSuccess={() => {
      setResetPasswordUser(null);
      toast.success(`Password reset for ${resetPasswordUser.username}`);
    }}
  />
)}
```

**New File**: `front/src/components/shared/ResetPasswordDialog.tsx`

```tsx
import { useState } from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Loader2, AlertCircle } from "lucide-react";
import { apiClient } from "@/lib/api-client";
import type { User } from "@/types";

interface ResetPasswordDialogProps {
  user: User;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSuccess?: () => void;
}

export function ResetPasswordDialog({
  user,
  open,
  onOpenChange,
  onSuccess,
}: ResetPasswordDialogProps) {
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);

    if (newPassword !== confirmPassword) {
      setError("Passwords do not match");
      return;
    }

    if (newPassword.length < 8) {
      setError("Password must be at least 8 characters");
      return;
    }

    setLoading(true);
    try {
      const response = await apiClient.POST("/users/{uid}/reset-password", {
        params: { path: { uid: user.uid } },
        body: { new_password: newPassword },
      });

      if (response.error) {
        setError(response.error.message || "Failed to reset password");
        return;
      }

      onSuccess?.();
      handleClose();
    } catch (err) {
      setError("Network error. Please try again.");
    } finally {
      setLoading(false);
    }
  };

  const handleClose = () => {
    setNewPassword("");
    setConfirmPassword("");
    setError(null);
    onOpenChange(false);
  };

  const isValid = newPassword.length >= 8 && newPassword === confirmPassword;

  return (
    <Dialog open={open} onOpenChange={handleClose}>
      <DialogContent data-testid="reset-password-dialog">
        <DialogHeader>
          <DialogTitle>Reset Password</DialogTitle>
          <DialogDescription>
            Set a new password for <strong>{user.username}</strong>
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={handleSubmit}>
          {error && (
            <Alert variant="destructive" className="mb-4" data-testid="reset-password-error">
              <AlertCircle className="h-4 w-4" />
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}

          <div className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="new-password">New Password</Label>
              <Input
                id="new-password"
                type="password"
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
                placeholder="Enter new password"
                disabled={loading}
                data-testid="reset-password-new"
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="confirm-password">Confirm Password</Label>
              <Input
                id="confirm-password"
                type="password"
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
                placeholder="Confirm new password"
                disabled={loading}
                data-testid="reset-password-confirm"
              />
            </div>
          </div>

          <DialogFooter className="mt-6">
            <Button
              type="button"
              variant="outline"
              onClick={handleClose}
              disabled={loading}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={loading || !isValid}
              data-testid="reset-password-submit"
            >
              {loading ? (
                <>
                  <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                  Resetting...
                </>
              ) : (
                "Reset Password"
              )}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
```

**File**: `front/src/lib/permissions.ts`

Add permission helper:

```ts
export function canResetPassword(roles?: string[]): boolean {
  return roles?.includes("admin") ?? false;
}
```

### OpenAPI Specification Update

**File**: `internal/api/openapi.yml`

Add under paths:

```yaml
  /users/{uid}/reset-password:
    post:
      summary: Reset user password (admin only)
      description: |
        Allows administrators to reset any user's password without knowing the current password.
        Requires web session authentication (not API keys).
      tags:
        - Users
      security:
        - bearerAuth: []
      parameters:
        - name: uid
          in: path
          required: true
          schema:
            type: string
            format: uuid
          description: Target user's UID
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required:
                - new_password
              properties:
                new_password:
                  type: string
                  minLength: 8
                  description: The new password for the user
      responses:
        "200":
          description: Password reset successfully
          content:
            application/json:
              schema:
                type: object
                properties:
                  message:
                    type: string
                    example: Password reset successfully
        "400":
          description: Invalid request (missing password or too short)
        "401":
          description: Not authenticated
        "403":
          description: Not authorized (non-admin or API key auth)
        "404":
          description: User not found
```

## Testing Requirements

### Unit Tests

**File**: `internal/api/auth_test.go`

```go
func TestResetPassword(t *testing.T) {
    t.Run("admin can reset other user password", func(t *testing.T) {
        // Setup: create admin and target user
        // Action: POST /users/:uid/reset-password with admin web token
        // Assert: 200 OK, target user can login with new password
    })

    t.Run("non-admin cannot reset password", func(t *testing.T) {
        // Setup: create viewer user and target user
        // Action: POST /users/:uid/reset-password with viewer web token
        // Assert: 403 Forbidden
    })

    t.Run("API key authentication rejected", func(t *testing.T) {
        // Setup: create admin with API key
        // Action: POST /users/:uid/reset-password with API key auth
        // Assert: 403 Forbidden with "Web session required" message
    })

    t.Run("password too short rejected", func(t *testing.T) {
        // Setup: create admin and target user
        // Action: POST with 5-char password
        // Assert: 400 Bad Request
    })

    t.Run("target user not found", func(t *testing.T) {
        // Setup: create admin
        // Action: POST /users/nonexistent-uid/reset-password
        // Assert: 404 Not Found
    })
}
```

### E2E Tests

**New File**: `front/e2e/admin-password-reset.spec.ts`

```ts
import { test, expect } from "./fixtures";

const API_BASE = "http://localhost:8080/api/v1";

function uniqueUsername(prefix: string): string {
  return `${prefix}_${Date.now()}_${Math.random().toString(36).slice(2, 7)}`;
}

test.describe("Admin Password Reset", () => {
  test("admin can reset another user's password via UI", async ({
    authenticatedPage,
  }) => {
    const testUsername = uniqueUsername("pwreset");
    const initialPassword = "initialPassword123";
    const newPassword = "newSecurePassword456";

    // Create test user via API
    const loginResponse = await authenticatedPage.request.post(
      `${API_BASE}/auth/login`,
      {
        data: { username: "admin", password: "admintest" },
      }
    );
    const { token } = await loginResponse.json();

    const createResponse = await authenticatedPage.request.post(
      `${API_BASE}/users`,
      {
        headers: { Authorization: `Bearer ${token}` },
        data: {
          username: testUsername,
          password: initialPassword,
          roles: [],
        },
      }
    );
    const testUser = await createResponse.json();

    try {
      // Navigate to users page
      await authenticatedPage.goto("/users");
      await authenticatedPage.waitForLoadState("networkidle");

      // Open actions menu for test user
      await authenticatedPage
        .getByTestId(`user-actions-${testUsername}`)
        .click();

      // Click reset password
      await authenticatedPage
        .getByTestId(`reset-password-${testUsername}`)
        .click();

      // Wait for dialog
      await expect(
        authenticatedPage.getByTestId("reset-password-dialog")
      ).toBeVisible();

      // Fill in new password
      await authenticatedPage
        .getByTestId("reset-password-new")
        .fill(newPassword);
      await authenticatedPage
        .getByTestId("reset-password-confirm")
        .fill(newPassword);

      // Submit
      await authenticatedPage.getByTestId("reset-password-submit").click();

      // Wait for dialog to close
      await expect(
        authenticatedPage.getByTestId("reset-password-dialog")
      ).not.toBeVisible();

      // Verify user can login with new password
      const verifyLogin = await authenticatedPage.request.post(
        `${API_BASE}/auth/login`,
        {
          data: { username: testUsername, password: newPassword },
        }
      );
      expect(verifyLogin.status()).toBe(200);
    } finally {
      // Cleanup: delete test user
      await authenticatedPage.request.delete(
        `${API_BASE}/users/${testUser.uid}`,
        {
          headers: { Authorization: `Bearer ${token}` },
        }
      );
    }
  });

  test("admin can reset password via API", async ({ request }) => {
    const testUsername = uniqueUsername("apireset");
    const initialPassword = "initialPassword123";
    const newPassword = "apiResetPassword789";

    // Login as admin
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    const { token } = await loginResponse.json();

    // Create test user
    const createResponse = await request.post(`${API_BASE}/users`, {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        username: testUsername,
        password: initialPassword,
        roles: [],
      },
    });
    const testUser = await createResponse.json();

    try {
      // Reset password
      const resetResponse = await request.post(
        `${API_BASE}/users/${testUser.uid}/reset-password`,
        {
          headers: { Authorization: `Bearer ${token}` },
          data: { new_password: newPassword },
        }
      );

      expect(resetResponse.status()).toBe(200);
      const resetData = await resetResponse.json();
      expect(resetData.message).toBe("Password reset successfully");

      // Verify new password works
      const verifyLogin = await request.post(`${API_BASE}/auth/login`, {
        data: { username: testUsername, password: newPassword },
      });
      expect(verifyLogin.status()).toBe(200);

      // Verify old password doesn't work
      const oldPasswordLogin = await request.post(`${API_BASE}/auth/login`, {
        data: { username: testUsername, password: initialPassword },
      });
      expect(oldPasswordLogin.status()).toBe(401);
    } finally {
      // Cleanup
      await request.delete(`${API_BASE}/users/${testUser.uid}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
  });

  test("non-admin cannot reset password", async ({ request }) => {
    // Login as viewer (non-admin)
    const viewerLogin = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "viewer", password: "viewer" },
    });
    const { token: viewerToken } = await viewerLogin.json();

    // Get admin's UID
    const adminLogin = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    const { token: adminToken } = await adminLogin.json();

    const meResponse = await request.get(`${API_BASE}/auth/me`, {
      headers: { Authorization: `Bearer ${adminToken}` },
    });
    const adminUser = await meResponse.json();

    // Try to reset admin's password as viewer
    const resetResponse = await request.post(
      `${API_BASE}/users/${adminUser.uid}/reset-password`,
      {
        headers: { Authorization: `Bearer ${viewerToken}` },
        data: { new_password: "hackedPassword123" },
      }
    );

    expect(resetResponse.status()).toBe(403);
  });

  test("password validation enforced", async ({ request }) => {
    // Login as admin
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    const { token } = await loginResponse.json();

    // Create test user
    const testUsername = uniqueUsername("pwvalid");
    const createResponse = await request.post(`${API_BASE}/users`, {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        username: testUsername,
        password: "initialPassword123",
        roles: [],
      },
    });
    const testUser = await createResponse.json();

    try {
      // Try to set password that's too short
      const resetResponse = await request.post(
        `${API_BASE}/users/${testUser.uid}/reset-password`,
        {
          headers: { Authorization: `Bearer ${token}` },
          data: { new_password: "short" },
        }
      );

      expect(resetResponse.status()).toBe(400);
    } finally {
      // Cleanup
      await request.delete(`${API_BASE}/users/${testUser.uid}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
  });
});
```

## Security Considerations

1. **Web session only**: Explicitly reject API key authentication to prevent automated password resets
2. **Admin role required**: Only users with admin role can use this endpoint
3. **Audit logging**: All password resets are logged with admin username and target user
4. **No rate limiting bypass**: This endpoint doesn't bypass rate limiting on the login endpoint
5. **Clear password_change_required**: Reset clears this flag so user doesn't need to change again on login

## Migration

No database schema changes required.

## Rollout Plan

1. Implement backend endpoint with unit tests
2. Update OpenAPI specification and regenerate frontend types
3. Implement frontend UI components
4. Add E2E tests
5. Deploy to staging and test manually
6. Deploy to production
