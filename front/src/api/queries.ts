import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient } from "./client";
import type { components } from "./schema";

// Type aliases for convenience
export type User = components["schemas"]["User"];
export type CreateUserRequest = components["schemas"]["CreateUserRequest"];
export type UpdateUserRequest = components["schemas"]["UpdateUserRequest"];
export type Database = components["schemas"]["Database"];
export type DatabaseLimited = components["schemas"]["DatabaseLimited"];
export type CreateDatabaseRequest =
  components["schemas"]["CreateDatabaseRequest"];
export type UpdateDatabaseRequest =
  components["schemas"]["UpdateDatabaseRequest"];
export type AccessGrant = components["schemas"]["AccessGrant"];
export type CreateGrantRequest = components["schemas"]["CreateGrantRequest"];
export type Connection = components["schemas"]["Connection"];
export type Query = components["schemas"]["Query"];
export type QueryWithRows = components["schemas"]["QueryWithRows"];
export type AuditEvent = components["schemas"]["AuditEvent"];
export type APIKey = components["schemas"]["APIKey"];
export type CreateAPIKeyRequest = components["schemas"]["CreateAPIKeyRequest"];
export type CreateAPIKeyResponse =
  components["schemas"]["CreateAPIKeyResponse"];

// ============================================================================
// Users
// ============================================================================

export function useUsers() {
  return useQuery({
    queryKey: ["users"],
    queryFn: async (): Promise<User[]> => {
      const response = await apiClient.GET("/users");
      if (response.error) {
        throw new Error(response.error.error || "Failed to load users");
      }
      return response.data?.users || [];
    },
  });
}

export function useUser(uid: string) {
  return useQuery({
    queryKey: ["users", uid],
    queryFn: async (): Promise<User> => {
      const response = await apiClient.GET("/users/{uid}", {
        params: { path: { uid } },
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to load user");
      }
      return response.data;
    },
    enabled: !!uid,
  });
}

export function useCreateUser(options?: {
  onSuccess?: (user: User) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (userData: CreateUserRequest): Promise<User> => {
      const response = await apiClient.POST("/users", {
        body: userData,
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to create user");
      }
      return response.data;
    },
    onSuccess: (user) => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
      options?.onSuccess?.(user);
    },
    onError: options?.onError,
  });
}

export function useUpdateUser(
  uid: string,
  options?: {
    onSuccess?: () => void;
    onError?: (error: Error) => void;
  }
) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (userData: UpdateUserRequest): Promise<void> => {
      const response = await apiClient.PUT("/users/{uid}", {
        params: { path: { uid } },
        body: userData,
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to update user");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
      queryClient.invalidateQueries({ queryKey: ["users", uid] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

export function useDeleteUser(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<void> => {
      const response = await apiClient.DELETE("/users/{uid}", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to delete user");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

// ============================================================================
// Databases
// ============================================================================

export function useDatabases() {
  return useQuery({
    queryKey: ["databases"],
    queryFn: async (): Promise<(Database | DatabaseLimited)[]> => {
      const response = await apiClient.GET("/databases");
      if (response.error) {
        throw new Error(response.error.error || "Failed to load databases");
      }
      return response.data?.databases || [];
    },
  });
}

export function useDatabase(uid: string) {
  return useQuery({
    queryKey: ["databases", uid],
    queryFn: async (): Promise<Database | DatabaseLimited> => {
      const response = await apiClient.GET("/databases/{uid}", {
        params: { path: { uid } },
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to load database");
      }
      return response.data;
    },
    enabled: !!uid,
  });
}

export function useCreateDatabase(options?: {
  onSuccess?: (db: Database) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (data: CreateDatabaseRequest): Promise<Database> => {
      const response = await apiClient.POST("/databases", {
        body: data,
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to create database");
      }
      return response.data;
    },
    onSuccess: (db) => {
      queryClient.invalidateQueries({ queryKey: ["databases"] });
      options?.onSuccess?.(db);
    },
    onError: options?.onError,
  });
}

export function useUpdateDatabase(
  uid: string,
  options?: {
    onSuccess?: () => void;
    onError?: (error: Error) => void;
  }
) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (data: UpdateDatabaseRequest): Promise<void> => {
      const response = await apiClient.PUT("/databases/{uid}", {
        params: { path: { uid } },
        body: data,
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to update database");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["databases"] });
      queryClient.invalidateQueries({ queryKey: ["databases", uid] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

export function useDeleteDatabase(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<void> => {
      const response = await apiClient.DELETE("/databases/{uid}", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to delete database");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["databases"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

// ============================================================================
// Grants
// ============================================================================

export function useGrants(filters?: {
  user_id?: string;
  database_id?: string;
  active_only?: boolean;
}) {
  return useQuery({
    queryKey: ["grants", filters],
    queryFn: async (): Promise<AccessGrant[]> => {
      const response = await apiClient.GET("/grants", {
        params: { query: filters },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to load grants");
      }
      return response.data?.grants || [];
    },
  });
}

export function useGrant(uid: string) {
  return useQuery({
    queryKey: ["grants", uid],
    queryFn: async (): Promise<AccessGrant> => {
      const response = await apiClient.GET("/grants/{uid}", {
        params: { path: { uid } },
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to load grant");
      }
      return response.data;
    },
    enabled: !!uid,
  });
}

export function useCreateGrant(options?: {
  onSuccess?: (grant: AccessGrant) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (data: CreateGrantRequest): Promise<AccessGrant> => {
      const response = await apiClient.POST("/grants", {
        body: data,
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to create grant");
      }
      return response.data;
    },
    onSuccess: (grant) => {
      queryClient.invalidateQueries({ queryKey: ["grants"] });
      options?.onSuccess?.(grant);
    },
    onError: options?.onError,
  });
}

export function useRevokeGrant(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<void> => {
      const response = await apiClient.DELETE("/grants/{uid}", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to revoke grant");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["grants"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

// ============================================================================
// Connections
// ============================================================================

export function useConnections(filters?: {
  user_id?: string;
  database_id?: string;
  limit?: number;
  offset?: number;
}) {
  return useQuery({
    queryKey: ["connections", filters],
    queryFn: async (): Promise<Connection[]> => {
      const response = await apiClient.GET("/connections", {
        params: { query: filters },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to load connections");
      }
      return response.data?.connections || [];
    },
  });
}

// ============================================================================
// Queries
// ============================================================================

export function useQueries(filters?: {
  connection_id?: string;
  user_id?: string;
  database_id?: string;
  start_time?: string;
  end_time?: string;
  limit?: number;
  offset?: number;
}) {
  return useQuery({
    queryKey: ["queries", filters],
    queryFn: async (): Promise<Query[]> => {
      const response = await apiClient.GET("/queries", {
        params: { query: filters },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to load queries");
      }
      return response.data?.queries || [];
    },
  });
}

export function useQueryDetails(uid: string) {
  return useQuery({
    queryKey: ["queries", uid],
    queryFn: async (): Promise<Query> => {
      const response = await apiClient.GET("/queries/{uid}", {
        params: { path: { uid } },
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to load query");
      }
      return response.data;
    },
    enabled: !!uid,
  });
}

// Type for paginated query rows response
export type QueryRowsResult = {
  rows: components["schemas"]["QueryRow"][];
  next_cursor?: string;
  has_more: boolean;
  total_rows: number;
};

export function useQueryRows(
  uid: string,
  options?: { cursor?: string; limit?: number }
) {
  return useQuery({
    queryKey: ["queries", uid, "rows", options?.cursor, options?.limit],
    queryFn: async (): Promise<QueryRowsResult> => {
      const response = await apiClient.GET("/queries/{uid}/rows", {
        params: {
          path: { uid },
          query: { cursor: options?.cursor, limit: options?.limit },
        },
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to load query rows");
      }
      return response.data as QueryRowsResult;
    },
    enabled: !!uid,
  });
}

// ============================================================================
// Audit
// ============================================================================

export function useAuditEvents(filters?: {
  event_type?: string;
  user_id?: string;
  performed_by?: string;
  start_time?: string;
  end_time?: string;
  limit?: number;
  offset?: number;
}) {
  return useQuery({
    queryKey: ["audit", filters],
    queryFn: async (): Promise<AuditEvent[]> => {
      const response = await apiClient.GET("/audit", {
        params: { query: filters },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to load audit events");
      }
      return response.data?.audit_events || [];
    },
  });
}

// ============================================================================
// API Keys
// ============================================================================

export function useAPIKeys(filters?: {
  user_id?: string;
  include_all?: boolean;
}) {
  return useQuery({
    queryKey: ["api-keys", filters],
    queryFn: async (): Promise<APIKey[]> => {
      const response = await apiClient.GET("/keys", {
        params: { query: filters },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to load API keys");
      }
      return response.data?.keys || [];
    },
  });
}

export function useAPIKey(id: string) {
  return useQuery({
    queryKey: ["api-keys", id],
    queryFn: async (): Promise<APIKey> => {
      const response = await apiClient.GET("/keys/{id}", {
        params: { path: { id } },
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to load API key");
      }
      return response.data;
    },
    enabled: !!id,
  });
}

export function useCreateAPIKey(options?: {
  onSuccess?: (data: CreateAPIKeyResponse) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (
      data: CreateAPIKeyRequest
    ): Promise<CreateAPIKeyResponse> => {
      const response = await apiClient.POST("/keys", {
        body: data,
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.error || "Failed to create API key");
      }
      return response.data;
    },
    onSuccess: (data) => {
      queryClient.invalidateQueries({ queryKey: ["api-keys"] });
      options?.onSuccess?.(data);
    },
    onError: options?.onError,
  });
}

export function useRevokeAPIKey(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (id: string): Promise<void> => {
      const response = await apiClient.DELETE("/keys/{id}", {
        params: { path: { id } },
      });
      if (response.error) {
        throw new Error(response.error.error || "Failed to revoke API key");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["api-keys"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

// ============================================================================
// Health
// ============================================================================

export function useHealth() {
  return useQuery({
    queryKey: ["health"],
    queryFn: async () => {
      const response = await apiClient.GET("/health");
      if (response.error) {
        throw new Error(response.error.error || "Service unhealthy");
      }
      return response.data;
    },
    retry: false,
  });
}
