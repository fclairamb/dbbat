import {
  createContext,
  useContext,
  useState,
  useEffect,
  useCallback,
  type ReactNode,
} from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  storeToken,
  clearToken,
  getStoredToken,
  apiClient,
} from "@/api/client";

interface User {
  uid: string;
  username: string;
  roles: string[];
  passwordChangeRequired: boolean;
}

interface Session {
  expiresAt: string;
  createdAt: string;
}

interface AuthState {
  user: User | null;
  session: Session | null;
  isAuthenticated: boolean;
  isLoading: boolean;
  isAdmin: boolean;
}

interface AuthContextType extends AuthState {
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  refreshUser: () => Promise<void>;
}

const AuthContext = createContext<AuthContextType | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const [state, setState] = useState<AuthState>({
    user: null,
    session: null,
    isAuthenticated: false,
    isLoading: true,
    isAdmin: false,
  });

  // Validate session by calling /auth/me
  const validateSession = useCallback(async (): Promise<boolean> => {
    try {
      const response = await apiClient.GET("/auth/me");

      if (response.error || !response.data) {
        return false;
      }

      const data = response.data;
      setState({
        user: {
          uid: data.uid,
          username: data.username,
          roles: data.roles,
          passwordChangeRequired: data.password_change_required,
        },
        session: {
          expiresAt: data.session?.expires_at || "",
          createdAt: data.session?.created_at || "",
        },
        isAuthenticated: true,
        isLoading: false,
        isAdmin: data.roles?.includes("admin") ?? false,
      });
      return true;
    } catch {
      return false;
    }
  }, []);

  // Check for stored token on mount
  useEffect(() => {
    const checkStoredAuth = async () => {
      const token = getStoredToken();

      if (!token) {
        setState((prev) => ({ ...prev, isLoading: false }));
        return;
      }

      // Token exists, validate it
      const valid = await validateSession();
      if (!valid) {
        clearToken();
        setState({
          user: null,
          session: null,
          isAuthenticated: false,
          isLoading: false,
          isAdmin: false,
        });
        // Stale token: make sure nothing from a previous identity lingers
        // in the query cache before the login screen renders.
        queryClient.clear();
      }
    };

    checkStoredAuth();
  }, [validateSession, queryClient]);

  const login = useCallback(async (username: string, password: string) => {
    setState((prev) => ({ ...prev, isLoading: true }));

    try {
      const response = await apiClient.POST("/auth/login", {
        body: { username, password },
      });

      if (response.error || !response.data) {
        // Extract error code from response for password_change_required detection
        const errorData = response.error as {
          code?: string;
          message?: string;
        };
        throw new Error(errorData?.code || "Login failed");
      }

      const data = response.data;

      // Store the token
      storeToken(data.token);

      // Drop anything cached under the previous identity (or from an
      // anonymous session) before any query mounts under the new one.
      queryClient.clear();

      // Update state with user info from login response
      setState({
        user: {
          uid: data.user.uid,
          username: data.user.username,
          roles: data.user.roles,
          passwordChangeRequired: data.user.password_change_required,
        },
        session: {
          expiresAt: data.expires_at,
          createdAt: new Date().toISOString(),
        },
        isAuthenticated: true,
        isLoading: false,
        isAdmin: data.user.roles?.includes("admin") ?? false,
      });
    } catch (error) {
      setState({
        user: null,
        session: null,
        isAuthenticated: false,
        isLoading: false,
        isAdmin: false,
      });
      throw error;
    }
  }, [queryClient]);

  const logout = useCallback(async () => {
    try {
      // Call logout endpoint to revoke the session
      await apiClient.POST("/auth/logout");
    } catch {
      // Ignore errors - we're logging out anyway
    } finally {
      clearToken();
      // Reset auth state first so the router unmounts authenticated routes
      // before the cache is emptied below — otherwise still-mounted queries
      // could refetch without a token and bounce through the 401 redirect.
      setState({
        user: null,
        session: null,
        isAuthenticated: false,
        isLoading: false,
        isAdmin: false,
      });
      queryClient.clear();
    }
  }, [queryClient]);

  const refreshUser = useCallback(async () => {
    await validateSession();
  }, [validateSession]);

  return (
    <AuthContext.Provider value={{ ...state, login, logout, refreshUser }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth() {
  const context = useContext(AuthContext);
  if (!context) {
    throw new Error("useAuth must be used within an AuthProvider");
  }
  return context;
}

// Re-export User type for use in other files
export type { User };
