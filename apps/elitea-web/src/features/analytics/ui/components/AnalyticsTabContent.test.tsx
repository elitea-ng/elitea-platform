import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { RenderResult } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { ProjectAnalytics } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../../test/setup';
import { ANALYTICS_TAB } from '../../lib/constants';
import { AnalyticsTabContent } from './AnalyticsTabContent';

const BASE = '/api/v2';

const DATA: ProjectAnalytics = {
  kpis: {
    total_project_users: 2,
    ai_active_users: 1,
    adoption_rate: 50,
    llm_calls: 1,
    total_tokens: 33,
  },
  top_ai_users: [],
  daily_activity: [],
  models: [],
  models_truncated: false,
};

function renderScreen(ui: ReactElement): RenderResult {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

const NOOP = (): void => {};

describe('AnalyticsTabContent', () => {
  it('shows a spinner while needsOverview and isFetching are both true', () => {
    const { getByRole } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.overview}
        needsOverview
        isFetching
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(getByRole('progressbar')).toBeInTheDocument();
  });

  it('shows the error message when needsOverview and isError are both true', () => {
    const { getByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.overview}
        needsOverview
        isFetching={false}
        isError
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(getByText('Failed to load analytics data.')).toBeInTheDocument();
  });

  it('renders null for the Overview tab when data is undefined but not loading/erroring', () => {
    const { container } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.overview}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('renders AnalyticsOverview for tab 0 when data is present', () => {
    const { getByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.overview}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={DATA}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(getByText('AI ACTIVE')).toBeInTheDocument();
  });

  it('renders AnalyticsAgents for the agents tab', async () => {
    server.use(http.get(`${BASE}/elitea_core/analytics_agents/prompt_lib/7`, () => HttpResponse.json({ items: [] })));
    const { findByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.agents}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(await findByText('Agent Activity')).toBeInTheDocument();
  });

  it('renders AnalyticsTools for the tools tab', async () => {
    server.use(http.get(`${BASE}/elitea_core/analytics_tools/prompt_lib/7`, () => HttpResponse.json({ items: [] })));
    const { findByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.tools}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(await findByText('Tool Details')).toBeInTheDocument();
  });

  it('renders AnalyticsUsers for the users tab', async () => {
    server.use(http.get(`${BASE}/elitea_core/analytics_users/prompt_lib/7`, () => HttpResponse.json({ items: [] })));
    const { findByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.users}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(await findByText('User Activity')).toBeInTheDocument();
  });

  it('renders null for the health tab when data is undefined', () => {
    const { container } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.health}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('renders AnalyticsHealth for the health tab when data is present', () => {
    const { getByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.health}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={DATA}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(getByText('No health data available.')).toBeInTheDocument();
  });

  it('renders AnalyticsGuide for the guide tab', () => {
    const { getByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.guide}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(getByText('Overview Tab')).toBeInTheDocument();
  });

  // The two tabs the reference deployment has and this one did not. They are
  // asserted through the SWITCH, not by rendering the component directly: the
  // defect class this file exists for is a tab wired to the wrong case, and
  // rendering a component in isolation cannot see it.
  it('renders AnalyticsCosts for the costs tab', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/analytics_costs/prompt_lib/7`, () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const { findByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.costs}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    // The costs tab owns its query, so its own failure state is what proves the
    // switch reached it rather than the container's.
    expect(await findByText('Failed to load analytics data.')).toBeInTheDocument();
  });

  it('renders AnalyticsTokens for the tokens tab', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/analytics_costs/prompt_lib/7`, () =>
        HttpResponse.json({
          kpis: { total_cost: 0, currency: 'USD', periods: 0, spend_available: false, window_days: 7 },
          periods: [],
          by_scope: [],
          periods_truncated: false,
          date_from: '2026-08-01T00:00:00Z',
          date_to: '2026-08-02T00:00:00Z',
          estimate: {
            token_dimension_available: true,
            cost_dimension_available: false,
            cache_dimension_available: false,
            currency: 'USD',
            priced_calls: 0,
            unpriced_calls: 1,
            totals: { calls: 1, prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 },
            by_model: [],
            by_user: [],
            daily: [],
            by_model_truncated: false,
            by_user_truncated: false,
          },
        }),
      ),
    );
    const { findByText } = renderScreen(
      <AnalyticsTabContent
        activeTab={ANALYTICS_TAB.tokens}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect((await findByText('TOTAL TOKENS')).parentElement).toHaveTextContent('15');
  });

  it('renders null for an out-of-range tab index (defensive default branch)', () => {
    const { container } = renderScreen(
      <AnalyticsTabContent
        activeTab={99}
        needsOverview={false}
        isFetching={false}
        isError={false}
        data={undefined}
        projectId="7"
        dateFrom=""
        dateTo=""
        pendingUserId={null}
        onUserClick={NOOP}
        onBackToSource={NOOP}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
