/**
 * The display logic of the panel lives in these pure functions, so this is
 * where the two rules that are easy to get wrong are pinned: `max_tokens === 0`
 * must render as `-` (never `0`), and both counts must be digit-grouped.
 */
import { describe, expect, it } from 'vitest';

import {
  deriveUtilizationPercentage,
  formatNumberWithSpaces,
  formatTokensDisplay,
  toContextBudgetStats,
} from './contextStatus';

const NBSP = ' ';

describe('formatNumberWithSpaces', () => {
  it.each([
    [0, '0'],
    [7, '7'],
    [999, '999'],
    [1000, `1${NBSP}000`],
    [12345, `12${NBSP}345`],
    [128000, `128${NBSP}000`],
    [1234567, `1${NBSP}234${NBSP}567`],
  ])('groups %s as %s', (input, expected) => {
    expect(formatNumberWithSpaces(input)).toBe(expected);
  });

  it('rounds a fractional count rather than grouping the decimals', () => {
    expect(formatNumberWithSpaces(1000.4)).toBe(`1${NBSP}000`);
  });

  it('keeps the sign outside the grouping', () => {
    expect(formatNumberWithSpaces(-1234)).toBe(`-1${NBSP}234`);
  });

  it('returns an empty string for a non-finite count instead of "NaN"', () => {
    expect(formatNumberWithSpaces(Number.NaN)).toBe('');
    expect(formatNumberWithSpaces(Number.POSITIVE_INFINITY)).toBe('');
  });
});

describe('formatTokensDisplay', () => {
  it('renders max_tokens === 0 as a dash, not as a zero', () => {
    expect(formatTokensDisplay(1500, 0)).toBe(`1${NBSP}500 / -`);
  });

  it('groups both numbers when there is a budget', () => {
    expect(formatTokensDisplay(12000, 128000)).toBe(`12${NBSP}000 / 128${NBSP}000`);
  });
});

describe('deriveUtilizationPercentage', () => {
  it('is 0 when there is no budget to divide by', () => {
    expect(deriveUtilizationPercentage(1500, 0)).toBe(0);
  });

  it.each([
    [0, 128000, 0],
    [12800, 128000, 10],
    [64000, 128000, 50],
    [128000, 128000, 100],
    [200000, 128000, 156],
  ])('reports %s of %s as %s%%', (current, max, expected) => {
    expect(deriveUtilizationPercentage(current, max)).toBe(expected);
  });
});

describe('toContextBudgetStats', () => {
  const wire = {
    current_tokens: 12000,
    max_tokens: 128000,
    message_groups_in_context: 9,
    strategy_name: 'sliding_window_with_summary',
    utilization: 9.375,
    context_analytics: { summaries_generated: 3 },
  };

  it('narrows the wire bag into display-ready stats', () => {
    expect(toContextBudgetStats(wire)).toEqual({
      budgetMode: 'balanced',
      usageAvailable: true,
      currentTokens: 12000,
      maxTokens: 128000,
      tokensDisplay: `12${NBSP}000 / 128${NBSP}000`,
      utilizationPercentage: 9,
      isHighUtilization: false,
      messageGroups: 9,
      summariesGenerated: 3,
      strategyName: 'sliding window with summary',
    });
  });

  it('ignores the wire `utilization` field, whose scale differs between backends', () => {
    // The Go route already returns a percentage (9.375), the old Python one a
    // fraction. Reading it directly would render either 9% or 938%.
    expect(toContextBudgetStats({ ...wire, utilization: 0.09375 })?.utilizationPercentage).toBe(9);
  });

  it('flags utilization at or above 90% as high', () => {
    expect(toContextBudgetStats({ ...wire, current_tokens: 128000 })?.isHighUtilization).toBe(true);
    expect(toContextBudgetStats({ ...wire, current_tokens: 115000 })?.isHighUtilization).toBe(false);
  });

  it('treats a missing analytics object as zero summaries rather than throwing', () => {
    const { context_analytics: _dropped, ...withoutAnalytics } = wire;
    expect(toContextBudgetStats(withoutAnalytics)?.summariesGenerated).toBe(0);
  });

  it('defaults every absent or wrongly-typed count to 0 and the strategy to an empty name', () => {
    expect(toContextBudgetStats({ current_tokens: '12000', strategy_name: 42 })).toEqual({
      budgetMode: 'balanced',
      usageAvailable: false,
      currentTokens: 0,
      maxTokens: 0,
      tokensDisplay: '—',
      utilizationPercentage: undefined,
      isHighUtilization: false,
      messageGroups: 0,
      summariesGenerated: 0,
      strategyName: '',
    });
  });

  it('returns undefined for a non-object payload so the caller can render nothing', () => {
    expect(toContextBudgetStats(undefined)).toBeUndefined();
    expect(toContextBudgetStats(null)).toBeUndefined();
    expect(toContextBudgetStats('nope')).toBeUndefined();
  });
});

it('does not report absent or explicitly unavailable analytics as measured zero', () => {
  for (const availability of [{ context_analytics_available: false }, { unavailable: ['current_tokens'] }, { unavailable: ['max_tokens'] }]) {
    const stats = toContextBudgetStats({ current_tokens: 0, max_tokens: 272000, ...availability });
    expect(stats?.usageAvailable).toBe(false);
    expect(stats?.utilizationPercentage).toBeUndefined();
  }
});
