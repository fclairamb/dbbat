# DBBat Frontend

A React-based Single Page Application (SPA) for managing and monitoring the DBBat PostgreSQL observability proxy.

## Tech Stack

- **Framework**: React 19 with TypeScript
- **Build Tool**: Vite 6
- **Package Manager**: Bun
- **Routing**: TanStack Router (file-based routing)
- **Data Fetching**: TanStack Query (React Query)
- **Styling**: Tailwind CSS v4
- **UI Components**: Radix UI primitives + custom shadcn/ui-style components
- **Forms**: React Hook Form + Zod validation
- **API Client**: openapi-fetch (type-safe, generated from OpenAPI spec)
- **Icons**: Lucide React

## Project Structure

```
front/
├── src/
│   ├── api/
│   │   ├── client.ts          # API client setup with auth middleware
│   │   └── schema.ts          # Generated TypeScript types from OpenAPI
│   ├── components/
│   │   ├── layout/            # Layout components (header, nav, etc.)
│   │   ├── shared/            # Shared business logic components
│   │   └── ui/                # Reusable UI primitives (button, dialog, etc.)
│   ├── contexts/
│   │   └── AuthContext.tsx    # Authentication context and hooks
│   ├── hooks/                 # Custom React hooks
│   ├── routes/                # File-based routes
│   │   ├── __root.tsx         # Root layout
│   │   ├── login.tsx          # Public login page
│   │   ├── _authenticated.tsx # Authenticated layout wrapper
│   │   └── _authenticated/    # Protected routes
│   │       ├── users/         # User management pages
│   │       ├── databases/     # Database configuration pages
│   │       ├── grants/        # Access grant management pages
│   │       ├── connections/   # Connection monitoring pages
│   │       ├── queries/       # Query log viewer pages
│   │       └── audit/         # Audit log viewer pages
│   ├── lib/                   # Utility functions
│   ├── main.tsx               # Application entry point
│   └── index.css              # Global styles and Tailwind imports
├── public/                    # Static assets
├── vite.config.ts             # Vite configuration
├── package.json               # Dependencies and scripts
└── tsconfig.json              # TypeScript configuration
```

## Getting Started

### Prerequisites

- **Bun** v1.0+ (package manager and runtime)
- **Node.js** v18+ (for compatibility)
- **DBBat backend** running on `http://localhost:8080` (for API)

### Installation

```bash
# Install dependencies
bun install
```

### Development

```bash
# Start development server (with hot reload)
bun run dev
```

The development server will start on `http://localhost:5173/app/` (default).

**Note**: In development mode, the app automatically proxies `/api` requests to the backend at `http://localhost:8080`.

### Building for Production

```bash
# Build with TypeScript type checking
bun run build

# Build without type checking (faster)
bun run build:no-check
```

The production build will be output to the `dist/` directory.

### Preview Production Build

```bash
# Preview the production build locally
bun run preview
```

### Linting

```bash
# Run ESLint
bun run lint
```

### End-to-End Testing

The project uses **Playwright** for end-to-end testing against a **production build** of the application.

#### How It Works

The E2E test suite:
1. **Builds** the complete application (frontend + backend with embedded resources)
2. **Starts** PostgreSQL via docker-compose
3. **Runs** the dbbat server with `DBB_RUN_MODE=test` (creates admin/admintest credentials and sample data)
4. **Executes** Playwright tests against `http://localhost:8080/app/`
5. **Tears down** the server and PostgreSQL after tests complete

This is all handled automatically by Playwright's global setup and teardown scripts.

#### Prerequisites

- **Docker** must be running (for PostgreSQL)
- **Bun** installed (package manager)
- **Playwright browsers** installed (done automatically on first run with `bunx playwright install`)

#### Running Tests

```bash
# From project root
make test-e2e

# Or from front/ directory
bun run test:e2e              # Run all tests (headless, all browsers)
bun run test:e2e:chromium     # Run tests on Chromium only
bun run test:e2e:firefox      # Run tests on Firefox only
bun run test:e2e:webkit       # Run tests on WebKit only
bun run test:e2e:ui           # Run with UI mode (interactive)
bun run test:e2e:headed       # Run in headed mode (see browser)
bun run test:e2e:debug        # Debug tests (step through with inspector)
bun run test:report           # View HTML test report
```

#### Test Structure

Tests are located in the `e2e/` directory:

```
e2e/
├── global-setup.ts      # Build & start server (runs once before all tests)
├── global-teardown.ts   # Stop server & cleanup (runs once after all tests)
├── fixtures.ts          # Shared test fixtures (authenticated page)
├── login.spec.ts        # Login flow tests
├── navigation.spec.ts   # Navigation tests
├── users.spec.ts        # User management tests
├── databases.spec.ts    # Database configuration tests
├── grants.spec.ts       # Access grants tests
└── observability.spec.ts # Connections/queries/audit tests
```

#### Screenshot Capture

Tests automatically capture screenshots at key points:

- **Login flow**: Login page, error states, successful login
- **Navigation**: Each main section (users, databases, grants, etc.)
- **CRUD operations**: Create dialogs, list views, detail pages
- **Error states**: Failures and validation errors

Screenshots are saved to `test-results/screenshots/`.

#### Test Features

- **Production build testing**: Tests run against the actual production build (not dev server)
- **Automated setup/teardown**: Server lifecycle managed automatically by Playwright
- **Test mode**: Server runs with `DBB_RUN_MODE=test` for predictable state
  - Admin credentials: `admin` / `admintest`
  - Sample users: `viewer` (viewer role), `connector` (connector role)
  - Sample database and grants pre-configured
- **Authenticated fixture**: Reusable fixture that logs in before tests
- **Full page screenshots**: Capture entire page for layout verification
- **Network idle waiting**: Ensures pages are fully loaded
- **Multiple browsers**: Tests run on Chromium, Firefox, and WebKit
- **Automatic retry**: Tests retry on failure in CI environments
- **Video recording**: Videos saved on test failure

#### Example Test

```typescript
import { test, expect } from "./fixtures";

test("should display users list", async ({ authenticatedPage }) => {
  await authenticatedPage.goto("/users");
  await authenticatedPage.waitForLoadState("networkidle");

  // Take screenshot
  await authenticatedPage.screenshot({
    path: "test-results/screenshots/users-list.png",
    fullPage: true,
  });

  // Verify we're on the users page
  await expect(authenticatedPage).toHaveURL(/\/users/);
});
```

#### Manual Testing (Without Playwright)

If you want to manually test against the production build:

```bash
# Terminal 1: Start PostgreSQL
docker compose up -d postgres

# Terminal 2: Build and run server in test mode
make build-all
DBB_RUN_MODE=test \
  DBB_DSN="postgres://postgres:postgres@localhost:5001/dbbat?sslmode=disable" \
  DBB_KEY="MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" \
  ./bin/dbbat serve

# Access at http://localhost:8080/app/
# Login with: admin / admintest
```

## Configuration

### Base URL

The frontend can be served at a custom base path using the `VITE_BASE_URL` environment variable:

```bash
# Default: /app/
VITE_BASE_URL=/app/ bun run dev

# Serve at root
VITE_BASE_URL=/ bun run dev

# Custom path
VITE_BASE_URL=/custom-path/ bun run dev
```

**Important**: When building for production with a custom base URL, set the environment variable during build:

```bash
VITE_BASE_URL=/custom-path/ bun run build
```

The backend must be configured with the matching `DBB_BASE_URL` environment variable.

### API Base URL

The API base URL is automatically configured:

- **Development**: `http://localhost:8080/api` (can be overridden with `VITE_API_BASE_URL`)
- **Production**: `/api` (relative to the deployment URL)

To use a different backend during development:

```bash
VITE_API_BASE_URL=http://localhost:9000/api bun run dev
```

## Architecture

### Routing

The app uses **TanStack Router** with file-based routing. Routes are defined in `src/routes/`:

- **Public routes**: `login.tsx`
- **Authenticated routes**: All routes under `_authenticated/` (protected by auth guard)

Route files are automatically discovered and compiled into a route tree (`routeTree.gen.ts`).

### Authentication

Authentication is managed through:

1. **AuthContext** (`src/contexts/AuthContext.tsx`)
   - Stores current user state
   - Provides login/logout functions
   - Persists auth credentials (Basic Auth)

2. **API Client** (`src/api/client.ts`)
   - Automatically adds `Authorization` header to all requests
   - Supports both Basic Auth and Bearer tokens
   - Middleware can be updated/removed dynamically

3. **Authenticated Layout** (`src/routes/_authenticated.tsx`)
   - Wraps all protected routes
   - Redirects to `/login` if not authenticated
   - Provides navigation and layout for authenticated pages

### Data Fetching

The app uses **TanStack Query** for data fetching with:

- Automatic caching (1-minute stale time)
- Request deduplication
- Background refetching
- Optimistic updates

Example pattern:

```typescript
import { useQuery } from "@tanstack/react-query";
import { apiClient } from "@/api/client";

function useUsers() {
  return useQuery({
    queryKey: ["users"],
    queryFn: async () => {
      const { data } = await apiClient.GET("/api/users");
      return data?.users || [];
    },
  });
}
```

### Type Safety

The app uses **openapi-fetch** with generated TypeScript types from the OpenAPI specification:

1. Backend OpenAPI spec: `../internal/api/openapi.yml`
2. Generated types: `src/api/schema.ts`
3. Type-safe API client: `apiClient` in `src/api/client.ts`

To regenerate types after OpenAPI spec changes:

```bash
bun run generate-client
```

### UI Components

UI components follow the **shadcn/ui** pattern:

- **Radix UI primitives**: Accessible, unstyled components
- **Tailwind CSS**: Utility-first styling
- **CVA**: Class variance authority for component variants
- **Custom components**: Located in `src/components/ui/`

Components are designed to be:
- Fully accessible (ARIA compliant)
- Keyboard navigable
- Themeable (via Tailwind)
- Composable

## Features

### User Management
- Create, update, and delete users
- Toggle admin privileges
- Reset passwords

### Database Configuration
- Add target PostgreSQL databases
- Configure connection details (host, port, database name)
- Store encrypted credentials
- Test connections

### Access Grants
- Grant time-windowed access to databases
- Set access levels (read/write)
- Configure quotas (max queries, max bytes)
- Revoke access manually

### Observability
- **Connections**: View all proxy connections with filters
- **Queries**: Browse query logs with execution times and results
- **Audit Log**: Track all access control changes

## Development Guidelines

### Adding a New Route

1. Create a route file in `src/routes/` or `src/routes/_authenticated/`
2. Export a route configuration using TanStack Router conventions
3. The route will be automatically included in the route tree

Example:

```typescript
// src/routes/_authenticated/example/index.tsx
import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/_authenticated/example/")({
  component: ExamplePage,
});

function ExamplePage() {
  return <div>Example Page</div>;
}
```

### Adding a New UI Component

1. Add the component to `src/components/ui/`
2. Use Radix UI primitives when available
3. Style with Tailwind CSS utility classes
4. Export from the component file

### Calling the API

1. Use the `apiClient` from `src/api/client.ts`
2. All types are automatically inferred from the OpenAPI schema
3. Wrap API calls in TanStack Query hooks for caching

Example:

```typescript
import { apiClient } from "@/api/client";
import { useQuery } from "@tanstack/react-query";

function useConnections() {
  return useQuery({
    queryKey: ["connections"],
    queryFn: async () => {
      const { data, error } = await apiClient.GET("/api/connections");
      if (error) throw error;
      return data?.connections || [];
    },
  });
}
```

## Integration with Backend

The frontend is designed to be embedded in the Go backend:

1. **Build**: Run `bun run build` to create production bundle in `dist/`
2. **Embed**: Backend uses `go:embed` to include `dist/` files
3. **Serve**: Backend serves SPA at configured base URL (default: `/app/`)
4. **Fallback**: Backend handles SPA routing by serving `index.html` for all sub-routes

See `../scripts/build-frontend.sh` for the automated build process.

## Troubleshooting

### Port Already in Use

If port 5173 is already in use, Vite will automatically try the next available port (5174, 5175, etc.).

### API Requests Fail with CORS

In development, ensure the backend is running on `http://localhost:8080` and that the Vite proxy is configured correctly in `vite.config.ts`.

### Types Out of Sync

If TypeScript errors occur after backend API changes:

```bash
bun run generate-client
```

This regenerates `src/api/schema.ts` from the latest OpenAPI specification.

### Hot Reload Not Working

Ensure you're accessing the app at `http://localhost:5173/app/` (with the base path) during development.

## Scripts Reference

| Script | Description |
|--------|-------------|
| `dev` | Start development server with hot reload |
| `build` | Build for production (with type checking) |
| `build:no-check` | Build for production (skip type checking) |
| `lint` | Run ESLint on all source files |
| `preview` | Preview production build locally |
| `generate-client` | Generate TypeScript types from OpenAPI spec |
| `test:e2e` | Run E2E tests with Playwright (headless) |
| `test:e2e:ui` | Run E2E tests with interactive UI |
| `test:e2e:headed` | Run E2E tests in headed mode (visible browser) |
| `test:e2e:debug` | Debug E2E tests with Playwright inspector |
| `test:report` | View HTML test report |
