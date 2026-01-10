# Frontend Application Specification

## Overview

Build a modern, pink-themed admin dashboard for DBBat using Bun, shadcn/ui, and TanStack Query with auto-generated TypeScript types from the OpenAPI specification.

## Technical Stack

| Technology | Version | Purpose |
|------------|---------|---------|
| Bun | Latest (1.x) | Runtime, package manager, bundler |
| React | 19.x | UI framework |
| Vite | 6.x+ | Build tool (via Bun) |
| TypeScript | 5.x | Type safety |
| Tailwind CSS | 4.x | Styling |
| shadcn/ui | Latest | Component library (new-york style) |
| TanStack Query | 5.x | Data fetching & caching |
| TanStack Router | 1.x | File-based routing |
| openapi-typescript | 7.x | Type generation from OpenAPI |
| openapi-fetch | 0.14.x | Type-safe API client |
| openapi-react-query | 0.5.x | TanStack Query integration |
| Lucide React | Latest | Icons |

## Project Structure

```
front/
├── public/
│   ├── logo.png              # DBBat logo (copied from root)
│   └── favicon.ico           # Favicon (generated from logo)
├── src/
│   ├── api/
│   │   ├── schema.ts         # Auto-generated OpenAPI types
│   │   ├── client.ts         # openapi-fetch client setup
│   │   ├── queries.ts        # TanStack Query hooks
│   │   └── index.ts          # Re-exports
│   ├── components/
│   │   ├── ui/               # shadcn components
│   │   ├── layout/
│   │   │   ├── AppLayout.tsx
│   │   │   ├── Sidebar.tsx
│   │   │   └── Header.tsx
│   │   └── shared/           # Reusable app components
│   │       ├── DataTable.tsx
│   │       ├── StatCard.tsx
│   │       └── LoadingSpinner.tsx
│   ├── contexts/
│   │   └── AuthContext.tsx   # Authentication state
│   ├── hooks/
│   │   └── useAuth.ts        # Auth hook
│   ├── lib/
│   │   └── utils.ts          # cn() and utilities
│   ├── routes/               # TanStack Router file-based routes
│   │   ├── __root.tsx
│   │   ├── index.tsx         # Dashboard
│   │   ├── login.tsx
│   │   ├── users/
│   │   │   ├── index.tsx     # List users
│   │   │   └── $uid.tsx      # User details
│   │   ├── databases/
│   │   │   ├── index.tsx     # List databases
│   │   │   └── $uid.tsx      # Database details
│   │   ├── grants/
│   │   │   └── index.tsx     # List grants
│   │   ├── connections/
│   │   │   └── index.tsx     # List connections
│   │   ├── queries/
│   │   │   ├── index.tsx     # List queries
│   │   │   └── $uid.tsx      # Query details with rows
│   │   ├── audit/
│   │   │   └── index.tsx     # Audit log
│   │   └── api-keys/
│   │       └── index.tsx     # API keys management
│   ├── App.tsx
│   ├── main.tsx
│   ├── index.css             # Tailwind + theme variables
│   └── routeTree.gen.ts      # Auto-generated route tree
├── .env.example
├── bunfig.toml               # Bun configuration
├── components.json           # shadcn configuration
├── eslint.config.js
├── package.json
├── tsconfig.json
├── tsconfig.app.json
├── tsconfig.node.json
└── vite.config.ts
```

## Design System

### Brand Colors (Pink Theme)

The theme is inspired by the DBBat logo: a cute pink bat holding a database with a lock. The color palette should be warm, friendly, and professional.

```css
:root {
  /* Primary: Vibrant pink from logo */
  --primary: oklch(0.65 0.2 350);
  --primary-foreground: oklch(0.98 0.01 350);

  /* Accent: Deeper magenta */
  --accent: oklch(0.55 0.22 330);
  --accent-foreground: oklch(0.98 0.01 350);

  /* Background: Very light pink wash */
  --background: oklch(0.98 0.015 350);
  --foreground: oklch(0.15 0.05 350);

  /* Card: Slightly more saturated than background */
  --card: oklch(0.97 0.02 350);
  --card-foreground: oklch(0.15 0.05 350);

  /* Muted: Soft pink-gray */
  --muted: oklch(0.92 0.025 350);
  --muted-foreground: oklch(0.45 0.05 350);

  /* Border: Light pink border */
  --border: oklch(0.88 0.04 350);

  /* Sidebar: Slightly darker pink */
  --sidebar: oklch(0.94 0.035 350);
  --sidebar-foreground: oklch(0.15 0.05 350);
  --sidebar-primary: oklch(0.65 0.2 350);
  --sidebar-primary-foreground: oklch(0.98 0.01 350);
  --sidebar-accent: oklch(0.88 0.06 350);
  --sidebar-accent-foreground: oklch(0.15 0.05 350);
  --sidebar-border: oklch(0.85 0.05 350);

  /* Destructive: Red with pink undertone */
  --destructive: oklch(0.6 0.22 15);

  /* Chart colors */
  --chart-1: oklch(0.65 0.2 350);   /* Primary pink */
  --chart-2: oklch(0.6 0.18 320);   /* Magenta */
  --chart-3: oklch(0.7 0.15 10);    /* Coral */
  --chart-4: oklch(0.65 0.12 280);  /* Purple */
  --chart-5: oklch(0.75 0.1 40);    /* Peach */

  --radius: 0.625rem;
}

/* Dark mode - deeper pinks */
.dark {
  --primary: oklch(0.7 0.18 350);
  --primary-foreground: oklch(0.15 0.05 350);

  --background: oklch(0.15 0.03 350);
  --foreground: oklch(0.95 0.02 350);

  --card: oklch(0.2 0.04 350);
  --card-foreground: oklch(0.95 0.02 350);

  --muted: oklch(0.25 0.04 350);
  --muted-foreground: oklch(0.65 0.04 350);

  --border: oklch(0.3 0.05 350);

  --sidebar: oklch(0.18 0.035 350);
  --sidebar-foreground: oklch(0.95 0.02 350);
  --sidebar-border: oklch(0.28 0.04 350);
}
```

### Typography

- **Font**: System font stack (Inter-like)
- **Headings**: Semi-bold, slightly tighter letter-spacing
- **Body**: Regular weight, comfortable line-height

### Components Style

Use shadcn/ui `new-york` style variant:
- Rounded corners (`radius: 0.625rem`)
- Subtle shadows
- Lucide icons
- Clean, minimal aesthetic

## API Integration

### Type Generation

Generate types from the OpenAPI spec located at `internal/api/openapi.yml`:

```json
// package.json scripts
{
  "scripts": {
    "generate-client": "openapi-typescript ../internal/api/openapi.yml -o ./src/api/schema.ts --root-types --root-types-no-schema-prefix"
  }
}
```

### API Client Setup

```typescript
// src/api/client.ts
import createClient from 'openapi-fetch';
import type { paths } from './schema';

export const apiBaseUrl: string = import.meta.env.VITE_API_BASE_URL || '';

export const apiClient = createClient<paths>({
  baseUrl: apiBaseUrl
});

let authMiddleware: ReturnType<typeof apiClient.use> | null = null;

export const setBasicAuth = (username: string, password: string) => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
  }

  const credentials = btoa(`${username}:${password}`);
  authMiddleware = apiClient.use({
    onRequest({ request }) {
      request.headers.set('Authorization', `Basic ${credentials}`);
      return request;
    },
  });
};

export const setBearerAuth = (token: string) => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
  }

  authMiddleware = apiClient.use({
    onRequest({ request }) {
      request.headers.set('Authorization', `Bearer ${token}`);
      return request;
    },
  });
};

export const removeAuth = () => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
    authMiddleware = null;
  }
};
```

### Query Hooks Pattern

```typescript
// src/api/queries.ts
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { apiClient } from './client';
import type { components } from './schema';

type User = components['schemas']['User'];
type CreateUserRequest = components['schemas']['CreateUserRequest'];

// List users hook
export function useUsers() {
  return useQuery({
    queryKey: ['users'],
    queryFn: async (): Promise<User[]> => {
      const response = await apiClient.GET('/users');
      if (!response.data?.users) {
        throw new Error('Failed to load users');
      }
      return response.data.users;
    },
  });
}

// Create user mutation
export function useCreateUser(options?: {
  onSuccess?: (user: User) => void;
  onError?: (error: Error) => void;
}) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (userData: CreateUserRequest): Promise<User> => {
      const response = await apiClient.POST('/users', {
        body: userData
      });
      if (!response.data) {
        throw new Error('Failed to create user');
      }
      return response.data;
    },
    onSuccess: (user) => {
      queryClient.invalidateQueries({ queryKey: ['users'] });
      options?.onSuccess?.(user);
    },
    onError: options?.onError,
  });
}

// Similar patterns for:
// - useDatabases, useCreateDatabase, useUpdateDatabase, useDeleteDatabase
// - useGrants, useCreateGrant, useRevokeGrant
// - useConnections
// - useQueries, useQuery (single)
// - useAuditEvents
// - useAPIKeys, useCreateAPIKey, useRevokeAPIKey
```

## Pages

### 1. Login Page (`/login`)

- Simple centered login form
- Username/password fields
- "Remember me" checkbox (stores credentials in localStorage)
- Pink gradient background with subtle pattern
- Logo prominently displayed

### 2. Dashboard (`/`)

- **Header stats row**: 4 stat cards
  - Active connections (live count)
  - Queries today (count)
  - Active users (count)
  - Databases configured (count)
- **Recent activity**: Table of last 10 queries
- **Quick actions**: Buttons for common tasks

### 3. Users Page (`/users`)

- **Data table** with columns:
  - Username
  - Roles (badge chips)
  - Rate limit exempt (badge)
  - Created at
  - Actions (edit, delete)
- **Create user dialog** (admin only)
- **Edit user dialog** (change password, roles)

### 4. Databases Page (`/databases`)

- **Data table** with columns:
  - Name
  - Description
  - Host:Port (admin only)
  - Database name (admin only)
  - SSL mode (admin only)
  - Actions
- **Create database dialog** (admin only)
- Connection credentials hidden by default, reveal on click

### 5. Grants Page (`/grants`)

- **Filter bar**: User filter, Database filter, Active only toggle
- **Data table** with columns:
  - User
  - Database
  - Access level (read/write badge)
  - Time window (starts_at - expires_at)
  - Status (active, expired, revoked)
  - Usage (queries, bytes)
  - Actions (revoke)
- **Create grant dialog**

### 6. Connections Page (`/connections`)

- **Filter bar**: User, Database, Active only
- **Data table** with columns:
  - User
  - Database
  - Source IP
  - Connected at
  - Duration (calculated or disconnected_at)
  - Queries count
  - Bytes transferred
- Auto-refresh toggle (poll every 5s)

### 7. Queries Page (`/queries`)

- **Filter bar**: Connection, User, Database, Time range
- **Data table** with columns:
  - SQL text (truncated, expandable)
  - User
  - Database
  - Executed at
  - Duration (ms)
  - Rows affected
  - Status (success/error)
- Click row to navigate to query details

### 8. Query Details Page (`/queries/:uid`)

- **Query info card**:
  - Full SQL text (syntax highlighted if possible)
  - Parameters (if present)
  - Execution metadata
- **Result rows table** (if available)
  - Paginated
  - Column headers from data
  - JSON viewer for complex values

### 9. Audit Log Page (`/audit`)

- **Filter bar**: Event type, User, Performer, Time range
- **Data table** with columns:
  - Event type (color-coded badge)
  - Target user
  - Performed by
  - Details (expandable JSON)
  - Timestamp
- Timeline view option

### 10. API Keys Page (`/api-keys`)

- **Data table** with columns:
  - Name
  - Key prefix (masked)
  - Expires at
  - Last used
  - Request count
  - Status (active, expired, revoked)
  - Actions (revoke)
- **Create key dialog**
  - Name input
  - Optional expiration date
  - **Important**: Show full key ONLY on creation with copy button

## Layout

### Sidebar Navigation

```
[Logo: DBBat bat icon]
[Search bar]

MAIN
├── Dashboard
├── Users
├── Databases
└── Grants

OBSERVABILITY
├── Connections
├── Queries
└── Audit Log

SETTINGS
├── API Keys
└── Profile
```

### Header

- Breadcrumbs
- User menu (profile, logout)
- Theme toggle (light/dark)

## Authentication Flow

1. On app load, check localStorage for saved credentials
2. If found, attempt API call to `/health` or `/users` to validate
3. If valid, set auth context and redirect to dashboard
4. If invalid or no credentials, redirect to login
5. After login success, store credentials (if "remember me") and redirect

## Build & Serve Integration

### Development

```bash
cd front
bun install
bun run dev  # Starts Vite dev server on port 5173
```

### Production Build

```bash
cd front
bun run generate-client  # Generate types from OpenAPI
bun run build            # Build to dist/
```

### Backend Integration

The frontend build output (`front/dist/`) should be:
1. Copied to a `resources/` directory in the backend
2. Embedded using Go's `embed` package
3. Served at `/app` route in the Gin router

```go
// internal/api/server.go
//go:embed resources/*
var frontendFS embed.FS

func (s *Server) setupRoutes() {
    // ... API routes ...

    // Serve frontend at /app
    frontendFiles, _ := fs.Sub(frontendFS, "resources")
    s.router.StaticFS("/app", http.FS(frontendFiles))

    // Redirect root to /app
    s.router.GET("/", func(c *gin.Context) {
        c.Redirect(http.StatusMovedPermanently, "/app")
    })
}
```

### Build Script

Add a Makefile target or script:

```bash
#!/bin/bash
# scripts/build-frontend.sh

cd front
bun install
bun run generate-client
bun run build

# Copy to backend resources
rm -rf ../internal/api/resources
cp -r dist ../internal/api/resources
```

## Configuration Files

### bunfig.toml

```toml
[install]
# Use exact versions
exact = true

[run]
# Silence the bun startup message
silent = false
```

### components.json (shadcn)

```json
{
  "$schema": "https://ui.shadcn.com/schema.json",
  "style": "new-york",
  "rsc": false,
  "tsx": true,
  "tailwind": {
    "config": "",
    "css": "src/index.css",
    "baseColor": "neutral",
    "cssVariables": true,
    "prefix": ""
  },
  "aliases": {
    "components": "@/components",
    "utils": "@/lib/utils",
    "ui": "@/components/ui",
    "lib": "@/lib",
    "hooks": "@/hooks"
  },
  "iconLibrary": "lucide"
}
```

### vite.config.ts

```typescript
import path from "path";
import { fileURLToPath } from "url";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react-swc';
import { TanStackRouterVite } from '@tanstack/router-plugin/vite';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig(({ command }) => ({
  plugins: [
    TanStackRouterVite(),
    react(),
    tailwindcss()
  ],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  base: '/app/',  // Important: base path for serving under /app
  define: {
    ...(command !== 'build' && {
      'import.meta.env.VITE_API_BASE_URL': JSON.stringify('http://localhost:8080/api')
    })
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true
      }
    }
  }
}));
```

### .env.example

```bash
# API base URL (empty for production, full URL for dev)
VITE_API_BASE_URL=http://localhost:8080/api
```

## shadcn Components to Install

```bash
bunx shadcn@latest init

# Core UI components
bunx shadcn@latest add button
bunx shadcn@latest add input
bunx shadcn@latest add label
bunx shadcn@latest add card
bunx shadcn@latest add badge
bunx shadcn@latest add table
bunx shadcn@latest add dialog
bunx shadcn@latest add dropdown-menu
bunx shadcn@latest add select
bunx shadcn@latest add checkbox
bunx shadcn@latest add switch
bunx shadcn@latest add textarea
bunx shadcn@latest add tooltip
bunx shadcn@latest add separator
bunx shadcn@latest add skeleton
bunx shadcn@latest add alert
bunx shadcn@latest add alert-dialog
bunx shadcn@latest add form
bunx shadcn@latest add toast
bunx shadcn@latest add sidebar
bunx shadcn@latest add breadcrumb
bunx shadcn@latest add scroll-area
bunx shadcn@latest add collapsible
bunx shadcn@latest add command
bunx shadcn@latest add popover
bunx shadcn@latest add calendar
bunx shadcn@latest add date-picker
```

## Dependencies

### package.json

```json
{
  "name": "dbbat-frontend",
  "private": true,
  "version": "0.0.0",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "lint": "eslint .",
    "preview": "vite preview",
    "generate-client": "openapi-typescript ../internal/api/openapi.yml -o ./src/api/schema.ts --root-types --root-types-no-schema-prefix"
  },
  "dependencies": {
    "@hookform/resolvers": "^5.x",
    "@radix-ui/react-alert-dialog": "^1.x",
    "@radix-ui/react-checkbox": "^1.x",
    "@radix-ui/react-collapsible": "^1.x",
    "@radix-ui/react-dialog": "^1.x",
    "@radix-ui/react-dropdown-menu": "^2.x",
    "@radix-ui/react-label": "^2.x",
    "@radix-ui/react-popover": "^1.x",
    "@radix-ui/react-scroll-area": "^1.x",
    "@radix-ui/react-select": "^2.x",
    "@radix-ui/react-separator": "^1.x",
    "@radix-ui/react-slot": "^1.x",
    "@radix-ui/react-switch": "^1.x",
    "@radix-ui/react-tooltip": "^1.x",
    "@tailwindcss/vite": "^4.x",
    "@tanstack/react-query": "^5.x",
    "@tanstack/react-router": "^1.x",
    "class-variance-authority": "^0.7.x",
    "clsx": "^2.x",
    "cmdk": "^1.x",
    "date-fns": "^3.x",
    "lucide-react": "^0.x",
    "openapi-fetch": "^0.14.x",
    "react": "^19.x",
    "react-dom": "^19.x",
    "react-hook-form": "^7.x",
    "tailwind-merge": "^3.x",
    "tailwindcss": "^4.x",
    "zod": "^4.x"
  },
  "devDependencies": {
    "@eslint/js": "^9.x",
    "@tanstack/react-router-devtools": "^1.x",
    "@tanstack/router-plugin": "^1.x",
    "@types/node": "^24.x",
    "@types/react": "^19.x",
    "@types/react-dom": "^19.x",
    "@vitejs/plugin-react-swc": "^4.x",
    "eslint": "^9.x",
    "eslint-plugin-react-hooks": "^6.x",
    "eslint-plugin-react-refresh": "^0.4.x",
    "globals": "^16.x",
    "openapi-typescript": "^7.x",
    "tw-animate-css": "^1.x",
    "typescript": "~5.x",
    "typescript-eslint": "^8.x",
    "vite": "^6.x"
  }
}
```

## Implementation Notes

### Keep It Simple

- No unnecessary abstractions
- Minimal custom components beyond shadcn
- Straightforward data fetching patterns
- Clear separation of concerns

### Performance

- Use TanStack Query's caching effectively
- Lazy load routes with TanStack Router
- Optimize re-renders with proper key usage
- Use skeleton loading states

### Error Handling

- Display API errors in toast notifications
- Show inline validation errors in forms
- Graceful degradation for failed requests
- Retry logic handled by TanStack Query

### Accessibility

- Proper ARIA labels
- Keyboard navigation support (via Radix primitives)
- Focus management in dialogs
- Color contrast compliance

## Future Considerations

- Real-time updates via WebSocket for connections/queries
- Export functionality (CSV, JSON) for tables
- Query syntax highlighting
- Mobile-responsive design improvements
- Saved filters/views
