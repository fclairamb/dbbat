# Frontend Role-Based UI Restrictions

## Status: Draft

## Summary

Implement role-based UI restrictions on the frontend to provide clear visual feedback about permission limitations, prevent unauthorized navigation attempts, and guide users to understand their capabilities through disabled buttons with explanatory tooltips.

## Context

The backend implements three independent roles (`admin`, `viewer`, `connector`) with different capabilities. The frontend must reflect these restrictions in the UI to:
1. Prevent wasted navigation attempts to restricted endpoints
2. Provide clear visual feedback about permission limitations
3. Educate users about why certain actions are unavailable
4. Maintain a professional, informative user experience

## Requirements

### Navigation Restrictions

Users **without** the `viewer` role must not attempt to navigate to:
- `/queries` - Query history and result data
- `/audit` - Audit log events

**Implementation:**
- Hide or disable navigation links to these pages for non-viewer users
- If a user manually navigates to these URLs (e.g., via browser history), show an access denied page with clear explanation
- Access denied page should explain: "This page requires the viewer role. Contact your administrator if you need access."

### Button State Management

Buttons should be **disabled** (greyed out, non-clickable) based on roles:

| Button | Available to | Disabled for | Tooltip when disabled |
|--------|-------------|--------------|----------------------|
| **Create User** | `admin` only | All non-admins | "Only administrators can create users" |
| **Create Database** | `admin` only | All non-admins | "Only administrators can create databases" |
| **Create Grant** | `admin` only | All non-admins | "Only administrators can create grants" |
| **Create API Key** | `admin`, `connector` | `viewer` only | "Viewers cannot create API keys" |
| **Revoke Grant** | `admin` only | All non-admins | "Only administrators can revoke grants" (when disabled)<br>"Revoke this grant" (when enabled) |
| **Revoke API Key** | `admin`, `connector` | `viewer` only | "Viewers cannot revoke API keys" (when disabled)<br>"Revoke this API key" (when enabled) |
| **Delete User** | `admin` only | All non-admins | "Only administrators can delete users" |
| **Delete Database** | `admin` only | All non-admins | "Only administrators can delete databases" |

### Tooltip Behavior

**For disabled buttons:**
- Show tooltip immediately on hover (no delay)
- Tooltip explains why the button is disabled
- Use informative, non-technical language
- Keep messages concise (under 50 characters when possible)

**For enabled action buttons:**
- Show tooltip on hover with action confirmation
- Examples: "Revoke this grant", "Delete this user", "Create a new database"
- Helps users understand what will happen when they click

### Visual Design

**Disabled button appearance:**
- Grey text color (reduced opacity: ~40%)
- Grey background or border (reduced opacity: ~40%)
- Cursor: `not-allowed`
- No hover state change (remains grey)

**Tooltip styling:**
- Dark background with white text
- Small font size (12-14px)
- Appears above or below the button (whichever has more space)
- Slight delay for enabled buttons (300ms)
- Immediate appearance for disabled buttons (0ms)

## Implementation Details

### Role Check Utility

Create a frontend utility to check user roles:

```typescript
// utils/permissions.ts
export const hasRole = (user: User, role: 'admin' | 'viewer' | 'connector'): boolean => {
  return user.roles.includes(role);
};

export const canCreateUser = (user: User): boolean => hasRole(user, 'admin');
export const canCreateDatabase = (user: User): boolean => hasRole(user, 'admin');
export const canCreateGrant = (user: User): boolean => hasRole(user, 'admin');
export const canCreateAPIKey = (user: User): boolean =>
  hasRole(user, 'admin') || hasRole(user, 'connector');
export const canViewQueries = (user: User): boolean => hasRole(user, 'viewer');
export const canViewAudit = (user: User): boolean => hasRole(user, 'viewer');
export const canRevokeGrant = (user: User): boolean => hasRole(user, 'admin');
export const canDeleteUser = (user: User): boolean => hasRole(user, 'admin');
export const canDeleteDatabase = (user: User): boolean => hasRole(user, 'admin');
```

### Button Component Pattern

Create a reusable `PermissionButton` component:

```typescript
interface PermissionButtonProps {
  onClick: () => void;
  disabled?: boolean;
  disabledReason?: string; // Tooltip when disabled
  enabledTooltip?: string; // Tooltip when enabled
  children: React.ReactNode;
}

const PermissionButton = ({
  onClick,
  disabled,
  disabledReason,
  enabledTooltip,
  children
}: PermissionButtonProps) => {
  const tooltip = disabled ? disabledReason : enabledTooltip;

  return (
    <Tooltip content={tooltip} delay={disabled ? 0 : 300}>
      <button
        onClick={onClick}
        disabled={disabled}
        className={disabled ? 'btn-disabled' : 'btn-primary'}
        style={{ cursor: disabled ? 'not-allowed' : 'pointer' }}
      >
        {children}
      </button>
    </Tooltip>
  );
};
```

### Navigation Guard

Implement a navigation guard for restricted routes:

```typescript
// components/ProtectedRoute.tsx
const ProtectedRoute = ({
  children,
  requireRole
}: {
  children: React.ReactNode;
  requireRole?: 'admin' | 'viewer' | 'connector';
}) => {
  const { user } = useAuth();

  if (requireRole && !hasRole(user, requireRole)) {
    return (
      <AccessDenied
        requiredRole={requireRole}
        message={`This page requires the ${requireRole} role.`}
      />
    );
  }

  return <>{children}</>;
};
```

### Route Configuration

Update route definitions to include role requirements:

```typescript
// routes.tsx
<Route path="/queries" element={
  <ProtectedRoute requireRole="viewer">
    <QueriesPage />
  </ProtectedRoute>
} />

<Route path="/audit" element={
  <ProtectedRoute requireRole="viewer">
    <AuditPage />
  </ProtectedRoute>
} />
```

### Navigation Menu

Conditionally render navigation items:

```typescript
// components/Navigation.tsx
const Navigation = () => {
  const { user } = useAuth();

  return (
    <nav>
      <NavLink to="/databases">Databases</NavLink>
      <NavLink to="/grants">Grants</NavLink>
      {hasRole(user, 'viewer') && (
        <>
          <NavLink to="/queries">Queries</NavLink>
          <NavLink to="/audit">Audit</NavLink>
        </>
      )}
      {hasRole(user, 'admin') && (
        <NavLink to="/users">Users</NavLink>
      )}
    </nav>
  );
};
```

## Example Scenarios

### Scenario 1: Connector User (Database Access Only)

**User roles:** `['connector']`

**UI State:**
- Navigation: Only sees "Databases" and "Grants" (shows their own grants only)
- "Create Database" button: Disabled, tooltip: "Only administrators can create databases"
- "Create Grant" button: Disabled, tooltip: "Only administrators can create grants"
- "Create API Key" button: Enabled, tooltip: "Create a new API key"
- Cannot navigate to `/queries` or `/audit`

### Scenario 2: Viewer User (Read-Only Monitoring)

**User roles:** `['viewer']`

**UI State:**
- Navigation: Sees "Databases", "Grants", "Queries", "Audit"
- "Create Database" button: Disabled, tooltip: "Only administrators can create databases"
- "Create API Key" button: Disabled, tooltip: "Viewers cannot create API keys"
- Can navigate to `/queries` and `/audit` to monitor activity
- All database details show name and description only (no connection details)

### Scenario 3: Admin User (Full Management)

**User roles:** `['admin']` (may also have `['admin', 'connector']`)

**UI State:**
- Navigation: Sees all sections (but not `/queries` or `/audit` unless they also have `viewer` role)
- All "Create" buttons: Enabled
- All "Delete" buttons: Enabled
- "Revoke" buttons: Enabled with tooltip "Revoke this grant"
- Full database connection details visible

### Scenario 4: Admin + Viewer User (Management + Monitoring)

**User roles:** `['admin', 'viewer']`

**UI State:**
- Navigation: Sees all sections including "Queries" and "Audit"
- All administrative actions enabled
- Full visibility into all activity logs
- Complete database connection details

## Testing

### Manual Test Cases

1. **Connector without viewer role**
   - Login as user with only `connector` role
   - Verify navigation does not show "Queries" or "Audit" links
   - Attempt to navigate to `/queries` manually → Should see access denied page
   - Verify "Create Database" button is disabled with correct tooltip

2. **Viewer without admin role**
   - Login as user with only `viewer` role
   - Verify "Create API Key" button is disabled with correct tooltip
   - Verify can navigate to `/queries` and `/audit`
   - Verify cannot see "Create User" button (or it's disabled)

3. **Admin role functionality**
   - Login as admin user
   - Verify all "Create" buttons are enabled
   - Hover over enabled "Revoke" button → Should show "Revoke this grant"
   - Verify can perform all administrative actions

4. **Tooltip behavior**
   - Hover over disabled button → Tooltip appears immediately
   - Hover over enabled button → Tooltip appears after 300ms delay
   - Move mouse away → Tooltip disappears

### Automated Tests

```typescript
describe('Role-based UI restrictions', () => {
  it('hides queries link for non-viewer users', () => {
    renderWithUser({ roles: ['connector'] });
    expect(screen.queryByText('Queries')).not.toBeInTheDocument();
  });

  it('disables create database button for non-admin', () => {
    renderWithUser({ roles: ['connector'] });
    const button = screen.getByText('Create Database');
    expect(button).toBeDisabled();
  });

  it('shows correct tooltip for disabled button', async () => {
    renderWithUser({ roles: ['viewer'] });
    const button = screen.getByText('Create API Key');
    await userEvent.hover(button);
    expect(await screen.findByText('Viewers cannot create API keys')).toBeInTheDocument();
  });
});
```

## Security Considerations

**Frontend restrictions are NOT security boundaries:**
- All security enforcement happens on the backend via API authorization
- Frontend restrictions are UX/UI improvements only
- Users with browser dev tools can still attempt API calls
- Backend must reject unauthorized requests with 403 Forbidden
- Frontend should gracefully handle 403 responses with clear error messages

**Defense in depth:**
1. Backend enforces role-based access control
2. Frontend hides/disables UI elements for better UX
3. Navigation guards provide immediate feedback
4. API error responses are handled gracefully

## Future Enhancements

1. **Contextual help links**: When showing disabled tooltips, include a link to documentation about roles
2. **Role request workflow**: Allow users to request additional roles via UI
3. **Capability indicators**: Show role badges next to user name in header
4. **Progressive disclosure**: Hide disabled buttons entirely for cleaner UI (with toggle to "show all")
5. **Bulk operations**: Extend restrictions to bulk action buttons
