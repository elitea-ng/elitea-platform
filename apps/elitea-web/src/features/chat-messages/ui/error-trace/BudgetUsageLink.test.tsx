/**
 * #6732: a budget refusal offers the way to Usage, labelled for whose budget
 * ran out; any other failure offers nothing.
 */
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import {
  BudgetUsageLink,
  budgetUsageHref,
  isBudgetFailureCode,
  toolResultBudgetCode,
  usageSettingsHref,
} from './BudgetUsageLink';

describe('BudgetUsageLink (#6732)', () => {
  it('names the member scope', () => {
    render(<BudgetUsageLink code="MEMBER_BUDGET_EXHAUSTED" />);
    const link = screen.getByTestId('budget-usage-link');
    expect(link).toHaveTextContent('View my usage');
    // The member view reads the caller's own budget, not the project's.
    expect(link.getAttribute('href')).toMatch(/\/settings\/usage\?scope=user$/);
  });

  it.each(['PROJECT_BUDGET_EXHAUSTED', 'MODEL_BUDGET_EXHAUSTED'])('names the project scope for %s', (code) => {
    render(<BudgetUsageLink code={code} />);
    const link = screen.getByTestId('budget-usage-link');
    expect(link).toHaveTextContent('View project usage');
    expect(link.getAttribute('href')).toMatch(/\/settings\/usage$/);
  });

  it('builds the member view under the configured app base', () => {
    expect(budgetUsageHref('/app/', 'MEMBER_BUDGET_EXHAUSTED')).toBe('/app/settings/usage?scope=user');
    expect(budgetUsageHref('/app/', 'PROJECT_BUDGET_EXHAUSTED')).toBe('/app/settings/usage');
  });

  it('renders nothing for another failure', () => {
    const { container } = render(<BudgetUsageLink code="MODEL_TIMEOUT" />);
    expect(container).toBeEmptyDOMElement();
    expect(isBudgetFailureCode(undefined)).toBe(false);
  });

  it('joins the configured app base without a double slash', () => {
    expect(usageSettingsHref('/app/')).toBe('/app/settings/usage');
    expect(usageSettingsHref('')).toBe('/settings/usage');
  });

  // #1238: a metered tool refused by the budget returns an error result with
  // the same public code a refused model call fails the turn with.
  it('reads the budget code of a refused tool result', () => {
    const refused = JSON.stringify({
      error: {
        code: 'model_gateway.member_budget_exhausted',
        public_code: 'MEMBER_BUDGET_EXHAUSTED',
        message: 'web_search was refused: your budget for this billing period is exhausted. The provider was not called.',
      },
    });
    const code = toolResultBudgetCode(refused);
    expect(code).toBe('MEMBER_BUDGET_EXHAUSTED');
    render(<BudgetUsageLink code={code} />);
    expect(screen.getByTestId('budget-usage-link').getAttribute('href')).toMatch(/\/settings\/usage\?scope=user$/);
  });

  it('reads no budget code from another tool error', () => {
    expect(toolResultBudgetCode(JSON.stringify({ error: { code: 'web_provider_unpriced', public_code: 'TOOL_ERROR' } }))).toBeUndefined();
    expect(toolResultBudgetCode('plain text failure')).toBeUndefined();
    expect(toolResultBudgetCode(undefined)).toBeUndefined();
  });
});
