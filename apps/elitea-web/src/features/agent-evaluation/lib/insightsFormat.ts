/** Display formatting for the insights cards. `undefined` is "not recorded", never zero. */
import { t } from '@/shared/i18n';

function notRecorded(): string {
  return t('features.agentEvaluation.insights.notRecorded', 'not recorded');
}

export function formatMs(value: number | undefined): string {
  if (value === undefined) return notRecorded();
  if (value >= 1000) {
    return t('features.agentEvaluation.insights.seconds', '{{value}} s', { value: (value / 1000).toFixed(1) });
  }
  return t('features.agentEvaluation.insights.milliseconds', '{{value}} ms', { value: Math.round(value) });
}

export function formatCount(value: number | undefined): string {
  return value === undefined ? notRecorded() : new Intl.NumberFormat().format(value);
}

/** Estimated money, in the catalogue's own unit (USD). A missing price is not "free". */
export function formatCost(value: number | undefined): string {
  if (value === undefined) return t('features.agentEvaluation.insights.unpriced', 'not priced');
  return new Intl.NumberFormat(undefined, {
    style: 'currency',
    currency: 'USD',
    maximumFractionDigits: value < 1 ? 4 : 2,
  }).format(value);
}

/** A signed difference, or "no comparison" when one side is missing. */
export function formatDelta(value: number | undefined, format: (n: number) => string): string {
  if (value === undefined) return t('features.agentEvaluation.insights.noComparison', 'n/a');
  const sign = value > 0 ? '+' : value < 0 ? '-' : '';
  return `${sign}${format(Math.abs(value))}`;
}

/** `snake_case` limit keys read as words. */
export function limitLabel(key: string): string {
  const words = key.replaceAll('_', ' ').trim();
  return words === '' ? key : words.charAt(0).toUpperCase() + words.slice(1);
}
