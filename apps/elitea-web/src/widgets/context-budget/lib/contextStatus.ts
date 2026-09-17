import { resolveContextBudgetMode, type ContextBudgetMode } from '@/shared/lib/contextBudget';

/** The digit-group separator: U+00A0, so a grouped count never line-wraps mid-number. */
const GROUP_SEPARATOR = '\u00a0';

/** Warn at the worker’s 90% usable-input trigger, before the hard limit. */
const HIGH_UTILIZATION_PERCENTAGE = 90;

/** Legacy count formatting when a maximum is unavailable. */
const NO_MAX_TOKENS_DISPLAY = '-';

/**
 * Digit grouping for token counts. Old app calls `Intl.NumberFormat('fr-FR')`,
 * whose separator character changed between ICU versions (U+00A0 -> U+202F),
 * which would make any assertion on the output version-dependent. This groups
 * explicitly with a non-breaking space instead: same visual result, stable
 * across runtimes.
 */
export function formatNumberWithSpaces(value: number): string {
  if (!Number.isFinite(value)) return '';
  const rounded = Math.round(value);
  const digits = Math.abs(rounded).toString().replace(/\B(?=(\d{3})+(?!\d))/g, GROUP_SEPARATOR);
  return rounded < 0 ? `-${digits}` : digits;
}

/** @public The narrowed, display-ready shape the panel renders. */
export interface ContextBudgetStats {
  readonly budgetMode: ContextBudgetMode;
  readonly usageAvailable: boolean;
  readonly currentTokens: number;
  readonly maxTokens: number;
  /** `"12 000 / 128 000"`, or `"12 000 / -"` when `maxTokens` is 0. */
  readonly tokensDisplay: string;
  /** Undefined until measured. The bar caps over-budget values at 100%. */
  readonly utilizationPercentage: number | undefined;
  readonly isHighUtilization: boolean;
  readonly messageGroups: number;
  readonly summariesGenerated: number;
  /** `context_summarization` -> `context summarization`. */
  readonly strategyName: string;
}

function readNumber(source: Record<string, unknown>, key: string): number {
  const value = source[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

function readAnalytics(wire: Record<string, unknown>): Record<string, unknown> {
  const analytics = wire['context_analytics'];
  return typeof analytics === 'object' && analytics !== null ? (analytics as Record<string, unknown>) : {};
}

function readStrategyName(wire: Record<string, unknown>): string {
  const name = wire['strategy_name'];
  return typeof name === 'string' ? name.replace(/_/g, ' ') : '';
}

/** Percentage of a known budget. Callers separately represent unknown capacity. */
export function deriveUtilizationPercentage(currentTokens: number, maxTokens: number): number {
  if (maxTokens <= 0) return 0;
  return Math.round((currentTokens / maxTokens) * 100);
}

export function formatTokensDisplay(currentTokens: number, maxTokens: number): string {
  const max = maxTokens === 0 ? NO_MAX_TOKENS_DISPLAY : formatNumberWithSpaces(maxTokens);
  return `${formatNumberWithSpaces(currentTokens)} / ${max}`;
}

/** Narrows the wire bag. Returns `undefined` for a missing/non-object response so the caller renders nothing. */
export function toContextBudgetStats(wire: unknown): ContextBudgetStats | undefined {
  if (typeof wire !== 'object' || wire === null) return undefined;
  const source = wire as Record<string, unknown>;

  const currentTokens = readNumber(source, 'current_tokens');
  const maxTokens = readNumber(source, 'max_tokens');
  const unavailable = Array.isArray(source.unavailable) ? source.unavailable : [];
  const usageAvailable = source.context_analytics_available !== false &&
    typeof source.current_tokens === 'number' && maxTokens > 0 &&
    !unavailable.includes('current_tokens') && !unavailable.includes('max_tokens');
  const utilizationPercentage = usageAvailable ? deriveUtilizationPercentage(currentTokens, maxTokens) : undefined;

  return {
    budgetMode: resolveContextBudgetMode(source.budget_mode),
    usageAvailable,
    currentTokens,
    maxTokens,
    tokensDisplay: usageAvailable ? formatTokensDisplay(currentTokens, maxTokens) : '—',
    utilizationPercentage,
    isHighUtilization: usageAvailable && currentTokens * 100 >= maxTokens * HIGH_UTILIZATION_PERCENTAGE,
    messageGroups: readNumber(source, 'message_groups_in_context'),
    summariesGenerated: readNumber(readAnalytics(source), 'summaries_generated'),
    strategyName: readStrategyName(source),
  };
}
