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

/** Short, list-friendly age: "now", "5m", "3h", "2d", then a date. */
export function formatAge(value: unknown, now: number = Date.now()): string {
  const ms = parseServerTime(value);
  if (ms === null) return '';
  const seconds = Math.max(0, Math.round((now - ms) / 1000));
  if (seconds < 60) return 'now';
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h`;
  if (seconds < 7 * 86_400) return `${Math.floor(seconds / 86_400)}d`;
  return new Date(ms).toISOString().slice(0, 10);
}
