/**
 * Role-based permission utilities for frontend UI restrictions.
 *
 * Note: These are UI/UX helpers only. All security enforcement happens
 * on the backend. These functions help provide clear visual feedback
 * about permission limitations.
 */

export type UserRole = 'admin' | 'viewer' | 'connector';

/**
 * Check if a user has a specific role
 */
export const hasRole = (roles: string[] | undefined, role: UserRole): boolean => {
  if (!roles) return false;
  return roles.includes(role);
};

/**
 * Check if user has any of the specified roles
 */
export const hasAnyRole = (roles: string[] | undefined, requiredRoles: UserRole[]): boolean => {
  if (!roles) return false;
  return requiredRoles.some(role => roles.includes(role));
};

/**
 * Check if user has all of the specified roles
 */
export const hasAllRoles = (roles: string[] | undefined, requiredRoles: UserRole[]): boolean => {
  if (!roles) return false;
  return requiredRoles.every(role => roles.includes(role));
};

// Navigation permissions
export const canViewQueries = (roles: string[] | undefined): boolean => hasAnyRole(roles, ['admin', 'viewer']);
export const canViewAudit = (roles: string[] | undefined): boolean => hasAnyRole(roles, ['admin', 'viewer']);

// Admin-only permissions
export const canCreateUser = (roles: string[] | undefined): boolean => hasRole(roles, 'admin');
export const canCreateDatabase = (roles: string[] | undefined): boolean => hasRole(roles, 'admin');
export const canCreateGrant = (roles: string[] | undefined): boolean => hasRole(roles, 'admin');
export const canRevokeGrant = (roles: string[] | undefined): boolean => hasRole(roles, 'admin');
export const canDeleteUser = (roles: string[] | undefined): boolean => hasRole(roles, 'admin');
export const canDeleteDatabase = (roles: string[] | undefined): boolean => hasRole(roles, 'admin');
export const canUpdateUser = (roles: string[] | undefined): boolean => hasRole(roles, 'admin');
export const canUpdateDatabase = (roles: string[] | undefined): boolean => hasRole(roles, 'admin');

// API Key permissions (admin or connector, but not viewer-only)
export const canCreateAPIKey = (roles: string[] | undefined): boolean => hasAnyRole(roles, ['admin', 'connector']);

export const canRevokeAPIKey = (roles: string[] | undefined): boolean => hasAnyRole(roles, ['admin', 'connector']);

/**
 * Get tooltip message explaining why a button is disabled
 */
export const getDisabledReason = (action: string, _roles?: string[]): string => {
  const roleMessages: Record<string, string> = {
    'create-user': 'Only administrators can create users',
    'create-database': 'Only administrators can create databases',
    'create-grant': 'Only administrators can create grants',
    'create-api-key': 'Viewers cannot create API keys',
    'revoke-grant': 'Only administrators can revoke grants',
    'revoke-api-key': 'Viewers cannot revoke API keys',
    'delete-user': 'Only administrators can delete users',
    'delete-database': 'Only administrators can delete databases',
    'update-user': 'Only administrators can update users',
    'update-database': 'Only administrators can update databases',
  };

  return roleMessages[action] || 'You do not have permission for this action';
};

/**
 * Get tooltip message for enabled action buttons
 */
export const getActionTooltip = (action: string): string => {
  const actionMessages: Record<string, string> = {
    'create-user': 'Create a new user',
    'create-database': 'Add a new database configuration',
    'create-grant': 'Grant database access to a user',
    'create-api-key': 'Create a new API key',
    'revoke-grant': 'Revoke this grant',
    'revoke-api-key': 'Revoke this API key',
    'delete-user': 'Delete this user',
    'delete-database': 'Delete this database',
    'update-user': 'Update user details',
    'update-database': 'Update database configuration',
  };

  return actionMessages[action] || 'Perform this action';
};
