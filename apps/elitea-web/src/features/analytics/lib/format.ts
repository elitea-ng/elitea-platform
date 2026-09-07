/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/analytics/lib/helpers/analyticsCommon.helpers.js`
 * (`fmtNum`, `fmtDuration`) — pure, table-driven-tested formatting helpers
 * shared by every analytics screen.
 */

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
 * Two fraction digits are not enough here. A cost estimate over a short window
 * is routinely below one cent — the reference deployment shows `$0.000189` for
 * three calls — and two digits render every such figure as `$0.00`, which reads
 * as "this project spent nothing". So an amount below one cent keeps six
 * digits, which is the precision the reference screen shows.
 *
 * This is a DISPLAY conversion and the only place a cost becomes a float. The
 * exact decimal stays on the wire: the server computes every figure in
 * PostgreSQL NUMERIC and publishes it unrounded.
 */
export function fmtUsd(value: number | null | undefined): string {
  if (value == null) return '$0.00';
  const digits = value !== 0 && Math.abs(value) < 0.01 ? 6 : 2;
  return new Intl.NumberFormat(undefined, {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: digits,
  }).format(value);
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
