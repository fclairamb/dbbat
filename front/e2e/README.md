# End-to-End Tests

This directory contains Playwright-based end-to-end tests for the DBBat frontend.

## Quick Start

```bash
# Install dependencies (if not already done)
bun install

# Run tests (headless)
bun run test:e2e

# Run tests with UI (interactive)
bun run test:e2e:ui

# Run tests in headed mode (see browser)
bun run test:e2e:headed

# Debug tests
bun run test:e2e:debug

# View test report
bun run test:report
```

## Prerequisites

Before running tests, ensure:

1. **Backend is running** in test mode:
   ```bash
   RUN_MODE=test ./dbbat serve
   ```

   The backend should be accessible at `http://localhost:8080` with default admin credentials (`admin`/`admintest`).

2. **Frontend dev server is running**:
   ```bash
   bun run dev
   ```

   The dev server should be accessible at `http://localhost:5173/app/`.

3. **Playwright browsers are installed**:
   ```bash
   bunx playwright install
   ```

4. **Check prerequisites** (optional helper script):
   ```bash
   ./e2e/check-backend.sh
   ```

## Test Files

| File | Description |
|------|-------------|
| `fixtures.ts` | Shared test fixtures (authenticated page context) |
| `login.spec.ts` | Login flow and authentication tests |
| `navigation.spec.ts` | Navigation between main sections |
| `users.spec.ts` | User management tests |
| `databases.spec.ts` | Database configuration tests |
| `grants.spec.ts` | Access grant management tests |
| `observability.spec.ts` | Connections, queries, and audit log tests |

## Test Structure

### Using Fixtures

The `fixtures.ts` file provides an `authenticatedPage` fixture that automatically logs in before each test:

```typescript
import { test, expect } from "./fixtures";

test("should display users", async ({ authenticatedPage }) => {
  await authenticatedPage.goto("/users");
  // Test is already authenticated
});
```

### Taking Screenshots

Screenshots are automatically captured at key points. To add more:

```typescript
await page.screenshot({
  path: "test-results/screenshots/my-test.png",
  fullPage: true, // Capture entire page
});
```

### Waiting for Page Load

Always wait for pages to fully load before assertions:

```typescript
await page.waitForLoadState("networkidle");
```

## Writing New Tests

1. **Create a new spec file** in the `e2e/` directory:
   ```bash
   touch e2e/my-feature.spec.ts
   ```

2. **Use the test fixtures**:
   ```typescript
   import { test, expect } from "./fixtures";

   test.describe("My Feature", () => {
     test("should do something", async ({ authenticatedPage }) => {
       await authenticatedPage.goto("/my-feature");
       // Add assertions
     });
   });
   ```

3. **Add screenshots** at important points:
   ```typescript
   await authenticatedPage.screenshot({
     path: "test-results/screenshots/my-feature.png",
   });
   ```

## Test Configuration

Configuration is in `playwright.config.ts`:

- **Base URL**: `http://localhost:5173/app`
- **Browsers**: Chromium, Firefox, WebKit
- **Retries**: 2 on CI, 0 locally
- **Video**: Recorded on failure
- **Screenshots**: Captured on failure

## Best Practices

1. **Use fixtures** for common setup (like authentication)
2. **Wait for network idle** before taking screenshots or assertions
3. **Use full-page screenshots** for layout verification
4. **Name screenshots descriptively** for easy identification
5. **Test user flows** rather than individual components
6. **Clean up test data** if tests create persistent data

## Debugging Tests

### Visual Debugging

```bash
# Run with UI mode (interactive)
bun run test:e2e:ui

# Run in headed mode (see browser)
bun run test:e2e:headed
```

### Inspector

```bash
# Step through tests with inspector
bun run test:e2e:debug
```

### Viewing Reports

After tests run, view the HTML report:

```bash
bun run test:report
```

## CI/CD Integration

Tests are configured to run automatically in CI:

- **Retries**: Tests retry 2 times on failure
- **Parallel**: Tests run sequentially in CI (workers=1)
- **Artifacts**: Screenshots, videos, and reports are saved

## Troubleshooting

### Backend Not Running

**Error**: Connection refused or timeout

**Solution**: Start the backend in test mode:
```bash
RUN_MODE=test ./dbbat serve
```

### Port Already in Use

**Error**: Port 5173 already in use

**Solution**: Stop the conflicting process or configure a different port in `playwright.config.ts`

### Authentication Fails

**Error**: Tests timeout on login or "Failed to fetch" errors

**Solution**:
- Ensure default admin user exists (`admin`/`admintest`) in test mode
- Verify both dev server and backend are running
- Check that the vite proxy is working correctly (no CORS errors)

### Flaky Tests

**Solution**:
- Add `waitForLoadState("networkidle")` before assertions
- Increase timeout for slow operations
- Use `waitForSelector()` for dynamic content

## Screenshot Organization

Screenshots are saved to `test-results/screenshots/`:

```
test-results/screenshots/
├── login-page.png
├── login-error.png
├── login-success.png
├── nav-users.png
├── nav-databases.png
├── users-list.png
├── databases-create-dialog.png
└── ...
```

## Coverage

Tests cover:

- ✅ Authentication (login, logout, auth guards)
- ✅ Navigation (all main sections)
- ✅ User management (list, create, actions)
- ✅ Database configuration (list, create, settings)
- ✅ Access grants (list, create, revoke)
- ✅ Observability (connections, queries, audit logs)

## Future Enhancements

- [ ] Test CRUD operations end-to-end
- [ ] Test form validation
- [ ] Test error states and edge cases
- [ ] Test responsive design (mobile viewports)
- [ ] Add API mocking for isolated testing
- [ ] Add visual regression testing
