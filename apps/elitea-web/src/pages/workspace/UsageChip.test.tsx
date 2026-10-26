import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { budgetLine, costOf, describeUsage, modelPrice, threadUsage, UsageChipView, type ModelPrice, type TurnUsage } from './UsageChip';

const turn = (over: Partial<TurnUsage> = {}): TurnUsage => ({ prompt: 10_000, completion: 500, cached: 6_000, cache_write: 0, reasoning: 0, calls: 2, final: true, model: 'gpt-x', ...over });
const price: ModelPrice = { input: 2, output: 8, cachedInput: 2, currency: 'USD' };
const priced = { name: 'gpt-x', input_cost_per_1m_tokens: 2, output_cost_per_1m_tokens: 8, cached_input_cost_per_1m_tokens: 2, currency: 'USD' };

function shown(turns: TurnUsage[], catalogue: unknown): HTMLElement {
  const priceOf = (model: string | undefined) => modelPrice(catalogue, model);
  const latest = turns[turns.length - 1] as TurnUsage;
  render(<UsageChipView text={describeUsage(latest, threadUsage(turns, priceOf), priceOf(latest.model), undefined)} />);
  return screen.getByTestId('usage-chip');
}

describe('UsageChip', () => {
  it('shows tokens and no currency figure when the catalogue names no price', () => {
    const chip = shown([turn()], { items: [{ name: 'gpt-x', input_cost_per_1m_tokens: null, output_cost_per_1m_tokens: null }] });
    expect(chip).toHaveTextContent('≈ 10.5K tokens');
    expect(chip.textContent).not.toMatch(/\$|USD/);
    expect(chip).toHaveAttribute('data-cached-percent', '60');
  });

  it('shows the cost from the catalogue’s price', () => {
    expect(shown([turn()], { items: [priced] })).toHaveTextContent(/0\.02 · 10\.5K tokens/);
  });

  it('reads a price only when all of it is there', () => {
    expect(modelPrice({ items: [priced] }, 'gpt-x')).toEqual(price);
    expect(modelPrice({ items: [{ ...priced, currency: null }] }, 'gpt-x')).toBeUndefined();
    expect(modelPrice({ items: [{ ...priced, output_cost_per_1m_tokens: null }] }, 'gpt-x')).toBeUndefined();
    expect(modelPrice({ items: [priced] }, 'other')).toBeUndefined();
    expect(modelPrice({ items: [priced] }, undefined)).toBeUndefined();
  });

  it('prices cached input at its own price', () => {
    expect(costOf(turn(), { ...price, cachedInput: 0.5 })).toBeCloseTo((4_000 * 2 + 6_000 * 0.5 + 500 * 8) / 1_000_000);
  });

  it('sums the thread and costs it only when every turn has a price', () => {
    const turns = [turn({ model: 'gpt-x' }), turn({ model: 'gpt-y' })];
    const both = threadUsage(turns, () => price);
    expect(both?.counts.prompt).toBe(20_000);
    expect(both?.cost?.amount).toBeCloseTo(2 * costOf(turn(), price));
    expect(threadUsage(turns, (model) => (model === 'gpt-x' ? price : undefined))?.cost).toBeUndefined();
    expect(threadUsage([], () => price)).toBeUndefined();
  });

  it('names the sub-agents, a running turn, the thread and the budget in its tooltip text', () => {
    const budget = budgetLine({ can_see_amounts: true, effective_limit: 100, percent_used: 41.6, currency: 'USD' });
    const text = describeUsage(turn({ final: false, subagents: { prompt: 900, completion: 100, cached: 0, cache_write: 0, reasoning: 0, calls: 1 } }), threadUsage([turn()], () => undefined), undefined, budget);
    expect(text.detail).toContain('This turn: 10K in (60% cached), 500 out');
    expect(text.detail).toContain('Sub-agents: 1K tokens');
    expect(text.detail).toContain('Still running');
    expect(text.detail).toContain('This thread: 10.5K tokens');
    expect(text.detail).toMatch(/Your budget: 42% of \S*100\.00 used this period/);
    expect(budgetLine({ can_see_amounts: false, effective_limit: 100, currency: 'USD' })).toBeUndefined();
  });
});
