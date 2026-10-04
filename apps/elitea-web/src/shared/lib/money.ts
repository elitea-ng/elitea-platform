/**
 * USD display formatting shared by every screen that shows spend: the
 * Analytics KPI tiles and cost tables, Settings › Usage, and Admin › Budgets
 * (#6682).
 *
 * Two fraction digits are not enough for an LLM bill. One embedding call costs
 * `$0.00000008`; printed as `$0.00` it reads as "nothing was spent", which is
 * a different claim from "something too small to show was spent". So:
 *
 *  - zero is `$0.00` — a real zero;
 *  - one cent and above keeps two digits (`$12.50`, `$1,234.57`);
 *  - below one cent keeps up to six digits (`$0.004`, `$0.000189`);
 *  - below the smallest six-digit figure prints `< $0.000001` rather than
 *    rounding to zero.
 *
 * This is a DISPLAY conversion. The exact decimal stays on the wire.
 *
 * ## Deliberate exemption: Admin › LLM Proxy usage and models panels
 *
 * `pages/admin/LlmProxyUsagePanel.tsx` and `LlmProxyModelsPanel.tsx` keep their
 * own fixed-precision `costLabel` (`$0.0042`, four digits, `<$0.0001` below
 * that). They already never print a non-zero spend as zero — the #6682 defect
 * this module exists for — and they are operator tables where a fixed number of
 * digits keeps a column of costs aligned for comparison, which the variable
 * 2-or-6 digits here would break. Their per-1M-token prices (`priceLabel`) keep
 * eight digits for the same reason. Moving them here is a column-layout
 * decision for that screen, not a correctness fix.
 */

/** The smallest non-zero amount six fraction digits can show. */
const SMALLEST_SHOWN_USD = 0.000001;

const CENT = 0.01;

function usd(value: number, maximumFractionDigits: number): string {
  return new Intl.NumberFormat(undefined, {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits,
  }).format(value);
}

/**
 * Formats a USD amount so that a non-zero figure never prints as zero.
 * A non-finite input (NaN, ±Infinity) has no meaningful amount and prints as
 * the zero it would otherwise have been coerced to by `toFixed` callers.
 */
export function formatSmallUsd(value: number): string {
  if (!Number.isFinite(value) || value === 0) return usd(0, 2);
  const magnitude = Math.abs(value);
  if (magnitude < SMALLEST_SHOWN_USD) {
    return value > 0 ? `< ${usd(SMALLEST_SHOWN_USD, 6)}` : `> ${usd(-SMALLEST_SHOWN_USD, 6)}`;
  }
  return usd(value, magnitude < CENT ? 6 : 2);
}
