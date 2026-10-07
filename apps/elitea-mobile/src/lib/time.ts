/**
 * Server timestamps arrive in two shapes: RFC 3339 with a zone, and a
 * zone-less `YYYY-MM-DD HH:MM:SS[.ffffff]` (message groups). Zone-less values
 * are read as UTC, which is what the server stores.
 */
export function parseServerTime(value: unknown): number | null {
  if (typeof value !== 'string' || value === '') return null;
  let text = value.trim();
  const zoneless = /^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(:\d{2}(\.\d+)?)?$/.test(text);
  if (zoneless) text = `${text.replace(' ', 'T')}Z`;
  // Date.parse handles at most millisecond precision consistently.
  text = text.replace(/(\.\d{3})\d+/, '$1');
  const ms = Date.parse(text);
  return Number.isNaN(ms) ? null : ms;
}

/** How a date is written: the device's locale and time zone unless a test fixes them. */
export interface DateStyle {
  locale?: string;
  timeZone?: string;
}

/**
 * Short, list-friendly age: "now", "5m", "3h", "2d", then the local calendar date in the user's
 * locale ("25 Sep", "Sep 25"; the year only when it is not this year's). The date is the day the
 * user lived it, in their time zone, never the UTC day (S4).
 */
export function formatAge(value: unknown, now: number = Date.now(), style: DateStyle = {}): string {
  const ms = parseServerTime(value);
  if (ms === null) return '';
  const seconds = Math.max(0, Math.round((now - ms) / 1000));
  if (seconds < 60) return 'now';
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h`;
  if (seconds < 7 * 86_400) return `${Math.floor(seconds / 86_400)}d`;
  return formatShortDate(ms, now, style);
}

/** "Sep 25" / "25 Sep", with the year when it differs from `now`'s (both in the same zone). */
export function formatShortDate(ms: number, now: number = Date.now(), { locale, timeZone }: DateStyle = {}): string {
  const year = (t: number) => new Intl.DateTimeFormat(locale, { year: 'numeric', timeZone }).format(t);
  const sameYear = year(ms) === year(now);
  return new Intl.DateTimeFormat(locale, {
    month: 'short',
    day: 'numeric',
    ...(sameYear ? {} : { year: 'numeric' }),
    timeZone,
  }).format(ms);
}
