import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { ContextBudgetStats } from '../lib/contextStatus';
import { toContextBudgetStats } from '../lib/contextStatus';
import { ContextBudgetCollapsed, ContextBudgetPanel } from './ContextBudgetPanel';

const NBSP = '\u00a0';

function statsFrom(overrides: Readonly<Record<string, unknown>> = {}): ContextBudgetStats {
  const stats = toContextBudgetStats({
    current_tokens: 12000,
    max_tokens: 128000,
    message_groups_in_context: 9,
    strategy_name: 'sliding_window_with_summary',
    context_analytics: { summaries_generated: 3 },
    ...overrides,
  });
  if (!stats) throw new Error('fixture did not narrow');
  return stats;
}

describe('ContextBudgetPanel', () => {
  it('renders the grouped token counts, the percentage and the three stat rows', () => {
    const { getByTestId } = renderWithTheme(<ContextBudgetPanel stats={statsFrom()} />);

    expect(getByTestId('context-budget-tokens').textContent).toBe(`12${NBSP}000 / 128${NBSP}000 tokens`);
    expect(getByTestId('context-budget-utilization').textContent).toBe('9%');
    expect(getByTestId('context-budget-stat-messages').textContent).toBe('Messages:9');
    expect(getByTestId('context-budget-stat-summaries').textContent).toBe('Summaries:3');
    expect(getByTestId('context-budget-stat-strategy').textContent).toBe('Strategy:sliding window with summary');
  });

  it('renders a dash for the maximum and only the Messages row when the context manager is off', () => {
    const { getByTestId, queryByTestId } = renderWithTheme(
      <ContextBudgetPanel stats={statsFrom({ max_tokens: 0 })} />,
    );

    expect(getByTestId('context-budget-tokens').textContent).toBe(`12${NBSP}000 / - tokens`);
    expect(getByTestId('context-budget-utilization').textContent).toBe('0%');
    expect(getByTestId('context-budget-stat-messages').textContent).toBe('Messages:9');
    expect(queryByTestId('context-budget-stat-summaries')).toBeNull();
    expect(queryByTestId('context-budget-stat-strategy')).toBeNull();
  });

  it('shows the warning icon and caps the bar once the budget is exceeded', () => {
    const { getByTestId } = renderWithTheme(
      <ContextBudgetPanel stats={statsFrom({ current_tokens: 200000 })} />,
    );

    expect(getByTestId('context-budget-utilization').textContent).toBe('156%');
    expect(getByTestId('context-budget-attention-icon')).toBeTruthy();
    // The bar caps at 100 even though the label above it says 156% — the
    // width comes from the same capped number.
    expect(getByTestId('context-budget-progress').getAttribute('data-percentage')).toBe('100');
  });

  it('omits the warning icon below the budget and sizes the bar to the real percentage', () => {
    const { getByTestId, queryByTestId } = renderWithTheme(<ContextBudgetPanel stats={statsFrom()} />);

    expect(queryByTestId('context-budget-attention-icon')).toBeNull();
    expect(getByTestId('context-budget-progress').getAttribute('data-percentage')).toBe('9');
  });
});

/**
 * The collapsed rail's own form, which did not exist.
 *
 * `ContextBudget` rendered the full card whatever the rail's width, so in the
 * 3.25rem collapsed rail every line of it wrapped to one word and the card
 * overflowed past the right edge of the viewport (measured: its children sat
 * at x=2002 in a 2000px viewport). The production rail shows the percentage
 * and a short status line there, and nothing else — `ContextBudgetInfo.jsx`'s
 * `if (collapsed) return <ContextBudgetCollapsed/>` branch.
 */
describe('ContextBudgetCollapsed', () => {
  it('shows only the percentage and the status line', () => {
    const { getByTestId, queryByTestId } = renderWithTheme(
      <ContextBudgetCollapsed stats={statsFrom()} />,
    );

    expect(getByTestId('context-budget-collapsed')).toBeTruthy();
    expect(getByTestId('context-budget-utilization').textContent).toBe('9%');
    expect(getByTestId('context-budget-progress').getAttribute('data-percentage')).toBe('9');
    // None of the card's own rows: no title, no token line, no stat rows.
    expect(queryByTestId('context-budget-tokens')).toBeNull();
    expect(queryByTestId('context-budget-stat-messages')).toBeNull();
    expect(queryByTestId('context-budget-stat-summaries')).toBeNull();
    expect(queryByTestId('context-budget-stat-strategy')).toBeNull();
  });

  it('caps the status line at 100% while the label keeps the true figure', () => {
    const { getByTestId } = renderWithTheme(
      <ContextBudgetCollapsed stats={statsFrom({ current_tokens: 200000 })} />,
    );

    expect(getByTestId('context-budget-utilization').textContent).toBe('156%');
    expect(getByTestId('context-budget-progress').getAttribute('data-percentage')).toBe('100');
  });
});
