import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient } from "./client";
import type { components } from "./schema";
import { downloadBlob } from "@/lib/download";

// Type aliases for convenience
export type User = components["schemas"]["User"];
export type CreateUserRequest = components["schemas"]["CreateUserRequest"];
export type UpdateUserRequest = components["schemas"]["UpdateUserRequest"];
export type UserGroup = components["schemas"]["UserGroup"];
export type CreateUserGroupRequest =
  components["schemas"]["CreateUserGroupRequest"];
export type ServerGroup = components["schemas"]["ServerGroup"];
export type CreateServerGroupRequest =
  components["schemas"]["CreateServerGroupRequest"];
export type Database = components["schemas"]["Database"];
export type DatabaseLimited = components["schemas"]["DatabaseLimited"];
export type CreateDatabaseRequest =
  components["schemas"]["CreateDatabaseRequest"];
export type UpdateDatabaseRequest =
  components["schemas"]["UpdateDatabaseRequest"];
export type AccessGrant = components["schemas"]["AccessGrant"];
export type AssignGrantRequest = components["schemas"]["AssignGrantRequest"];
export type GrantDefinition = components["schemas"]["GrantDefinition"];
export type CreateGrantDefinitionRequest =
  components["schemas"]["CreateGrantDefinitionRequest"];
export type UpdateGrantDefinitionRequest =
  components["schemas"]["UpdateGrantDefinitionRequest"];
export type ValidatePatternsRequest =
  components["schemas"]["ValidatePatternsRequest"];
export type ValidatePatternsResponse =
  components["schemas"]["ValidatePatternsResponse"];
export type PatternValidationResult =
  components["schemas"]["PatternValidationResult"];
export type QueryValidationResult =
  components["schemas"]["QueryValidationResult"];
export type GrantRequest = components["schemas"]["GrantRequest"];
export type CreateGrantRequestPayload =
  components["schemas"]["CreateGrantRequestPayload"];
export type Connection = components["schemas"]["Connection"];
export type ConnectionDetail = components["schemas"]["ConnectionDetail"];
export type GrantSummary = components["schemas"]["GrantSummary"];
export type Query = components["schemas"]["Query"];
export type QueryWithRows = components["schemas"]["QueryWithRows"];
export type AuditEvent = components["schemas"]["AuditEvent"];
export type APIKey = components["schemas"]["APIKey"];
export type CreateAPIKeyRequest = components["schemas"]["CreateAPIKeyRequest"];
export type CreateAPIKeyResponse =
  components["schemas"]["CreateAPIKeyResponse"];
export type ConnectionInfo = components["schemas"]["ConnectionInfo"];
export type ConnectionTestResult =
  components["schemas"]["ConnectionTestResult"];
export type DeviceConsentInfo = components["schemas"]["DeviceConsentInfo"];
export type ChainBreak = components["schemas"]["ChainBreak"];
export type AuditChainVerification =
  components["schemas"]["AuditChainVerification"];
export type QueryChainVerification =
  components["schemas"]["QueryChainVerification"];
export type RowChainVerification =
  components["schemas"]["RowChainVerification"];

// ============================================================================
// Auth Providers
// ============================================================================

export function useAuthProviders() {
  return useQuery({
    queryKey: ["auth-providers"],
    queryFn: async () => {
      const { data, error } = await apiClient.GET("/auth/providers");
      if (error) throw error;
      return data?.providers ?? [];
    },
    staleTime: 5 * 60 * 1000, // Cache for 5 minutes
  });
}

// ============================================================================
// Users
// ============================================================================

export function useUsers() {
  return useQuery({
    queryKey: ["users"],
    queryFn: async (): Promise<User[]> => {
      const response = await apiClient.GET("/users");
      if (response.error) {
        throw new Error(response.error.message || "Failed to load users");
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
        throw new Error(response.error?.message || "Failed to load user");
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
        throw new Error(response.error?.message || "Failed to create user");
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
        throw new Error(response.error.message || "Failed to update user");
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
        throw new Error(response.error.message || "Failed to delete user");
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
      const response = await apiClient.GET("/servers");
      if (response.error) {
        throw new Error(response.error.message || "Failed to load databases");
      }
      return response.data?.databases || [];
    },
  });
}

export function useDatabase(uid: string) {
  return useQuery({
    queryKey: ["databases", uid],
    queryFn: async (): Promise<Database | DatabaseLimited> => {
      const response = await apiClient.GET("/servers/{uid}", {
        params: { path: { uid } },
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.message || "Failed to load database");
      }
      return response.data;
    },
    enabled: !!uid,
  });
}

export function useDatabaseConnection(uid: string | undefined) {
  return useQuery({
    queryKey: ["databases", uid, "connection"],
    queryFn: async (): Promise<ConnectionInfo> => {
      const response = await apiClient.GET("/servers/{uid}/connection", {
        params: { path: { uid: uid! } },
      });
      if (response.error || !response.data) {
        throw Object.assign(
          new Error(
            (response.error as { message?: string })?.message ||
              "Failed to load connection URL"
          ),
          { status: response.response?.status }
        );
      }
      return response.data;
    },
    enabled: !!uid,
    retry: false,
  });
}

export function useCreateDatabase(options?: {
  onSuccess?: (db: Database) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (data: CreateDatabaseRequest): Promise<Database> => {
      const response = await apiClient.POST("/servers", {
        body: data,
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.message || "Failed to create database");
      }
      return response.data;
    },
    onSuccess: (db) => {
      queryClient.invalidateQueries({ queryKey: ["databases"] });
      queryClient.invalidateQueries({ queryKey: ["ssh-servers"] });
      queryClient.invalidateQueries({ queryKey: ["tunnel-servers"] });
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
      const response = await apiClient.PUT("/servers/{uid}", {
        params: { path: { uid } },
        body: data,
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to update database");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["databases"] });
      queryClient.invalidateQueries({ queryKey: ["databases", uid] });
      queryClient.invalidateQueries({ queryKey: ["ssh-servers"] });
      queryClient.invalidateQueries({ queryKey: ["tunnel-servers"] });
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
      const response = await apiClient.DELETE("/servers/{uid}", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to delete database");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["databases"] });
      queryClient.invalidateQueries({ queryKey: ["ssh-servers"] });
      queryClient.invalidateQueries({ queryKey: ["tunnel-servers"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

// Connectivity check: dials the server for real (SSH handshake for a bastion,
// tunnel + database login for a target) and returns the staged outcome. A
// failed check is a successful request with `ok: false` — only a transport or
// authorization failure rejects.
export function useTestServerConnection(uid: string) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (): Promise<ConnectionTestResult> => {
      const response = await apiClient.POST("/servers/{uid}/test", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to test the connection"
        );
      }
      return response.data as ConnectionTestResult;
    },
    onSuccess: () => {
      // A first successful bastion check pins the host key, so the row changed.
      queryClient.invalidateQueries({ queryKey: ["ssh-servers"] });
      queryClient.invalidateQueries({ queryKey: ["tunnel-servers"] });
      queryClient.invalidateQueries({ queryKey: ["databases"] });
    },
  });
}

// SSH servers (bastions). These are excluded from the regular database list;
// used by the "via SSH server" selector and admin SSH management.
export function useSSHServers(enabled = true) {
  return useQuery({
    queryKey: ["ssh-servers"],
    queryFn: async (): Promise<Database[]> => {
      const response = await apiClient.GET("/ssh-servers");
      if (response.error) {
        throw new Error(response.error.message || "Failed to load ssh servers");
      }
      return response.data?.servers || [];
    },
    enabled,
  });
}

// Tunnel (dial-path) servers: every ssh bastion *and* kubernetes cluster row.
// This is what the target form's "via" selector reads — a target may be dialed
// through either kind, so a bastion-only list would hide half the options.
export function useTunnelServers(enabled = true) {
  return useQuery({
    queryKey: ["tunnel-servers"],
    queryFn: async (): Promise<Database[]> => {
      const response = await apiClient.GET("/tunnel-servers");
      if (response.error) {
        throw new Error(response.error.message || "Failed to load tunnel servers");
      }
      return response.data?.servers || [];
    },
    enabled,
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
        throw new Error(response.error.message || "Failed to load grants");
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
        throw new Error(response.error?.message || "Failed to load grant");
      }
      return response.data;
    },
    enabled: !!uid,
  });
}

/**
 * Issues a grant by instantiating a grant definition. There is no ad-hoc grant
 * creation any more: the shape (controls, quotas, approval gating) and the
 * window's length all come from the definition.
 */
export function useAssignGrant(options?: {
  onSuccess?: (grant: AccessGrant) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (data: AssignGrantRequest): Promise<AccessGrant> => {
      const response = await apiClient.POST("/grants", {
        body: data,
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.message || "Failed to assign grant");
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
        throw new Error(response.error.message || "Failed to revoke grant");
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
// User Groups
// ============================================================================

export function useUserGroups(options?: { enabled?: boolean }) {
  return useQuery({
    queryKey: ["user-groups"],
    queryFn: async (): Promise<UserGroup[]> => {
      const response = await apiClient.GET("/user-groups");
      if (response.error) {
        throw new Error(response.error.message || "Failed to load user groups");
      }
      return response.data?.user_groups || [];
    },
    enabled: options?.enabled ?? true,
  });
}

export function useCreateUserGroup(options?: {
  onSuccess?: (group: UserGroup) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (data: CreateUserGroupRequest): Promise<UserGroup> => {
      const response = await apiClient.POST("/user-groups", { body: data });
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Failed to create user group"
        );
      }
      return response.data;
    },
    onSuccess: (group) => {
      queryClient.invalidateQueries({ queryKey: ["user-groups"] });
      queryClient.invalidateQueries({ queryKey: ["users"] });
      options?.onSuccess?.(group);
    },
    onError: options?.onError,
  });
}

export function useUpdateUserGroup(options?: {
  onSuccess?: (group: UserGroup) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (args: {
      uid: string;
      body: CreateUserGroupRequest;
    }): Promise<UserGroup> => {
      const response = await apiClient.PATCH("/user-groups/{uid}", {
        params: { path: { uid: args.uid } },
        body: args.body,
      });
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Failed to update user group"
        );
      }
      return response.data;
    },
    onSuccess: (group) => {
      queryClient.invalidateQueries({ queryKey: ["user-groups"] });
      queryClient.invalidateQueries({ queryKey: ["users"] });
      options?.onSuccess?.(group);
    },
    onError: options?.onError,
  });
}

export function useDeleteUserGroup(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<void> => {
      const response = await apiClient.DELETE("/user-groups/{uid}", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to delete user group"
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["user-groups"] });
      queryClient.invalidateQueries({ queryKey: ["users"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

// ============================================================================
// Server Groups
// ============================================================================

export function useServerGroups(options?: { enabled?: boolean }) {
  return useQuery({
    queryKey: ["server-groups"],
    queryFn: async (): Promise<ServerGroup[]> => {
      const response = await apiClient.GET("/server-groups");
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to load server groups"
        );
      }
      return response.data?.server_groups || [];
    },
    enabled: options?.enabled ?? true,
  });
}

export function useCreateServerGroup(options?: {
  onSuccess?: (group: ServerGroup) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (data: CreateServerGroupRequest): Promise<ServerGroup> => {
      const response = await apiClient.POST("/server-groups", { body: data });
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Failed to create server group"
        );
      }
      return response.data;
    },
    onSuccess: (group) => {
      queryClient.invalidateQueries({ queryKey: ["server-groups"] });
      // Membership is live, so a change here changes what live grants cover.
      queryClient.invalidateQueries({ queryKey: ["grants"] });
      options?.onSuccess?.(group);
    },
    onError: options?.onError,
  });
}

export function useUpdateServerGroup(options?: {
  onSuccess?: (group: ServerGroup) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (args: {
      uid: string;
      body: CreateServerGroupRequest;
    }): Promise<ServerGroup> => {
      const response = await apiClient.PATCH("/server-groups/{uid}", {
        params: { path: { uid: args.uid } },
        body: args.body,
      });
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Failed to update server group"
        );
      }
      return response.data;
    },
    onSuccess: (group) => {
      queryClient.invalidateQueries({ queryKey: ["server-groups"] });
      queryClient.invalidateQueries({ queryKey: ["grants"] });
      options?.onSuccess?.(group);
    },
    onError: options?.onError,
  });
}

export function useDeleteServerGroup(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<void> => {
      const response = await apiClient.DELETE("/server-groups/{uid}", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to delete server group"
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["server-groups"] });
      queryClient.invalidateQueries({ queryKey: ["grants"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

// ============================================================================
// Grant Definitions
// ============================================================================

export function useGrantDefinitions(filters?: { active_only?: boolean }) {
  return useQuery({
    queryKey: ["grant-definitions", filters],
    queryFn: async (): Promise<GrantDefinition[]> => {
      const response = await apiClient.GET("/grant-definitions", {
        params: { query: filters },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to load grant definitions"
        );
      }
      return response.data?.grant_definitions || [];
    },
  });
}

export function useCreateGrantDefinition(options?: {
  onSuccess?: (def: GrantDefinition) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (
      data: CreateGrantDefinitionRequest
    ): Promise<GrantDefinition> => {
      const response = await apiClient.POST("/grant-definitions", {
        body: data,
      });
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Failed to create grant definition"
        );
      }
      return response.data;
    },
    onSuccess: (def) => {
      queryClient.invalidateQueries({ queryKey: ["grant-definitions"] });
      options?.onSuccess?.(def);
    },
    onError: options?.onError,
  });
}

export function useUpdateGrantDefinition(options?: {
  onSuccess?: (def: GrantDefinition) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (args: {
      uid: string;
      body: UpdateGrantDefinitionRequest;
    }): Promise<GrantDefinition> => {
      const response = await apiClient.PATCH("/grant-definitions/{uid}", {
        params: { path: { uid: args.uid } },
        body: args.body,
      });
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Failed to update grant definition"
        );
      }
      return response.data;
    },
    onSuccess: (def) => {
      queryClient.invalidateQueries({ queryKey: ["grant-definitions"] });
      // Grant requests embed the live version of their definition, so an edit
      // (which archives the current version and inserts a successor) changes
      // what every request against that lineage renders.
      queryClient.invalidateQueries({ queryKey: ["grant-requests"] });
      options?.onSuccess?.(def);
    },
    onError: options?.onError,
  });
}

/**
 * Previews a set of approval patterns against a bench of sample queries —
 * which patterns fail to compile, and which sample each successfully
 * compiled pattern would hold, with exactly ApprovalGate.Match's semantics.
 * Pure compute on the server: no store access, nothing persisted, and no
 * definition needs to exist yet — so this can run against whatever is
 * currently typed into the dialog, not just what's saved.
 */
export function useValidateGrantDefinitionPatterns(options?: {
  onError?: (error: Error) => void;
}) {
  return useMutation({
    mutationFn: async (
      data: ValidatePatternsRequest
    ): Promise<ValidatePatternsResponse> => {
      const response = await apiClient.POST(
        "/grant-definitions/validate-patterns",
        { body: data }
      );
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Failed to validate patterns"
        );
      }
      return response.data;
    },
    onError: options?.onError,
  });
}

/**
 * Deactivates a grant definition across its whole version lineage. This fails
 * closed: every grant issued from it stops authorising new connections, and
 * the response says how many that was.
 */
export function useDeactivateGrantDefinition(options?: {
  onSuccess?: (affectedGrants: number) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<number> => {
      const response = await apiClient.DELETE("/grant-definitions/{uid}", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to deactivate grant definition"
        );
      }
      return response.data?.affected_grants ?? 0;
    },
    onSuccess: (affectedGrants) => {
      queryClient.invalidateQueries({ queryKey: ["grant-definitions"] });
      // A definition edit versions the row, and deactivation reaches every
      // grant issued from it, so the grants list is stale either way.
      queryClient.invalidateQueries({ queryKey: ["grants"] });
      options?.onSuccess?.(affectedGrants);
    },
    onError: options?.onError,
  });
}

// ============================================================================
// Grant Requests
// ============================================================================

export function useGrantRequests(filters?: {
  status?: "pending" | "approved" | "denied" | "cancelled" | "expired";
  user_id?: string;
  database_id?: string;
}) {
  return useQuery({
    queryKey: ["grant-requests", filters],
    queryFn: async (): Promise<GrantRequest[]> => {
      const response = await apiClient.GET("/grant-requests", {
        params: { query: filters as Record<string, unknown> },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to load grant requests"
        );
      }
      return response.data?.grant_requests || [];
    },
  });
}

export function useCreateGrantRequest(options?: {
  onSuccess?: (req: GrantRequest) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (
      data: CreateGrantRequestPayload
    ): Promise<GrantRequest> => {
      const response = await apiClient.POST("/grant-requests", { body: data });
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Failed to submit grant request"
        );
      }
      return response.data;
    },
    onSuccess: (req) => {
      queryClient.invalidateQueries({ queryKey: ["grant-requests"] });
      options?.onSuccess?.(req);
    },
    onError: options?.onError,
  });
}

export function useApproveGrantRequest(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<void> => {
      const response = await apiClient.POST(
        "/grant-requests/{uid}/approve",
        { params: { path: { uid } } }
      );
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to approve grant request"
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["grant-requests"] });
      queryClient.invalidateQueries({ queryKey: ["grants"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

export function useDenyGrantRequest(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (args: {
      uid: string;
      reason?: string;
    }): Promise<void> => {
      const response = await apiClient.POST(
        "/grant-requests/{uid}/deny",
        {
          params: { path: { uid: args.uid } },
          body: { reason: args.reason ?? "" },
        }
      );
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to deny grant request"
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["grant-requests"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

export function useCancelGrantRequest(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<void> => {
      const response = await apiClient.POST(
        "/grant-requests/{uid}/cancel",
        { params: { path: { uid } } }
      );
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to cancel grant request"
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["grant-requests"] });
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
  before?: string;
  limit?: number;
  offset?: number;
}) {
  return useQuery({
    queryKey: ["connections", filters],
    queryFn: async (): Promise<Connection[]> => {
      const response = await apiClient.GET("/connections", {
        params: { query: filters as Record<string, unknown> },
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to load connections");
      }
      return response.data?.connections || [];
    },
  });
}

export function useConnection(uid: string) {
  return useQuery({
    queryKey: ["connections", uid],
    queryFn: async (): Promise<ConnectionDetail> => {
      const response = await apiClient.GET("/connections/{uid}", {
        params: { path: { uid } },
      });
      if (response.error || !response.data) {
        throw new Error(response.error?.message || "Failed to load connection");
      }
      return response.data;
    },
    enabled: !!uid,
  });
}

// useDownloadConnectionDump fetches the raw pcapng session capture through
// the authenticated API client (so the bearer token and the 401 -> login
// redirect middleware both apply, unlike a bare <a href> to the API) and
// hands the resulting blob to the browser's native download flow.
//
// A capture swept by retention (or deleted) between page load and click
// surfaces as a 404, which is worded distinctly so it reads as "gone", not
// as a generic failure.
export function useDownloadConnectionDump(
  uid: string,
  options?: { onError?: (error: Error) => void }
) {
  return useMutation({
    mutationFn: async (): Promise<void> => {
      const response = await apiClient.GET("/connections/{uid}/dump", {
        params: { path: { uid } },
        parseAs: "blob",
      });
      if (response.error || !response.data) {
        throw new Error(
          response.response?.status === 404
            ? "Capture is no longer available"
            : (response.error as { message?: string })?.message ||
                "Failed to download capture"
        );
      }
      downloadBlob(response.data, `${uid}.pcapng`);
    },
    onError: options?.onError,
  });
}

// ============================================================================
// Queries
// ============================================================================

export function useQueries(
  filters?: {
    connection_id?: string;
    user_id?: string;
    database_id?: string;
    start_time?: string;
    end_time?: string;
    before?: string;
    limit?: number;
    offset?: number;
  },
  options?: { enabled?: boolean }
) {
  return useQuery({
    queryKey: ["queries", filters],
    queryFn: async (): Promise<Query[]> => {
      const response = await apiClient.GET("/queries", {
        params: { query: filters as Record<string, unknown> },
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to load queries");
      }
      return response.data?.queries || [];
    },
    enabled: options?.enabled,
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
        throw new Error(response.error?.message || "Failed to load query");
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
        throw new Error(response.error?.message || "Failed to load query rows");
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
  before?: string;
  limit?: number;
  offset?: number;
}) {
  return useQuery({
    queryKey: ["audit", filters],
    queryFn: async (): Promise<AuditEvent[]> => {
      const response = await apiClient.GET("/audit", {
        params: { query: filters as Record<string, unknown> },
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to load audit events");
      }
      return response.data?.audit_events || [];
    },
  });
}

// ============================================================================
// Audit chain verification (admin-only endpoints)
// ============================================================================

/**
 * Walk the store-wide `audit_log` chain.
 *
 * A mutation rather than a query: verification is an explicit operator action
 * (a walk is O(rows)), never something a page load should trigger.
 */
export function useVerifyAuditChain() {
  return useMutation({
    mutationFn: async (): Promise<AuditChainVerification> => {
      const response = await apiClient.GET("/audit/verify");
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to verify the audit chain",
        );
      }
      return response.data;
    },
  });
}

/** Walk the per-connection query chains (store-wide when unscoped). */
export function useVerifyQueryChains() {
  return useMutation({
    mutationFn: async (
      params?: { connection?: string },
    ): Promise<QueryChainVerification> => {
      const response = await apiClient.GET("/audit/verify/queries", {
        params: { query: params?.connection ? { connection: params.connection } : {} },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to verify the query chains",
        );
      }
      return response.data;
    },
  });
}

/** Walk the per-capture result-row chains (store-wide when unscoped). */
export function useVerifyRowChains() {
  return useMutation({
    mutationFn: async (
      params?: { connection?: string; query?: string },
    ): Promise<RowChainVerification> => {
      const query: Record<string, string> = {};
      if (params?.connection) query.connection = params.connection;
      if (params?.query) query.query = params.query;
      const response = await apiClient.GET("/audit/verify/rows", {
        params: { query },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to verify the row chains",
        );
      }
      return response.data;
    },
  });
}

// ============================================================================
// API Keys
// ============================================================================

export function useAPIKeys(filters?: {
  user_id?: string;
  all_users?: boolean;
  include_all?: boolean;
}) {
  return useQuery({
    queryKey: ["api-keys", filters],
    queryFn: async (): Promise<APIKey[]> => {
      const response = await apiClient.GET("/keys", {
        params: { query: filters },
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to load API keys");
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
        throw new Error(response.error?.message || "Failed to load API key");
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
        throw new Error(response.error?.message || "Failed to create API key");
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
        throw new Error(response.error.message || "Failed to revoke API key");
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
// Device Authorization (RFC 8628)
// ============================================================================

export function useDeviceConsent(userCode: string) {
  return useQuery({
    queryKey: ["device-consent", userCode],
    queryFn: async (): Promise<DeviceConsentInfo> => {
      const response = await apiClient.GET("/auth/device/consent", {
        params: { query: { user_code: userCode } },
      });
      if (response.error || !response.data) {
        throw new Error(
          response.error?.message || "Device authorization request not found"
        );
      }
      return response.data;
    },
    enabled: !!userCode,
    retry: false,
  });
}

export function useRespondToDeviceConsent(
  userCode: string,
  options?: {
    onSuccess?: () => void;
    onError?: (error: Error) => void;
  }
) {
  return useMutation({
    mutationFn: async (approve: boolean): Promise<void> => {
      const response = await apiClient.POST("/auth/device/consent", {
        body: { user_code: userCode, approve },
      });
      if (response.error) {
        throw new Error(
          response.error.message ||
            "Failed to respond to device authorization request"
        );
      }
    },
    onSuccess: options?.onSuccess,
    onError: options?.onError,
  });
}

// ============================================================================
// Instance & Parameters
// ============================================================================

export type GlobalParameter = components["schemas"]["GlobalParameter"];
export type PublicEndpoints = components["schemas"]["PublicEndpoints"];
export type ResolvedEndpoints = components["schemas"]["ResolvedEndpoints"];
export type InstanceInfo = components["schemas"]["InstanceInfo"];

export function useInstance() {
  return useQuery({
    queryKey: ["instance"],
    queryFn: async (): Promise<InstanceInfo> => {
      const response = await apiClient.GET("/instance");
      if (response.error || !response.data) {
        throw new Error(response.error?.message || "Failed to load instance info");
      }
      return response.data;
    },
  });
}

export function useUpdateInstancePublic(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: PublicEndpoints) => {
      const response = await apiClient.PUT("/instance/public", { body });
      if (response.error) {
        throw new Error(
          (response.error as { message?: string }).message ||
            "Failed to save settings"
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["instance"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

export function useParameters(groupKey?: string) {
  return useQuery({
    queryKey: ["parameters", groupKey],
    queryFn: async (): Promise<GlobalParameter[]> => {
      const response = await apiClient.GET("/parameters", {
        params: { query: groupKey ? { group_key: groupKey } : undefined },
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to load parameters");
      }
      return response.data ?? [];
    },
  });
}

export function useUpdateParameter(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({
      group,
      key,
      value,
    }: {
      group: string;
      key: string;
      value: string;
    }) => {
      const response = await apiClient.PUT("/parameters/{group}/{key}", {
        params: { path: { group, key } },
        body: { value },
      });
      if (response.error) {
        throw new Error(
          (response.error as { message?: string }).message ||
            "Failed to update parameter"
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["parameters"] });
      options?.onSuccess?.();
    },
    onError: options?.onError,
  });
}

export function useDeleteParameter(options?: {
  onSuccess?: () => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ group, key }: { group: string; key: string }) => {
      const response = await apiClient.DELETE("/parameters/{group}/{key}", {
        params: { path: { group, key } },
      });
      if (response.error) {
        throw new Error(
          (response.error as { message?: string }).message ||
            "Failed to delete parameter"
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["parameters"] });
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
        throw new Error(response.error.message || "Service unhealthy");
      }
      return response.data;
    },
    retry: false,
  });
}

// ============================================================================
// Version
// ============================================================================

export type VersionInfo = components["schemas"]["VersionInfo"];

export function useVersion() {
  return useQuery({
    queryKey: ["version"],
    queryFn: async (): Promise<VersionInfo> => {
      const response = await apiClient.GET("/version");
      if (response.error || !response.data) {
        throw new Error("Failed to fetch version info");
      }
      return response.data;
    },
    staleTime: 5 * 60 * 1000, // 5 minutes - version info rarely changes
    retry: false,
  });
}

// ============================================================================
// Approval holds
//
// A query matching one of the grant's approval patterns is parked mid-flight
// until a second human resolves it. There is no timeout: a hold ends on
// approve, deny, or the client giving up (which shows as `abandoned`, and must
// read differently from `denied` — nothing ran, and nobody is waiting anymore).
// ============================================================================

/**
 * The set of queries currently awaiting a decision. This is authoritative;
 * the live stream is only a hint that it changed, so every stream gap and
 * reconnect refetches this.
 */
export function usePendingApprovals(options?: {
  enabled?: boolean;
  refetchInterval?: number | false;
}) {
  return useQuery({
    queryKey: ["queries", "pending"],
    queryFn: async (): Promise<Query[]> => {
      const response = await apiClient.GET("/queries/pending");
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to load pending approvals",
        );
      }
      return response.data?.queries || [];
    },
    enabled: options?.enabled ?? true,
    refetchInterval: options?.refetchInterval ?? false,
    retry: false,
  });
}

/** Releases a held statement; it then runs. Self-approval is refused (403). */
export function useApproveQuery(options?: { onSuccess?: () => void }) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (uid: string): Promise<void> => {
      const response = await apiClient.POST("/queries/{uid}/approve", {
        params: { path: { uid } },
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to approve query");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["queries"] });
      options?.onSuccess?.();
    },
  });
}

/** Rejects a held statement; the client gets the reason as a native error. */
export function useDenyQuery(options?: { onSuccess?: () => void }) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (vars: {
      uid: string;
      reason?: string;
    }): Promise<void> => {
      const response = await apiClient.POST("/queries/{uid}/deny", {
        params: { path: { uid: vars.uid } },
        body: { reason: vars.reason },
      });
      if (response.error) {
        throw new Error(response.error.message || "Failed to deny query");
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["queries"] });
      options?.onSuccess?.();
    },
  });
}

/**
 * Bulk deny — the safety valve for "clear every hold now". Admin only.
 */
export function useDenyAllPendingApprovals(options?: {
  onSuccess?: () => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (reason?: string): Promise<void> => {
      const response = await apiClient.POST("/queries/pending/deny-all", {
        body: { reason },
      });
      if (response.error) {
        throw new Error(
          response.error.message || "Failed to deny pending approvals",
        );
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["queries"] });
      options?.onSuccess?.();
    },
  });
}
