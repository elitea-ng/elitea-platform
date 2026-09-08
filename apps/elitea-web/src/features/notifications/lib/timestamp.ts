/**
 * features/notifications/lib/timestamp.ts — port of
 * `apps/elitea-ui/src/common/convertChatConversationMessages.js:21-33`'s
 * `convertTime`, consumed by `NotificationListItem.jsx:105`
 * (`formatDistanceToNow(new Date(convertTime(notification.created_at)))`).
 *
 * Not available from `shared/lib` (chat's C-series units, which own the
 * rest of that source file, have not landed) — duplicated locally as a
 * self-contained ~10-line function rather than reached for across the
 * ownership fence. Flagged for consolidation once a chat unit lands the
 * same helper in `shared/lib`.
 *
 * Normalizes a backend timestamp that may be missing an explicit UTC
 * marker into one `new Date(...)` parses as UTC rather than local time —
 * `"2026-01-01 12:00:00"` (space-separated, naive) becomes
 * `"2026-01-01T12:00:00Z"`; a string already carrying `Z` or a `+offset`
 * is returned unchanged.
 */
import { format } from 'date-fns';

export function normalizeNotificationTimestamp(time: string): string {
  const timeStrings = time.split(' ');
  if (timeStrings.length > 1) {
    return `${timeStrings[0]}T${timeStrings[1]}Z`;
  }
  if (time.at(-1) === 'Z') {
    return time;
  }
  if (time.includes('+')) {
    return time;
  }
  return `${time}Z`;
}

/**
 * The Notifications TABLE's `Date & Time` column:
 * `format(new Date(value), 'dd-MMM-yyyy, kk:mm')`
 * (`NotificationTable.jsx:200-206`). A live deployment renders
 * `12-Jun-2026, 15:55`.
 *
 * `kk`, not `HH`, is deliberate — it is the reference's own pattern, and the
 * two differ: date-fns `kk` is the 1-24 clock, so midnight formats as `24:00`
 * rather than `00:00`. Reproduced rather than "corrected", because the column
 * has to match what production prints.
 *
 * The value goes through `normalizeNotificationTimestamp` first: the list
 * endpoint returns RFC3339, but the single-item endpoints return
 * `2006-01-02T15:04:05` with no zone, which `new Date` reads as LOCAL time
 * and would shift the whole column by the viewer's offset.
 *
 * An unparseable value renders as an empty cell rather than the string
 * `Invalid Date`.
 */
export function formatNotificationTimestamp(value: string | undefined): string {
  if (!value) return '';
  const parsed = new Date(normalizeNotificationTimestamp(value));
  if (Number.isNaN(parsed.getTime())) return '';
  return format(parsed, 'dd-MMM-yyyy, kk:mm');
}
