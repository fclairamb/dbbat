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
