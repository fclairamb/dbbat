import {
  createContext,
  useContext,
  useState,
  useEffect,
  useCallback,
  useRef,
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

// Surfaced when a session check (GET /auth/me) could not be definitively
// resolved — e.g. rate limited — so the caller knows a valid token might
// still be sitting there unconfirmed. Non-destructive: never implies the
// token was cleared.
export interface SessionRateLimitInfo {
  message: string;
  retryAfterSeconds?: number;
}

interface AuthState {
  user: User | null;
  session: Session | null;
  isAuthenticated: boolean;
  isLoading: boolean;
  isAdmin: boolean;
  sessionRateLimit: SessionRateLimitInfo | null;
}

interface AuthContextType extends AuthState {
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  refreshUser: () => Promise<void>;
  // Re-runs the stored-token session check on demand — used by the
  // rate-limited notice's manual retry action, in addition to the single
  // automatic re-validate scheduled from a Retry-After header.
  retrySessionCheck: () => void;
}

const AuthContext = createContext<AuthContextType | null>(null);

// Outcome of a single GET /auth/me attempt. Only a definitive 401 means the
// token itself is invalid; everything else (429 rate limit, 5xx, a network/
// transport error) means the check simply could not be performed, and must
// never be treated as grounds to destroy a stored session.
type SessionCheckOutcome =
  | { kind: "valid" }
  | { kind: "invalid" }
  | { kind: "indeterminate"; retryAfterSeconds?: number };

function parseRetryAfterSeconds(value: string | null): number | undefined {
  if (!value) {
    return undefined;
  }
  const seconds = Number(value);
  return Number.isFinite(seconds) && seconds > 0 ? seconds : undefined;
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const [state, setState] = useState<AuthState>({
    user: null,
    session: null,
    isAuthenticated: false,
    isLoading: true,
    isAdmin: false,
    sessionRateLimit: null,
  });

  // Validate session by calling /auth/me
  const validateSession = useCallback(async (): Promise<SessionCheckOutcome> => {
    try {
      const response = await apiClient.GET("/auth/me");

      if (!response.error && response.data) {
        const data = response.data;
        setState((prev) => ({
          ...prev,
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
          sessionRateLimit: null,
        }));
        return { kind: "valid" };
      }

      // openapi-fetch exposes the raw fetch Response as `response.response`.
      // Only a 401 definitively means the token is invalid.
      if (response.response.status === 401) {
        return { kind: "invalid" };
      }

      return {
        kind: "indeterminate",
        retryAfterSeconds: parseRetryAfterSeconds(
          response.response.headers.get("Retry-After"),
        ),
      };
    } catch {
      // Network / transport error: the check could not be performed either.
      return { kind: "indeterminate" };
    }
  }, []);

  // Re-runs the current check; set by the mount effect below so the manual
  // retry action can trigger the same logic without duplicating it.
  const checkStoredAuthRef = useRef<(allowAutoRetry: boolean) => void>(
    () => {},
  );
  // The single scheduled automatic re-validate (from a Retry-After), if
  // any — tracked outside the effect so a manual retry can cancel it.
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );

  const retrySessionCheck = useCallback(() => {
    if (retryTimerRef.current) {
      clearTimeout(retryTimerRef.current);
      retryTimerRef.current = undefined;
    }
    // A manual retry gets its own single automatic follow-up if it, too,
    // comes back rate limited — it isn't chained off a prior automatic
    // attempt, so it doesn't count against that budget.
    checkStoredAuthRef.current(true);
  }, []);

  // Check for stored token on mount
  useEffect(() => {
    let cancelled = false;

    // `allowAutoRetry` bounds Retry-After-driven re-validates to exactly
    // one automatic attempt: the initial check (and any manual retry via
    // retrySessionCheck) may schedule one, but that scheduled attempt
    // calls back in with allowAutoRetry=false so a persistent rate limit
    // can't chain retries forever without a user action.
    const checkStoredAuth = async (allowAutoRetry: boolean) => {
      const token = getStoredToken();

      if (!token) {
        if (!cancelled) {
          setState((prev) => ({ ...prev, isLoading: false }));
        }
        return;
      }

      // Token exists, validate it
      const result = await validateSession();
      if (cancelled) {
        return;
      }

      if (result.kind === "valid") {
        return; // validateSession already committed the authenticated state
      }

      if (result.kind === "invalid") {
        clearToken();
        setState({
          user: null,
          session: null,
          isAuthenticated: false,
          isLoading: false,
          isAdmin: false,
          sessionRateLimit: null,
        });
        // Stale token: make sure nothing from a previous identity lingers
        // in the query cache before the login screen renders.
        queryClient.clear();
        return;
      }

      // Indeterminate (429 / 5xx / network error): the token may still be
      // good — keep it and the query cache untouched, finish loading, and
      // surface a non-destructive notice instead of bouncing to /login.
      setState((prev) => ({
        ...prev,
        isLoading: false,
        sessionRateLimit: {
          message: result.retryAfterSeconds
            ? `Too many requests — retrying in ${result.retryAfterSeconds}s.`
            : "Too many requests — please retry in a moment.",
          retryAfterSeconds: result.retryAfterSeconds,
        },
      }));

      if (allowAutoRetry && result.retryAfterSeconds) {
        retryTimerRef.current = setTimeout(() => {
          retryTimerRef.current = undefined;
          if (!cancelled) {
            void checkStoredAuth(false);
          }
        }, result.retryAfterSeconds * 1000);
      }
    };

    checkStoredAuthRef.current = (allowAutoRetry: boolean) => {
      void checkStoredAuth(allowAutoRetry);
    };
    void checkStoredAuth(true);

    return () => {
      cancelled = true;
      if (retryTimerRef.current) {
        clearTimeout(retryTimerRef.current);
        retryTimerRef.current = undefined;
      }
    };
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
          retry_after?: number;
        };
        // POST /auth/login sits behind two different rate limiters that
        // disagree on body shape: the per-username tracker (writeRateLimited,
        // internal/api/errors.go) uses the Error schema — code:
        // "RATE_LIMITED" — but the IP-based PreAuthMiddleware
        // (internal/api/ratelimit.go) writes an ad-hoc
        // {error, message, retry_after} body with no `code` at all. Gate on
        // the raw HTTP status, not the error shape, so both paths get the
        // same friendly message; read retry_after from whichever shape is
        // present, falling back to the Retry-After header both set.
        if (response.response.status === 429) {
          const retryAfterSeconds =
            errorData?.retry_after ??
            parseRetryAfterSeconds(
              response.response.headers.get("Retry-After"),
            );
          throw new Error(
            retryAfterSeconds
              ? `Too many login attempts. Please try again in ${retryAfterSeconds}s.`
              : "Too many login attempts. Please try again shortly.",
          );
        }
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
        sessionRateLimit: null,
      });
    } catch (error) {
      setState({
        user: null,
        session: null,
        isAuthenticated: false,
        isLoading: false,
        isAdmin: false,
        sessionRateLimit: null,
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
        sessionRateLimit: null,
      });
      queryClient.clear();
    }
  }, [queryClient]);

  const refreshUser = useCallback(async () => {
    await validateSession();
  }, [validateSession]);

  return (
    <AuthContext.Provider
      value={{ ...state, login, logout, refreshUser, retrySessionCheck }}
    >
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
