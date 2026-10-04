/**
 * Pure date-range helpers for `AnalyticsContainer`'s filter bar (spec §3.3: a
 * slice's `model/` holds pure derived-state logic; this is the one piece of
 * `AnalyticsContainer`'s local state worth testing in isolation from React).
 *
 * ## Calendar-day presets (#6791)
 *
 * The presets used to be rolling windows (`Last 24h` = now-24h .. now). A
 * rolling 24 hours spans two calendar dates, so the daily chart showed two
 * buckets and a per-user "active days" count could read 2 for one day of use
 * (#6763). The presets are now whole calendar days in the browser's local
 * timezone — the same timezone the pickers display:
 *
 * | Preset   | From                         | To            |
 * |----------|------------------------------|---------------|
 * | Today    | today 00:00                  | today 23:59   |
 * | Last 7d  | 6 days before today, 00:00   | today 23:59   |
 * | Last 30d | 29 days before today, 00:00  | today 23:59   |
 * | Last 90d | 89 days before today, 00:00  | today 23:59   |
 *
 * Days are counted with `setDate`, never by subtracting multiples of 24 hours,
 * so a daylight-saving change inside the window does not shift a boundary off
 * midnight.
 *
 * ## The To field names the LAST MINUTE INCLUDED
 *
 * The pickers are minute-granular (`dd/MM/yyyy HH:mm`), and the server's upper
 * bound is EXCLUSIVE (`occurred_at < date_to`). Sending `23:59:00` would drop
 * the whole final minute; sending `23:59:59.999` would still drop its last
 * millisecond. So `toIsoRange` sends the instant the displayed minute ENDS —
 * for a preset, the next local midnight — and `From` the instant its displayed
 * minute starts. The same rule applies to a Custom range, which is never
 * rounded to a day boundary: what is on screen is exactly what is counted.
 */

export interface DateRange {
  readonly from: Date;
  readonly to: Date;
}

const MINUTE_MS = 60_000;

function startOfMinute(value: Date): Date {
  const next = new Date(value);
  next.setSeconds(0, 0);
  return next;
}

/**
 * The calendar-day range a preset button selects: `days` whole local days,
 * ending with (and including) the day `now` falls on. `days` below 1 is
 * treated as 1 — "today" is the shortest preset there is.
 */
export function presetToDateRange(days: number, now: Date): DateRange {
  const span = Math.max(1, Math.trunc(days));
  const from = new Date(now);
  from.setHours(0, 0, 0, 0);
  from.setDate(from.getDate() - (span - 1));
  // Re-pin to midnight AFTER moving the date: where midnight was skipped by a
  // DST change on that day the engine normalises forward, and this keeps the
  // bound at the first instant of the day either way.
  from.setHours(0, 0, 0, 0);

  const to = new Date(now);
  to.setHours(23, 59, 0, 0);
  return { from, to };
}

/**
 * The wire form of a range: ISO 8601 instants, `dateFrom` inclusive and
 * `dateTo` exclusive (see the header: the To field's minute is included).
 */
export function toIsoRange(range: DateRange): { dateFrom: string; dateTo: string } {
  const from = startOfMinute(range.from);
  const toExclusive = new Date(startOfMinute(range.to).getTime() + MINUTE_MS);
  return { dateFrom: from.toISOString(), dateTo: toExclusive.toISOString() };
}
