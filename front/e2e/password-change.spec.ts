import { test, expect } from "@playwright/test";

const API_BASE = "http://localhost:8080/api/v1";

// Generate unique test username to avoid conflicts with parallel tests
function uniqueUsername(prefix: string): string {
  return `${prefix}_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
}

test.describe("Password Change API", () => {
  test("should change own password without providing username", async ({
    request,
  }) => {
    // Step 1: Login as admin to get a token
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: {
        username: "admin",
        password: "admintest",
      },
    });
    expect(loginResponse.status()).toBe(200);

    const loginData = await loginResponse.json();
    const token = loginData.token;

    // Step 2: Create a temporary test user
    const testUsername = uniqueUsername("testuser");
    const testPassword = "initialpassword123";

    const createUserResponse = await request.post(`${API_BASE}/users`, {
      headers: {
        Authorization: `Bearer ${token}`,
      },
      data: {
        username: testUsername,
        password: testPassword,
        roles: [],
      },
    });
    expect(createUserResponse.status()).toBe(200);

    const userData = await createUserResponse.json();
    const testUserUid = userData.uid;

    // Step 3: The new user needs to change password (password_change_required)
    // Try to change password WITHOUT providing username - this is the bug fix test
    const newPassword = "newpassword456";
    const changePasswordResponse = await request.put(
      `${API_BASE}/users/${testUserUid}/password`,
      {
        data: {
          // NOTE: Intentionally NOT providing "username" field
          current_password: testPassword,
          new_password: newPassword,
        },
      }
    );

    // This should succeed - the username should be inferred from the :uid param
    expect(changePasswordResponse.status()).toBe(200);
    const changePasswordData = await changePasswordResponse.json();
    expect(changePasswordData.message).toBe("Password changed successfully");

    // Step 4: Verify the password was actually changed by logging in with new password
    const verifyLoginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: {
        username: testUsername,
        password: newPassword,
      },
    });
    expect(verifyLoginResponse.status()).toBe(200);

    // Step 5: Clean up - delete the test user
    const deleteResponse = await request.delete(
      `${API_BASE}/users/${testUserUid}`,
      {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      }
    );
    expect(deleteResponse.status()).toBe(200);
  });

  test("should still work when providing username (backwards compatibility)", async ({
    request,
  }) => {
    // Step 1: Login as admin to get a token
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: {
        username: "admin",
        password: "admintest",
      },
    });
    expect(loginResponse.status()).toBe(200);

    const loginData = await loginResponse.json();
    const token = loginData.token;

    // Step 2: Create a temporary test user
    const testUsername = uniqueUsername("testuser");
    const testPassword = "initialpassword123";

    const createUserResponse = await request.post(`${API_BASE}/users`, {
      headers: {
        Authorization: `Bearer ${token}`,
      },
      data: {
        username: testUsername,
        password: testPassword,
        roles: [],
      },
    });
    expect(createUserResponse.status()).toBe(200);

    const userData = await createUserResponse.json();
    const testUserUid = userData.uid;

    // Step 3: Change password WITH username (original behavior)
    const newPassword = "newpassword456";
    const changePasswordResponse = await request.put(
      `${API_BASE}/users/${testUserUid}/password`,
      {
        data: {
          username: testUsername,
          current_password: testPassword,
          new_password: newPassword,
        },
      }
    );

    expect(changePasswordResponse.status()).toBe(200);
    const changePasswordData = await changePasswordResponse.json();
    expect(changePasswordData.message).toBe("Password changed successfully");

    // Step 4: Verify the password was actually changed
    const verifyLoginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: {
        username: testUsername,
        password: newPassword,
      },
    });
    expect(verifyLoginResponse.status()).toBe(200);

    // Step 5: Clean up
    const deleteResponse = await request.delete(
      `${API_BASE}/users/${testUserUid}`,
      {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      }
    );
    expect(deleteResponse.status()).toBe(200);
  });

  test("should reject invalid current password", async ({ request }) => {
    // Step 1: Login as admin to get a token
    const loginResponse = await request.post(`${API_BASE}/auth/login`, {
      data: {
        username: "admin",
        password: "admintest",
      },
    });
    expect(loginResponse.status()).toBe(200);

    const loginData = await loginResponse.json();
    const token = loginData.token;

    // Step 2: Create a temporary test user
    const testUsername = uniqueUsername("testuser");
    const testPassword = "initialpassword123";

    const createUserResponse = await request.post(`${API_BASE}/users`, {
      headers: {
        Authorization: `Bearer ${token}`,
      },
      data: {
        username: testUsername,
        password: testPassword,
        roles: [],
      },
    });
    expect(createUserResponse.status()).toBe(200);

    const userData = await createUserResponse.json();
    const testUserUid = userData.uid;

    // Step 3: Try to change password with wrong current password
    const changePasswordResponse = await request.put(
      `${API_BASE}/users/${testUserUid}/password`,
      {
        data: {
          current_password: "wrongpassword",
          new_password: "newpassword456",
        },
      }
    );

    expect(changePasswordResponse.status()).toBe(401);
    const errorData = await changePasswordResponse.json();
    expect(errorData.error).toBe("invalid_credentials");

    // Step 4: Clean up
    const deleteResponse = await request.delete(
      `${API_BASE}/users/${testUserUid}`,
      {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      }
    );
    expect(deleteResponse.status()).toBe(200);
  });
});
