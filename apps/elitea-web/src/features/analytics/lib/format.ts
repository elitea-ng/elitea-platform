/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/analytics/lib/helpers/analyticsCommon.helpers.js`
 * (`fmtNum`, `fmtDuration`) — pure, table-driven-tested formatting helpers
 * shared by every analytics screen.
 */

import { formatSmallUsd } from '@/shared/lib/money';

/**
 * Formats a count as `1.2M` / `3.4K` / a plain integer string.
 * `null`/`undefined` render `'0'` (byte-for-byte baseline behaviour).
 */
export function fmtNum(n: number | null | undefined): string {
  if (n == null) return '0';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return String(n);
}

/**
 * Formats a millisecond duration as `123ms` / `4.5s`.
 * `null`/`undefined` render `'-'` (byte-for-byte baseline behaviour).
 */
export function fmtDuration(ms: number | null | undefined): string {
  if (ms == null) return '-';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

/**
 * Placeholder for a metric column the live Go backend genuinely does not
 * emit today (see this unit's final report — the analytics list/detail
 * response shapes carry far fewer fields than the baseline SPA's UI reads).
 * Deliberately distinct from `fmtNum(0)`, which asserts a real zero count:
 * this renders an honest "unknown", not a fabricated zero, while keeping
 * the baseline's column header in place for later backend enrichment.
 */
export const UNAVAILABLE_METRIC = '–';

/**
 * Formats an ISO-8601 instant as a short local date-time, or an em dash when
 * there is nothing to format.
 *
 * The dash here is NOT `UNAVAILABLE_METRIC`'s meaning. That constant marks a
 * figure this platform has no producer for; this marks a value that is simply
 * missing from one row — a member the request log has a count for but no
 * timestamp, which a malformed row could produce. Rendering `Invalid Date` is
 * the thing being avoided.
 */
/**
 * Formats a USD amount for a tile or a table cell.
 *
 * Delegates to `formatSmallUsd` (`shared/lib/money.ts`): below one cent keeps
 * six digits (the reference deployment shows `$0.000189` for three calls), and
 * a non-zero amount below that prints `< $0.000001` instead of `$0.00`
 * (#6682) — two digits read as "this project spent nothing".
 *
 * This is a DISPLAY conversion and the only place a cost becomes a float. The
 * exact decimal stays on the wire: the server computes every figure in
 * PostgreSQL NUMERIC and publishes it unrounded.
 */
export function fmtUsd(value: number | null | undefined): string {
  return formatSmallUsd(value ?? 0);
}

/**
 * Formats one row's contribution as a percentage of a total.
 *
 * A total of zero yields `UNAVAILABLE_METRIC` rather than `0.0%`: a share of
 * nothing is not zero percent, and printing one invites the reader to compare
 * rows that have no denominator between them.
 */
export function fmtShare(value: number, total: number): string {
  if (total <= 0) return UNAVAILABLE_METRIC;
  return `${((value / total) * 100).toFixed(1)}%`;
}

export function fmtTimestamp(value: unknown): string {
  if (typeof value !== 'string' || value === '') return '—';
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return '—';
  return parsed.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

/** The part of an `/analytics_costs` period row the billed-period label reads. */
export interface BilledPeriod {
  readonly scope: string;
  readonly period_start: string;
  readonly period_end: string;
}

/**
 * The calendar dates the COST tile's figure actually covers, or `undefined`
 * when no project-scope row says.
 *
 * `/analytics_costs` sums every PROJECT-scope accumulator row that OVERLAPS the
 * window, and each row is a whole billing period (a month). So under `Today`
 * the figure is the month's spend to date, not today's — and the tile has to
 * say which dates it is about rather than borrow the window's.
 *
 * Periods are UTC calendar months (`period_end` exclusive), so the dates are
 * formatted in UTC: in a negative-offset browser local time would print the
 * 1st as the last day of the previous month.
 */
export function fmtBilledPeriod(periods: readonly BilledPeriod[]): string | undefined {
  let start: number | undefined;
  let end: number | undefined;
  for (const period of periods) {
    if (period.scope !== 'project') continue;
    const from = Date.parse(period.period_start);
    const to = Date.parse(period.period_end);
    if (Number.isNaN(from) || Number.isNaN(to) || to <= from) continue;
    start = start === undefined ? from : Math.min(start, from);
    end = end === undefined ? to : Math.max(end, to);
  }
  if (start === undefined || end === undefined) return undefined;
  const format = new Intl.DateTimeFormat(undefined, { timeZone: 'UTC', day: 'numeric', month: 'short', year: 'numeric' });
  // `period_end` is exclusive: the last day covered ends one instant before it.
  return format.formatRange(new Date(start), new Date(end - 1));
}
