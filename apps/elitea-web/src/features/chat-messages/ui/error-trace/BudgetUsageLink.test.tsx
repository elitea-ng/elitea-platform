/**
 * #6732: a budget refusal offers the way to Usage, labelled for whose budget
 * ran out; any other failure offers nothing.
 */
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { BudgetUsageLink, isBudgetFailureCode, usageSettingsHref } from './BudgetUsageLink';

describe('BudgetUsageLink (#6732)', () => {
  it('names the member scope', () => {
    render(<BudgetUsageLink code="MEMBER_BUDGET_EXHAUSTED" />);
    const link = screen.getByTestId('budget-usage-link');
    expect(link).toHaveTextContent('View my usage');
    expect(link.getAttribute('href')).toMatch(/\/settings\/usage$/);
  });

  it.each(['PROJECT_BUDGET_EXHAUSTED', 'MODEL_BUDGET_EXHAUSTED'])('names the project scope for %s', (code) => {
    render(<BudgetUsageLink code={code} />);
    expect(screen.getByTestId('budget-usage-link')).toHaveTextContent('View project usage');
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
});
