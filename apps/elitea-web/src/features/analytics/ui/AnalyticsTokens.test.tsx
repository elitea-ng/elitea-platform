import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { RenderResult } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AnalyticsUsageEstimate } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';
import { AnalyticsTokens } from './AnalyticsTokens';

/**
 * The Tokens tab.
 *
 * Every case asserts a FIGURE or the ABSENCE of one. That is the point: the
 * defect this tab exists to close was a TOKENS tile reading 0 beside a correct
 * CALLS tile, and a test that only checked the component rendered would have
 * passed against it.
 */

const BASE = '/api/v2';
const RANGE = { dateFrom: '2026-07-20T00:00:00.000Z', dateTo: '2026-07-27T00:00:00.000Z' };
const COSTS_URL = `${BASE}/elitea_core/analytics_costs/prompt_lib/7`;

const ESTIMATE: AnalyticsUsageEstimate = {
  token_dimension_available: true,
  cost_dimension_available: false,
  cache_dimension_available: false,
  currency: 'USD',
  priced_calls: 0,
  unpriced_calls: 2,
  totals: { calls: 2, prompt_tokens: 2351, completion_tokens: 42, total_tokens: 2393 },
  by_model: [
    {
      provider: 'vllm',
      model: 'qwen3',
      calls: 2,
      prompt_tokens: 2351,
      completion_tokens: 42,
      total_tokens: 2393,
      priced: false,
    },
  ],
  by_user: [
    {
      user_id: 7,
      email: 'admin@client.local',
      name: 'Client Admin',
      calls: 2,
      prompt_tokens: 2351,
      completion_tokens: 42,
      total_tokens: 2393,
      priced: false,
    },
  ],
  daily: [
    { date: '2026-07-25', calls: 1, prompt_tokens: 351, completion_tokens: 2, total_tokens: 353 },
    { date: '2026-07-26', calls: 1, prompt_tokens: 2000, completion_tokens: 40, total_tokens: 2040 },
  ],
  by_model_truncated: false,
  by_user_truncated: false,
};

const COSTS_BODY = {
  kpis: { total_cost: 0, currency: 'USD', periods: 0, spend_available: false, window_days: 7 },
  periods: [],
  by_scope: [],
  periods_truncated: false,
  date_from: RANGE.dateFrom,
  date_to: RANGE.dateTo,
};

function renderScreen(ui: ReactElement): RenderResult {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

function serveEstimate(estimate: AnalyticsUsageEstimate | undefined): void {
  server.use(
    http.get(COSTS_URL, () =>
      HttpResponse.json(estimate === undefined ? COSTS_BODY : { ...COSTS_BODY, estimate }),
    ),
  );
}

function renderTab(): RenderResult {
  return renderScreen(
    <AnalyticsTokens
      projectId="7"
      dateFrom={RANGE.dateFrom}
      dateTo={RANGE.dateTo}
    />,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AnalyticsTokens', () => {
  it('reports the recorded token counts on the tiles', async () => {
    serveEstimate(ESTIMATE);
    const { findByText, getByText } = renderTab();

    // 2393 formats as 2.4K, and it appears on the tile and in both tables — so
    // the assertion is scoped to the tile that carries the label.
    expect((await findByText('TOTAL TOKENS')).parentElement).toHaveTextContent('2.4K');
    expect(getByText('INPUT TOKENS').parentElement).toHaveTextContent('2.4K');
    expect(getByText('OUTPUT TOKENS').parentElement).toHaveTextContent('42');
    expect(getByText('CALLS').parentElement).toHaveTextContent('2');
  });

  // The control for the case above. Without it a component that hardcoded
  // "2.4K" would pass every assertion in it.
  it('reports a different set of counts for a different response', async () => {
    serveEstimate({
      ...ESTIMATE,
      totals: { calls: 9, prompt_tokens: 11, completion_tokens: 7, total_tokens: 18 },
    });
    const { findByText, getByText } = renderTab();

    expect((await findByText('TOTAL TOKENS')).parentElement).toHaveTextContent('18');
    expect(getByText('INPUT TOKENS').parentElement).toHaveTextContent('11');
    expect(getByText('OUTPUT TOKENS').parentElement).toHaveTextContent('7');
    expect(getByText('CALLS').parentElement).toHaveTextContent('9');
  });

  it('lists the per-user and per-model breakdowns with their shares', async () => {
    serveEstimate(ESTIMATE);
    const { findByText, getByText } = renderTab();

    expect(await findByText('Token Usage by User')).toBeVisible();
    expect(getByText('admin@client.local')).toBeVisible();
    expect(getByText('Token Usage by Model')).toBeVisible();
    expect(getByText('qwen3')).toBeVisible();
    // One row of each table holds the whole window, so both shares read 100.0%.
    expect(getByText('Token Usage by User').parentElement).toHaveTextContent('100.0%');
  });

  it('names a caller by id when the identity corpus cannot name them', async () => {
    serveEstimate({
      ...ESTIMATE,
      by_user: [{ ...ESTIMATE.by_user[0]!, email: '', name: '' }],
    });
    const { findByText } = renderTab();

    expect(await findByText('User 7')).toBeVisible();
  });

  // The cache tiles the reference deployment shows are ABSENT here, and the tab
  // says why. Rendering them as 0 would claim the project read nothing from
  // cache, which is a different and unmeasured statement.
  it('omits the cache tiles and states that the counts are not recorded', async () => {
    serveEstimate(ESTIMATE);
    const { findByText, queryByText } = renderTab();

    expect(
      await findByText(
        'Cache read and cache write tokens are not recorded on this deployment, so they are not shown.',
      ),
    ).toBeVisible();
    expect(queryByText('CACHE READ')).not.toBeInTheDocument();
    expect(queryByText('CACHE WRITE')).not.toBeInTheDocument();
  });

  it('draws the daily chart when the window had traffic', async () => {
    serveEstimate(ESTIMATE);
    const { findByText, container } = renderTab();

    expect(await findByText('Daily Token Usage')).toBeVisible();
    expect(container.querySelector('.recharts-responsive-container')).not.toBeNull();
  });

  it('omits the chart when no day had traffic', async () => {
    serveEstimate({ ...ESTIMATE, daily: [] });
    const { findByText, queryByText } = renderTab();

    expect(await findByText('TOTAL TOKENS')).toBeVisible();
    expect(queryByText('Daily Token Usage')).not.toBeInTheDocument();
  });

  it('states that the deployment has no source when the estimate block is absent', async () => {
    serveEstimate(undefined);
    const { findByText, queryByText } = renderTab();

    expect(await findByText('Not available on this deployment')).toBeVisible();
    expect(
      await findByText(
        'This deployment carries no gateway request log, which is where token counts are recorded.',
      ),
    ).toBeVisible();
    // And no fabricated tiles beside it.
    expect(queryByText('TOTAL TOKENS')).not.toBeInTheDocument();
  });

  it('shows the load error and no tiles when the query fails', async () => {
    server.use(http.get(COSTS_URL, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { findByText, queryByText } = renderTab();

    expect(await findByText('Failed to load analytics data.')).toBeVisible();
    expect(queryByText('TOTAL TOKENS')).not.toBeInTheDocument();
    expect(queryByText('Token Usage by Model')).not.toBeInTheDocument();
  });

  it('says the list was capped when the server truncated it', async () => {
    serveEstimate({ ...ESTIMATE, by_model_truncated: true, by_user_truncated: true });
    const { findByText, getByText } = renderTab();

    expect(await findByText('The busiest models; the list was capped')).toBeVisible();
    expect(getByText('The busiest callers; the list was capped')).toBeVisible();
  });

  it('renders an empty-table message rather than an empty table body', async () => {
    serveEstimate({ ...ESTIMATE, by_model: [], by_user: [] });
    const { findByText, getByText, queryByText } = renderTab();

    expect(await findByText('No model was called in this window.')).toBeVisible();
    expect(getByText('No call in this window resolved to a user.')).toBeVisible();
    expect(queryByText('Share')).not.toBeInTheDocument();
  });
});
