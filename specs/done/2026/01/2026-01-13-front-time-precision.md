# Frontend Grant Time Precision

## Overview

Enhance the grant creation form to support time selection alongside date selection for `starts_at` and `expires_at` fields. Times should be displayed in the user's local timezone but transmitted to the API in UTC.

## Current Behavior

The grant creation dialog currently uses HTML5 native date inputs (`type="date"`) which:

- Only capture the date portion (YYYY-MM-DD)
- Default `starts_at` to today's date (midnight)
- Default `expires_at` to 30 days from now (midnight)
- Convert to ISO 8601 using `new Date(dateString).toISOString()`, resulting in midnight UTC
- Display dates without time information in the grants table

**Location**: `front/src/routes/_authenticated/grants/index.tsx` (lines 261-264, 366-382)

## Proposed Changes

### 1. Add Time Selection to Date Inputs

Replace the current date-only inputs with combined datetime inputs:

```tsx
// Before
<Input type="date" value={startsAt} ... />

// After
<Input type="datetime-local" value={startsAt} ... />
```

### 2. Default Start Time to Current Time

When opening the create grant dialog, `starts_at` should default to the current date and time (rounded to the nearest minute):

```tsx
const [startsAt, setStartsAt] = useState(() => {
  const now = new Date();
  // Round to nearest minute and format for datetime-local input
  now.setSeconds(0, 0);
  return formatDateTimeLocal(now);
});
```

### 3. UTC Storage, Local Display

**Input handling**: The `datetime-local` input works in local time. The value must be converted to UTC when sending to the API:

```tsx
// Convert local datetime-local value to UTC ISO string for API
const localDate = new Date(startsAt);
const utcString = localDate.toISOString(); // Already converts to UTC
```

**Display handling**: When displaying grants in the table, convert UTC timestamps to local time:

```tsx
// Before: date only
format(new Date(grant.starts_at), "MMM d, yyyy")

// After: date and time in local timezone
format(new Date(grant.starts_at), "MMM d, yyyy 'at' HH:mm")
```

### 4. Helper Functions

Add utility functions in `front/src/lib/date-utils.ts`:

```tsx
import { format, parseISO } from "date-fns";

/**
 * Format a Date object for use with datetime-local input.
 * datetime-local expects format: YYYY-MM-DDTHH:mm
 */
export function formatDateTimeLocal(date: Date): string {
  return format(date, "yyyy-MM-dd'T'HH:mm");
}

/**
 * Format an ISO 8601 UTC timestamp for display in local timezone.
 */
export function formatDateTime(isoString: string): string {
  return format(parseISO(isoString), "MMM d, yyyy 'at' HH:mm");
}

/**
 * Format an ISO 8601 UTC timestamp for display (date only).
 */
export function formatDate(isoString: string): string {
  return format(parseISO(isoString), "MMM d, yyyy");
}
```

## Implementation Details

### CreateGrantDialog Changes

Update the state initialization and input components:

```tsx
// State initialization with current time
const [startsAt, setStartsAt] = useState(() => {
  const now = new Date();
  now.setSeconds(0, 0);
  return formatDateTimeLocal(now);
});

const [expiresAt, setExpiresAt] = useState(() => {
  const future = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
  future.setSeconds(0, 0);
  return formatDateTimeLocal(future);
});

// Form submission - values are already in local time, toISOString() converts to UTC
const handleSubmit = () => {
  createGrant({
    // ...other fields
    starts_at: new Date(startsAt).toISOString(),
    expires_at: new Date(expiresAt).toISOString(),
  });
};

// Input components
<div className="space-y-2">
  <Label htmlFor="startsAt">Start Date & Time</Label>
  <Input
    id="startsAt"
    type="datetime-local"
    value={startsAt}
    onChange={(e) => setStartsAt(e.target.value)}
    required
  />
  <p className="text-xs text-muted-foreground">
    Displayed in your local timezone
  </p>
</div>

<div className="space-y-2">
  <Label htmlFor="expiresAt">Expiration Date & Time</Label>
  <Input
    id="expiresAt"
    type="datetime-local"
    value={expiresAt}
    min={startsAt}
    onChange={(e) => setExpiresAt(e.target.value)}
    required
  />
  <p className="text-xs text-muted-foreground">
    Displayed in your local timezone
  </p>
</div>
```

### Grants Table Display Changes

Update the date range display to include time:

```tsx
// Before
<span className="text-muted-foreground">
  {format(new Date(grant.starts_at), "MMM d, yyyy")} to{" "}
  {format(new Date(grant.expires_at), "MMM d, yyyy")}
</span>

// After
<span className="text-muted-foreground">
  {formatDateTime(grant.starts_at)} to {formatDateTime(grant.expires_at)}
</span>
```

### Reset State on Dialog Close

Ensure state resets properly when dialog closes:

```tsx
useEffect(() => {
  if (!open) {
    // Reset to current time when dialog is reopened
    const now = new Date();
    now.setSeconds(0, 0);
    setStartsAt(formatDateTimeLocal(now));

    const future = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
    future.setSeconds(0, 0);
    setExpiresAt(formatDateTimeLocal(future));
  }
}, [open]);
```

## User Experience

### Before
- User selects a date from a date picker
- Time defaults to midnight (00:00) in UTC
- No visibility into what time the grant actually starts

### After
- User sees current date and time pre-filled
- User can adjust both date and time as needed
- Clear indication that times are in local timezone
- Grant list shows both date and time for better visibility

## Edge Cases

1. **Timezone changes**: If a user creates a grant in one timezone and views it in another, the displayed time will correctly reflect the local time in the new timezone.

2. **DST transitions**: The `datetime-local` input and JavaScript Date handling automatically account for daylight saving time transitions.

3. **Validation**: The `min` attribute on `expiresAt` ensures it cannot be set before `startsAt`.

## Files to Modify

1. `front/src/routes/_authenticated/grants/index.tsx` - Update CreateGrantDialog and grants table
2. `front/src/lib/date-utils.ts` - Create new file with date formatting utilities
3. `front/e2e/grants.spec.ts` - Add tests for datetime input functionality

## Testing

### Unit Tests
- Verify `formatDateTimeLocal` produces correct format
- Verify `formatDateTime` correctly formats UTC to local display

### E2E Tests
- Verify datetime-local inputs are present in create dialog
- Verify default start time is approximately current time
- Verify created grants display with time information
- Verify grants created with specific times function correctly (become active at specified time)

## Migration Notes

No backend changes required. The API already accepts and returns full ISO 8601 datetime strings. This is a frontend-only enhancement.
