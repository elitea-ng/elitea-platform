/**
 * The project budget warning banner (issue 312).
 *
 * Four properties, and each of them is a way this banner could be worse than
 * having no banner at all:
 *
 *  - it appears only when the SERVER says the threshold is crossed. Deriving
 *    "over 80%" here would compare a null `percent_used` to a number and get
 *    the right answer by coercion, and would compare against 80 while the
 *    project's own threshold is 95;
 *  - it stays away when nothing is over the line, so it does not become the
 *    banner everyone learns to ignore;
 *  - dismissal survives a re-render of THIS session and is scoped to the
 *    project and the billing period, so silencing one project does not silence
 *    another and a new month is a new warning;
 *  - the link goes to the project's Usage page, which is where the reference's
 *    notification click-through lands.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { installWebStorageShim } from '@/test/webstorage';
import { server } from '@/test/setup';

installWebStorageShim();

import { BudgetWarningBanner } from '../ui/BudgetWarningBanner';
import { renderWithNavigation } from './testHarness';

const BUDGET_URL = '*/elitea_core/project_budget/prompt_lib/*/budget';

/** The shape `getProjectBudget` answers. Only four fields are read here. */
function budgetBody(overrides: Record<string, unknown> = {}) {
  return {
    monthly_limit: 100,
    effective_limit: 100,
    limit_source: 'explicit',
    currency: 'USD',
    enabled: true,
    warning_pct: 80,
    spend: 93.5,
    remaining: 6.5,
    percent_used: 93.5,
    warning_active: true,
    spend_available: true,
    period: 'August 2026',
    period_start: '2026-08-01',
    period_end: '2026-08-31',
    resets_at: '2026-09-01T00:00:00Z',
    ...overrides,
  };
}

function serveBudget(overrides: Record<string, unknown> = {}): void {
  server.use(http.get(BUDGET_URL, () => HttpResponse.json(budgetBody(overrides))));
}

beforeEach(() => {
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  window.sessionStorage.clear();
});

describe('BudgetWarningBanner', () => {
  it('warns when the server reports the threshold crossed, naming the project and the percentage', async () => {
    serveBudget();
    await renderWithNavigation(<BudgetWarningBanner projectId="7" projectName="alpha-team" />);

    const banner = await screen.findByTestId('budget-warning-banner');
    // The reference's own copy: "Budget warning: Acme has reached 80% of its
    // monthly budget."
    expect(banner).toHaveTextContent('Budget warning: alpha-team has reached 93% of its monthly budget.');
    expect(screen.getByTestId('budget-warning-usage-link')).toHaveAttribute('href', '/settings/usage');
  });

  it('stays away when the server says the threshold is not crossed', async () => {
    serveBudget({ warning_active: false, percent_used: 40, spend: 40, remaining: 60 });
    await renderWithNavigation(<BudgetWarningBanner projectId="7" projectName="alpha-team" />);

    // Wait for the read to settle, so this is "the answer was no" rather than
    // "the request had not finished yet".
    await waitFor(() => expect(screen.queryByTestId('budget-warning-banner')).not.toBeInTheDocument());
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByTestId('budget-warning-banner')).not.toBeInTheDocument();
  });

  it('stays away for a project with no budget, where percent_used is null', async () => {
    serveBudget({
      warning_active: false,
      monthly_limit: null,
      effective_limit: null,
      limit_source: 'unlimited',
      percent_used: null,
      remaining: null,
    });
    await renderWithNavigation(<BudgetWarningBanner projectId="7" projectName="alpha-team" />);

    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByTestId('budget-warning-banner')).not.toBeInTheDocument();
  });

  it('asks nothing at all while no project is selected', async () => {
    let asked = 0;
    server.use(
      http.get(BUDGET_URL, () => {
        asked += 1;
        return HttpResponse.json(budgetBody());
      }),
    );
    await renderWithNavigation(<BudgetWarningBanner projectId="" projectName="" />);

    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(asked).toBe(0);
    expect(screen.queryByTestId('budget-warning-banner')).not.toBeInTheDocument();
  });

  it('dismissal persists for the session, keyed on the project and the period', async () => {
    serveBudget();
    const user = userEvent.setup();
    await renderWithNavigation(<BudgetWarningBanner projectId="7" projectName="alpha-team" />);

    await screen.findByTestId('budget-warning-banner');
    await user.click(screen.getByRole('button', { name: 'Dismiss' }));
    await waitFor(() =>
      expect(screen.queryByTestId('budget-warning-banner')).not.toBeInTheDocument(),
    );

    // sessionStorage, namespaced so the logout sweep reaches it, and carrying
    // both the project and the period end — a different project or a new
    // billing period is a different key and warns again.
    expect(window.sessionStorage.getItem('el.budgetWarning.dismissed.7.2026-08-31')).toBe('1');
  });

  it('a dismissal already stored for this project and period suppresses the banner on mount', async () => {
    window.sessionStorage.setItem('el.budgetWarning.dismissed.7.2026-08-31', '1');
    serveBudget();
    await renderWithNavigation(<BudgetWarningBanner projectId="7" projectName="alpha-team" />);

    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByTestId('budget-warning-banner')).not.toBeInTheDocument();
  });

  it('a dismissal stored for ANOTHER period does not silence the new one', async () => {
    window.sessionStorage.setItem('el.budgetWarning.dismissed.7.2026-07-31', '1');
    serveBudget();
    await renderWithNavigation(<BudgetWarningBanner projectId="7" projectName="alpha-team" />);

    expect(await screen.findByTestId('budget-warning-banner')).toBeInTheDocument();
  });

  it('falls back to a neutral subject when the project has no name yet', async () => {
    serveBudget();
    await renderWithNavigation(<BudgetWarningBanner projectId="7" />);

    expect(await screen.findByTestId('budget-warning-banner')).toHaveTextContent(
      'Budget warning: this project has reached 93% of its monthly budget.',
    );
  });
});
