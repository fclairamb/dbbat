import createClient from "openapi-fetch";
import type { paths } from "./schema";

export const apiBaseUrl: string = import.meta.env.VITE_API_BASE_URL || "/api/v1";

export const apiClient = createClient<paths>({
  baseUrl: apiBaseUrl,
});

export type ApiClient = typeof apiClient;

// Storage key for session token
const TOKEN_KEY = "dbbat_session_token";

// Get stored token
export const getStoredToken = (): string | null => {
  return localStorage.getItem(TOKEN_KEY);
};

// Store token after login
export const storeToken = (token: string): void => {
  localStorage.setItem(TOKEN_KEY, token);
  setupBearerAuth(token);
};

// Clear token on logout or expiration
export const clearToken = (): void => {
  localStorage.removeItem(TOKEN_KEY);
  removeAuth();
};

let authMiddleware: ReturnType<typeof apiClient.use> | null = null;

// Handle 401 response by clearing token and redirecting to login
const handleUnauthorizedResponse = (response: Response): void => {
  if (response.status === 401) {
    // Don't redirect if already on login page to avoid infinite loops
    if (!window.location.pathname.endsWith("/login")) {
      localStorage.removeItem(TOKEN_KEY);
      window.location.href = "/app/login?session_expired=true";
    }
  }
};

// Set up Bearer auth middleware
const setupBearerAuth = (token: string): void => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
  }

  authMiddleware = apiClient.use({
    onRequest({ request }) {
      request.headers.set("Authorization", `Bearer ${token}`);
      return request;
    },
    onResponse({ response }) {
      handleUnauthorizedResponse(response);
      return response;
    },
  });
};

// Legacy: Set up Basic Auth (for password change if needed)
export const setBasicAuth = (username: string, password: string) => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
  }

  const credentials = btoa(`${username}:${password}`);
  authMiddleware = apiClient.use({
    onRequest({ request }) {
      request.headers.set("Authorization", `Basic ${credentials}`);
      return request;
    },
    onResponse({ response }) {
      handleUnauthorizedResponse(response);
      return response;
    },
  });
};

export const setBearerAuth = (token: string) => {
  setupBearerAuth(token);
};

export const removeAuth = () => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
    authMiddleware = null;
  }
};

// Initialize auth from stored token on module load
const storedToken = getStoredToken();
if (storedToken) {
  setupBearerAuth(storedToken);
}
