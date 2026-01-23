import { test, expect } from "@playwright/test";

const API_BASE = "http://localhost:8080/api/v1";

// Generate unique test username to avoid conflicts with parallel tests
function uniqueUsername(prefix: string): string {
  return `${prefix}_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
}

test.describe("Admin Password Reset", () => {
  test("admin can reset another user's password via API", async ({ request }) => {
    const testUsername = uniqueUsername("apireset");
    const initialPassword = "initialPassword123";
    const newPassword = "apiResetPassword789";

    // Step 1: Login as admin
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    expect(loginResponse.status()).toBe(200);
    const { token } = await loginResponse.json();

    // Step 2: Create test user
    const createResponse = await request.post(`${API_BASE}/users`, {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        username: testUsername,
        password: initialPassword,
        roles: [],
      },
    });
    expect(createResponse.status()).toBe(200);
    const testUser = await createResponse.json();

    try {
      // Step 3: Reset password via admin endpoint
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

      // Step 4: Verify new password works
      const verifyLogin = await request.post(`${API_BASE}/auth/login`, {
        data: { username: testUsername, password: newPassword },
      });
      expect(verifyLogin.status()).toBe(200);

      // Step 5: Verify old password doesn't work
      const oldPasswordLogin = await request.post(`${API_BASE}/auth/login`, {
        data: { username: testUsername, password: initialPassword },
      });
      expect(oldPasswordLogin.status()).toBe(401);
    } finally {
      // Cleanup: delete test user
      await request.delete(`${API_BASE}/users/${testUser.uid}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
  });

  test("non-admin cannot reset password", async ({ request }) => {
    // Step 1: Login as viewer (non-admin)
    const viewerLogin = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "viewer", password: "viewer" },
    });
    expect(viewerLogin.status()).toBe(200);
    const { token: viewerToken } = await viewerLogin.json();

    // Step 2: Get admin's UID
    const adminLogin = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    expect(adminLogin.status()).toBe(200);
    const { token: adminToken } = await adminLogin.json();

    const meResponse = await request.get(`${API_BASE}/auth/me`, {
      headers: { Authorization: `Bearer ${adminToken}` },
    });
    expect(meResponse.status()).toBe(200);
    const adminUser = await meResponse.json();

    // Step 3: Try to reset admin's password as viewer
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
    const testUsername = uniqueUsername("pwvalid");

    // Step 1: Login as admin
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    expect(loginResponse.status()).toBe(200);
    const { token } = await loginResponse.json();

    // Step 2: Create test user
    const createResponse = await request.post(`${API_BASE}/users`, {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        username: testUsername,
        password: "initialPassword123",
        roles: [],
      },
    });
    expect(createResponse.status()).toBe(200);
    const testUser = await createResponse.json();

    try {
      // Step 3: Try to set password that's too short
      const resetResponse = await request.post(
        `${API_BASE}/users/${testUser.uid}/reset-password`,
        {
          headers: { Authorization: `Bearer ${token}` },
          data: { new_password: "short" },
        }
      );

      expect(resetResponse.status()).toBe(400);
      const errorData = await resetResponse.json();
      expect(errorData.error).toBe("weak_password");
    } finally {
      // Cleanup
      await request.delete(`${API_BASE}/users/${testUser.uid}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
  });

  test("user not found returns 404", async ({ request }) => {
    // Login as admin
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    expect(loginResponse.status()).toBe(200);
    const { token } = await loginResponse.json();

    // Try to reset password for non-existent user
    const resetResponse = await request.post(
      `${API_BASE}/users/00000000-0000-0000-0000-000000000000/reset-password`,
      {
        headers: { Authorization: `Bearer ${token}` },
        data: { new_password: "newPassword123" },
      }
    );

    expect(resetResponse.status()).toBe(404);
  });

  test("admin cannot reset their own password via reset-password endpoint", async ({
    request,
  }) => {
    // Step 1: Login as admin
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    expect(loginResponse.status()).toBe(200);
    const { token } = await loginResponse.json();

    // Step 2: Get admin's UID
    const meResponse = await request.get(`${API_BASE}/auth/me`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(meResponse.status()).toBe(200);
    const adminUser = await meResponse.json();

    // Step 3: Try to reset own password
    const resetResponse = await request.post(
      `${API_BASE}/users/${adminUser.uid}/reset-password`,
      {
        headers: { Authorization: `Bearer ${token}` },
        data: { new_password: "newAdminPassword123" },
      }
    );

    // Should be forbidden
    expect(resetResponse.status()).toBe(403);

    // Check error message
    const errorData = await resetResponse.json();
    expect(errorData.error).toBe(
      "cannot reset your own password; use the password change endpoint instead"
    );
  });

  test("admin can reset password via UI", async ({ page }) => {
    const testUsername = uniqueUsername("uireset");
    const initialPassword = "initialPassword123";
    const newPassword = "uiResetPassword789";

    // Step 1: Login as admin via API and get token
    const loginResponse = await page.request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    expect(loginResponse.status()).toBe(200);
    const { token } = await loginResponse.json();

    // Step 2: Create test user via API
    const createResponse = await page.request.post(`${API_BASE}/users`, {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        username: testUsername,
        password: initialPassword,
        roles: ["connector"],
      },
    });
    expect(createResponse.status()).toBe(200);
    const testUser = await createResponse.json();

    try {
      // Step 3: Login via UI
      await page.goto("/app/login");
      await page.waitForLoadState("networkidle");

      await page.getByTestId("login-username").fill("admin");
      await page.getByTestId("login-password").fill("admintest");
      await page.getByTestId("login-submit").click();

      // Wait for navigation to complete
      await page.waitForURL((url) => !url.pathname.includes("/login"), {
        timeout: 10000,
      });

      // Step 4: Navigate to users page
      await page.goto("/app/users");
      await page.waitForLoadState("networkidle");

      // Step 5: Open actions menu for test user
      const actionsButton = page.getByTestId(`user-actions-${testUsername}`);
      await actionsButton.waitFor({ state: "visible", timeout: 10000 });
      await actionsButton.click();

      // Step 6: Click reset password
      await page.getByTestId(`reset-password-${testUsername}`).click();

      // Step 7: Wait for dialog
      const dialog = page.getByTestId("reset-password-dialog");
      await expect(dialog).toBeVisible();

      // Step 8: Fill in new password
      await page.getByTestId("reset-password-new").fill(newPassword);
      await page.getByTestId("reset-password-confirm").fill(newPassword);

      // Step 9: Submit
      await page.getByTestId("reset-password-submit").click();

      // Step 10: Wait for dialog to close
      await expect(dialog).not.toBeVisible({ timeout: 10000 });

      // Step 11: Verify user can login with new password via API
      const verifyLogin = await page.request.post(`${API_BASE}/auth/login`, {
        data: { username: testUsername, password: newPassword },
      });
      expect(verifyLogin.status()).toBe(200);
    } finally {
      // Cleanup: delete test user
      await page.request.delete(`${API_BASE}/users/${testUser.uid}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
  });

  test("password mismatch shows error in UI", async ({ page }) => {
    const testUsername = uniqueUsername("mismatch");
    const initialPassword = "initialPassword123";

    // Step 1: Login as admin via API and get token
    const loginResponse = await page.request.post(`${API_BASE}/auth/login`, {
      data: { username: "admin", password: "admintest" },
    });
    const { token } = await loginResponse.json();

    // Step 2: Create test user via API
    const createResponse = await page.request.post(`${API_BASE}/users`, {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        username: testUsername,
        password: initialPassword,
        roles: [],
      },
    });
    const testUser = await createResponse.json();

    try {
      // Step 3: Login via UI
      await page.goto("/app/login");
      await page.waitForLoadState("networkidle");

      await page.getByTestId("login-username").fill("admin");
      await page.getByTestId("login-password").fill("admintest");
      await page.getByTestId("login-submit").click();

      await page.waitForURL((url) => !url.pathname.includes("/login"), {
        timeout: 10000,
      });

      // Step 4: Navigate to users page
      await page.goto("/app/users");
      await page.waitForLoadState("networkidle");

      // Step 5: Open actions menu for test user
      await page.getByTestId(`user-actions-${testUsername}`).click();
      await page.getByTestId(`reset-password-${testUsername}`).click();

      // Step 6: Wait for dialog
      const dialog = page.getByTestId("reset-password-dialog");
      await expect(dialog).toBeVisible();

      // Step 7: Fill in mismatched passwords
      await page.getByTestId("reset-password-new").fill("password12345");
      await page.getByTestId("reset-password-confirm").fill("differentpassword");

      // Step 8: Submit should be disabled or show error
      const submitButton = page.getByTestId("reset-password-submit");
      await expect(submitButton).toBeDisabled();
    } finally {
      // Cleanup
      await page.request.delete(`${API_BASE}/users/${testUser.uid}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
  });
});
