/**
 * A run's start time as the reader's own local date and time (#1981).
 *
 * The list route sends `created_at` as a UTC wall clock WITHOUT a zone
 * (`2026-11-12T16:30:11.816831`, the same convention `archived_at` documents).
 * `new Date()` would read such a string as LOCAL time and show every run hours
 * off, so the zone is made explicit before parsing. A value that already
 * carries one (`Z` or an offset) is parsed as it stands, and one that is not a
 * date at all is returned unchanged rather than shown as "Invalid Date".
 */
const HAS_ZONE = /(?:Z|[+-]\d{2}:?\d{2})$/i;

export function formatRunTimestamp(value: string | undefined): string {
  if (value === undefined || value === '') return '—';
  const normalised = value.includes('T') || value.includes(' ') ? value.replace(' ', 'T') : value;
  const parsed = new Date(HAS_ZONE.test(normalised) ? normalised : `${normalised}Z`);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}
