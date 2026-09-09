import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { RenderResult } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AnalyticsUsageEstimate } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';
import { AnalyticsCosts } from './AnalyticsCosts';

/**
 * The Costs tab.
 *
 * The cases that matter most are the ones about a figure NOT shown: a model the
 * catalogue does not price must not read `$0.00`, and a window with no price at
 * all must not render a cost screen full of zeros. Both would make an
 * under-priced deployment look free.
 */

const BASE = '/api/v2';
const RANGE = { dateFrom: '2026-07-20T00:00:00.000Z', dateTo: '2026-07-27T00:00:00.000Z' };
const COSTS_URL = `${BASE}/elitea_core/analytics_costs/prompt_lib/7`;

const PRICED: AnalyticsUsageEstimate = {
  token_dimension_available: true,
  cost_dimension_available: true,
  cache_dimension_available: false,
  currency: 'USD',
  priced_calls: 3,
  unpriced_calls: 0,
  totals: {
    calls: 3,
    prompt_tokens: 2351,
    completion_tokens: 42,
    total_tokens: 2393,
    input_cost: 0.001763,
    output_cost: 0.000189,
    total_cost: 0.001952,
  },
  by_model: [
    {
      provider: 'openai',
      model: 'gpt-5.4-mini',
      calls: 3,
      prompt_tokens: 2351,
      completion_tokens: 42,
      total_tokens: 2393,
      priced: true,
      input_cost: 0.001763,
      output_cost: 0.000189,
      total_cost: 0.001952,
    },
  ],
  by_user: [
    {
      user_id: 7,
      email: 'admin@client.local',
      name: 'Client Admin',
      calls: 3,
      prompt_tokens: 2351,
      completion_tokens: 42,
      total_tokens: 2393,
      priced: true,
      input_cost: 0.001763,
      output_cost: 0.000189,
      total_cost: 0.001952,
    },
  ],
  daily: [
    {
      date: '2026-07-26',
      calls: 3,
      prompt_tokens: 2351,
      completion_tokens: 42,
      total_tokens: 2393,
      input_cost: 0.001763,
      output_cost: 0.000189,
      total_cost: 0.001952,
    },
  ],
  by_model_truncated: false,
  by_user_truncated: false,
  agent_dimension_available: true,
  attributed_agent_calls: 2,
  unattributed_agent_calls: 1,
  by_agent: [
    {
      application_id: '4001',
      name: 'Research Agent',
      calls: 2,
      prompt_tokens: 1500,
      completion_tokens: 30,
      total_tokens: 1530,
      priced: true,
      input_cost: 0.001125,
      output_cost: 0.000135,
      total_cost: 0.00126,
    },
  ],
  by_agent_truncated: false,
  tool_dimension_available: true,
  attributed_tool_calls: 1,
  unattributed_tool_calls: 0,
  by_tool: [
    {
      toolkit_id: '9',
      toolkit_name: 'Jira',
      tool_name: 'list_issues',
      attributed_runs: 1,
      prompt_tokens: 800,
      completion_tokens: 15,
      total_tokens: 815,
      priced: true,
      input_cost: 0.0006,
      output_cost: 0.0000675,
      total_cost: 0.0006675,
    },
  ],
  by_tool_truncated: false,
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
    <AnalyticsCosts
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

describe('AnalyticsCosts', () => {
  // Six fraction digits, not two. A window of a few calls costs well under one
  // cent, and `$0.00` on every tile reads as "this project spent nothing".
  it('shows sub-cent amounts at the precision that keeps them visible', async () => {
    serveEstimate(PRICED);
    const { findByText, getByText } = renderTab();

    expect((await findByText('TOTAL COST')).parentElement).toHaveTextContent('$0.001952');
    expect(getByText('INPUT TOKEN COST').parentElement).toHaveTextContent('$0.001763');
    expect(getByText('OUTPUT TOKEN COST').parentElement).toHaveTextContent('$0.000189');
  });

  // The control: a component printing a fixed string would pass the case above.
  it('shows a whole-dollar amount with two digits', async () => {
    serveEstimate({
      ...PRICED,
      totals: { ...PRICED.totals, input_cost: 12, output_cost: 6.5, total_cost: 18.5 },
    });
    const { findByText, getByText } = renderTab();

    expect((await findByText('TOTAL COST')).parentElement).toHaveTextContent('$18.50');
    expect(getByText('INPUT TOKEN COST').parentElement).toHaveTextContent('$12.00');
  });

  it('states that the figures are an estimate, not the accounted spend', async () => {
    serveEstimate(PRICED);
    const { findByText } = renderTab();

    expect(
      await findByText(
        'Costs are estimated from the local model-price table. The accounted figure is on the Overview tab; actual provider invoices may differ.',
      ),
    ).toBeVisible();
  });

  it('lists the per-user and per-model cost breakdowns', async () => {
    serveEstimate(PRICED);
    const { findByText, getByText } = renderTab();

    expect(await findByText('Cost by User')).toBeVisible();
    expect(getByText('admin@client.local')).toBeVisible();
    expect(getByText('Cost by Model')).toBeVisible();
    expect(getByText('gpt-5.4-mini')).toBeVisible();
  });

  // Issue #875: the agent and tool cost tables, using the same catalogue price
  // the model and user tables already do.
  it('lists the per-agent and per-tool cost breakdowns', async () => {
    serveEstimate(PRICED);
    const { findByText, getByText } = renderTab();

    expect(await findByText('Cost by Agent')).toBeVisible();
    expect(getByText('Research Agent')).toBeVisible();
    expect(getByText('Cost by Tool')).toBeVisible();
    expect(getByText('Jira / list_issues')).toBeVisible();
  });

  // A window before shared migration 0100/0119, or a runtime that tags no
  // request, must OMIT both tables rather than render them empty — an empty
  // table would read as "no agent or tool ran" for a window nothing could
  // correlate at all, the same failure mode the model/user tables already
  // refuse (see `marks an unpriced model rather than costing it at zero`).
  it('omits the agent and tool cost tables when their dimension is unavailable', async () => {
    serveEstimate({
      ...PRICED,
      agent_dimension_available: false,
      by_agent: undefined,
      tool_dimension_available: false,
      by_tool: undefined,
    });
    const { findByText, queryByText } = renderTab();

    await findByText('Cost by Model');
    expect(queryByText('Cost by Agent')).not.toBeInTheDocument();
    expect(queryByText('Cost by Tool')).not.toBeInTheDocument();
  });

  // A model with no catalogue rate keeps its tokens and shows "not priced".
  // `$0.00` there would report an unknown as free.
  it('marks an unpriced model rather than costing it at zero', async () => {
    serveEstimate({
      ...PRICED,
      priced_calls: 3,
      unpriced_calls: 4,
      by_model: [
        PRICED.by_model[0]!,
        {
          provider: 'vllm',
          model: 'qwen3',
          calls: 4,
          prompt_tokens: 4000,
          completion_tokens: 400,
          total_tokens: 4400,
          priced: false,
        },
      ],
    });
    const { findByText, getAllByText, getByText } = renderTab();

    expect(await findByText('Cost by Model')).toBeVisible();
    expect(getByText('qwen3')).toBeVisible();
    // Three money cells plus the share cell on that row.
    expect(getAllByText('not priced').length).toBeGreaterThanOrEqual(4);
    expect(
      getByText(
        'Calls that used a model the catalogue does not price: 4. Their tokens are counted and their cost is not.',
      ),
    ).toBeVisible();
  });

  it('says nothing about money when the catalogue prices nothing in the window', async () => {
    serveEstimate({
      ...PRICED,
      cost_dimension_available: false,
      priced_calls: 0,
      unpriced_calls: 3,
      totals: { calls: 3, prompt_tokens: 2351, completion_tokens: 42, total_tokens: 2393 },
    });
    const { findByText, queryByText } = renderTab();

    expect(await findByText('Not available on this deployment')).toBeVisible();
    expect(
      await findByText(
        'No model called in this window has a price in the model catalogue. Set a price on the LLM Proxy screen to see estimated cost here.',
      ),
    ).toBeVisible();
    // No tile, no table, no zero.
    expect(queryByText('TOTAL COST')).not.toBeInTheDocument();
    expect(queryByText('$0.00')).not.toBeInTheDocument();
    expect(queryByText('Cost by Model')).not.toBeInTheDocument();
  });

  it('states that the deployment has no source when the estimate block is absent', async () => {
    serveEstimate(undefined);
    const { findByText, queryByText } = renderTab();

    expect(await findByText('Not available on this deployment')).toBeVisible();
    expect(
      await findByText(
        'This deployment carries no gateway request log, which is what a cost estimate is computed from.',
      ),
    ).toBeVisible();
    expect(queryByText('TOTAL COST')).not.toBeInTheDocument();
  });

  it('shows the load error and no tiles when the query fails', async () => {
    server.use(http.get(COSTS_URL, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { findByText, queryByText } = renderTab();

    expect(await findByText('Failed to load analytics data.')).toBeVisible();
    expect(queryByText('TOTAL COST')).not.toBeInTheDocument();
    expect(queryByText('Cost by Model')).not.toBeInTheDocument();
  });

  it('draws the daily cost chart when the window had traffic', async () => {
    serveEstimate(PRICED);
    const { findByText, container } = renderTab();

    expect(await findByText('Daily Cost Trend')).toBeVisible();
    expect(container.querySelector('.recharts-responsive-container')).not.toBeNull();
  });

  it('counts the priced calls against the window total', async () => {
    serveEstimate({ ...PRICED, priced_calls: 2, unpriced_calls: 1, totals: { ...PRICED.totals, calls: 3 } });
    const { findByText } = renderTab();

    expect((await findByText('PRICED CALLS')).parentElement).toHaveTextContent('2 / 3');
  });
});
