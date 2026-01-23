# Grant Quota UI

## Problem Statement

The DBBat backend already supports quota controls for grants via `max_query_counts` (maximum number of queries allowed) and `max_bytes_transferred` (maximum amount of data that can be transferred). However, the frontend UI does not expose these fields when creating a grant, making it impossible for administrators to set quotas through the web interface.

Additionally, the grant list only shows query count usage but not the bytes transferred quota usage.

## Current State

### Backend Support (Already Implemented)

The `AccessGrant` model in `internal/store/models.go` already includes:

```go
MaxQueryCounts      *int64 `bun:"max_query_counts" json:"max_query_counts"`
MaxBytesTransferred *int64 `bun:"max_bytes_transferred" json:"max_bytes_transferred"`
```

The API (`internal/api/openapi.yml`) accepts these fields in `CreateGrantRequest`:

```yaml
max_query_counts:
  type: integer
  format: int64
  description: Maximum queries allowed (quota)
max_bytes_transferred:
  type: integer
  format: int64
  description: Maximum bytes transferred (quota)
```

### Current UI Limitations

1. The create grant dialog (`front/src/routes/_authenticated/grants/index.tsx`) does not include input fields for quota settings
2. The grant list shows `{g.query_count ?? 0}{g.max_query_counts && ` / ${g.max_query_counts}`} queries` but does not show bytes transferred usage
3. No way to set quotas without using the API directly

## Proposed Solution

### UI Changes

#### 1. Add Quota Fields to Create Grant Dialog

Add an optional "Quotas" section to the create grant dialog with two input fields:

```tsx
// After the Access Controls section
<div className="space-y-3">
  <Label>Quotas (Optional)</Label>
  <p className="text-sm text-muted-foreground">
    Set limits on usage. Leave empty for unlimited.
  </p>
  <div className="grid grid-cols-2 gap-4">
    <div className="space-y-2">
      <Label htmlFor="maxQueries">Max Queries</Label>
      <Input
        id="maxQueries"
        type="number"
        min="1"
        placeholder="Unlimited"
        value={maxQueries}
        onChange={(e) => setMaxQueries(e.target.value ? parseInt(e.target.value) : undefined)}
      />
    </div>
    <div className="space-y-2">
      <Label htmlFor="maxBytes">Max Data Transfer</Label>
      <div className="flex gap-2">
        <Input
          id="maxBytes"
          type="number"
          min="1"
          placeholder="Unlimited"
          value={maxBytesValue}
          onChange={(e) => setMaxBytesValue(e.target.value ? parseInt(e.target.value) : undefined)}
        />
        <Select value={bytesUnit} onValueChange={setBytesUnit}>
          <SelectTrigger className="w-24">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="MB">MB</SelectItem>
            <SelectItem value="GB">GB</SelectItem>
          </SelectContent>
        </Select>
      </div>
    </div>
  </div>
</div>
```

The max data transfer field should use a user-friendly unit selector (MB/GB) and convert to bytes when submitting to the API.

#### 2. Update Submit Handler

```tsx
const handleSubmit = (e: React.FormEvent) => {
  e.preventDefault();

  // Convert bytes unit to actual bytes
  const maxBytesTransferred = maxBytesValue
    ? maxBytesValue * (bytesUnit === "GB" ? 1024 * 1024 * 1024 : 1024 * 1024)
    : undefined;

  createGrant.mutate({
    user_id: userId,
    database_id: databaseId,
    controls: controls as ("read_only" | "block_copy" | "block_ddl")[],
    starts_at: new Date(startsAt).toISOString(),
    expires_at: new Date(expiresAt).toISOString(),
    max_query_counts: maxQueries || undefined,
    max_bytes_transferred: maxBytesTransferred,
  });
};
```

#### 3. Update Grant List Usage Column

Update the "Usage" column to show both query count and bytes transferred:

```tsx
{
  key: "usage",
  header: "Usage",
  cell: (g) => (
    <div className="text-sm space-y-1">
      <div>
        {g.query_count ?? 0}
        {g.max_query_counts && ` / ${g.max_query_counts}`} queries
      </div>
      <div>
        {formatBytes(g.bytes_transferred ?? 0)}
        {g.max_bytes_transferred && ` / ${formatBytes(g.max_bytes_transferred)}`}
      </div>
    </div>
  ),
},
```

#### 4. Add Bytes Formatting Helper

```tsx
function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + " " + sizes[i];
}
```

## State Management

Add the following state variables to `CreateGrantDialog`:

```tsx
const [maxQueries, setMaxQueries] = useState<number | undefined>(undefined);
const [maxBytesValue, setMaxBytesValue] = useState<number | undefined>(undefined);
const [bytesUnit, setBytesUnit] = useState<"MB" | "GB">("MB");
```

## Validation

- `max_query_counts`: Optional positive integer (>= 1)
- `max_bytes_transferred`: Optional positive integer (>= 1), converted from user-friendly MB/GB input
- Both fields use `null`/`undefined` to indicate unlimited

## Playwright Tests

Add the following tests to `front/e2e/grants.spec.ts`:

```typescript
test.describe("Grant Quota Management", () => {
  test("should show quota fields in create grant dialog", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });
    await createButton.click();

    // Wait for dialog to open
    await authenticatedPage.waitForSelector('[role="dialog"]');

    // Verify quota fields are present
    await expect(authenticatedPage.getByLabel(/max queries/i)).toBeVisible();
    await expect(authenticatedPage.getByLabel(/max data transfer/i)).toBeVisible();

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-create-with-quotas.png",
    });
  });

  test("should accept quota values when creating grant", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });
    await createButton.click();

    // Wait for dialog to open
    await authenticatedPage.waitForSelector('[role="dialog"]');

    // Fill quota fields
    const maxQueriesInput = authenticatedPage.getByLabel(/max queries/i);
    await maxQueriesInput.fill("1000");

    const maxBytesInput = authenticatedPage.getByLabel(/max data transfer/i);
    await maxBytesInput.fill("500");

    // Take screenshot with filled quotas
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-create-quotas-filled.png",
    });

    // Verify input values
    await expect(maxQueriesInput).toHaveValue("1000");
    await expect(maxBytesInput).toHaveValue("500");
  });

  test("should display quota usage in grant list", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot showing grant list with quota usage
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-quota-usage.png",
      fullPage: true,
    });

    // Verify usage column is present
    const content = await authenticatedPage.textContent("body");
    expect(content?.toLowerCase()).toMatch(/queries|usage/);
  });

  test("quota fields should be optional", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });
    await createButton.click();

    // Wait for dialog to open
    await authenticatedPage.waitForSelector('[role="dialog"]');

    // Verify quota fields have placeholder indicating unlimited
    const maxQueriesInput = authenticatedPage.getByLabel(/max queries/i);
    const maxBytesInput = authenticatedPage.getByLabel(/max data transfer/i);

    // Check placeholders indicate unlimited (fields should be empty by default)
    await expect(maxQueriesInput).toHaveAttribute("placeholder", /unlimited/i);
    await expect(maxBytesInput).toHaveAttribute("placeholder", /unlimited/i);
  });
});
```

## Implementation Steps

1. **Update CreateGrantDialog component**: Add state variables for quota fields
2. **Add quota input fields**: Add Max Queries and Max Data Transfer inputs after Access Controls section
3. **Add unit selector**: Implement MB/GB selector for data transfer field
4. **Update submit handler**: Include quota fields in the mutation payload
5. **Add formatBytes helper**: Create utility function for formatting bytes
6. **Update Usage column**: Display both queries and bytes transferred with quotas
7. **Update Playwright tests**: Add tests for quota functionality
8. **Regenerate API client**: Run `bun run generate-client` to ensure TypeScript types are up to date

## Future Enhancements

- Visual progress bars for quota usage
- Warning indicators when approaching quota limits
- Bulk quota editing for multiple grants
- Default quota templates
