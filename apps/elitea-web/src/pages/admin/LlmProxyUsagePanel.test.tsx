/**
 * The LLM Proxy Usage tab.
 *
 * The report itself is arithmetic and needs no defending. What is asserted here
 * is the part that can be wrong while looking right:
 *
 *  1. **A section that FAILED never renders as a section with nothing in it.**
 *     "No spend" is the reassuring reading of an empty table, and it is the one
 *     an operator would act on. The server answers with a per-section error for
 *     that reason; this proves the panel shows each one where its table would
 *     have been, and that the three working tables are still rendered beside it.
 *  2. **Sub-cent spend is not rounded to the same string as zero.** An
 *     uncatalogued audio model bills zero and no budget can stop it, so "$0.0000"
 *     and "billed nothing" must not be the same cell.
 *  3. **A partial body does not take the panel down with it.** The totals are
 *     formatted with `toFixed`, which throws on undefined, and the crash would
 *     unmount the very alert carrying the server's explanation — so the last
 *     case below sends a body with an empty `totals` object on purpose.
 *  4. **The member breakdown's omission is disclosed.** Calls with no resolvable
 *     member are counted in the totals and in the project breakdown but under no
 *     member, so the two do not sum — and a reader who is not told that will
 *     conclude the numbers are wrong, or worse, will not notice.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { LlmProxyUsagePanel } from './LlmProxyUsagePanel';
import { renderAdminRoute } from './__tests__/testRouter';

const FULL_REPORT = {
  window: '24h',
  totals: {
    requests: 1234,
    prompt_tokens: 900_000,
    completion_tokens: 100_000,
    total_tokens: 1_000_000,
    cost_usd: 12.5,
    models: 6,
    projects: 3,
  },
  daily: [{ day: '2026-08-23', requests: 1234, total_tokens: 1_000_000, cost_usd: 12.5 }],
  models: [
    {
      key: 'openai/gpt-4o',
      label: 'gpt-4o',
      detail: 'openai',
      requests: 1000,
      prompt_tokens: 800_000,
      completion_tokens: 90_000,
      total_tokens: 890_000,
      cost_usd: 12.4,
    },
  ],
  projects: [
    {
      key: 'project:7',
      label: 'Acme',
      detail: '7',
      requests: 1200,
      prompt_tokens: 880_000,
      completion_tokens: 98_000,
      total_tokens: 978_000,
      cost_usd: 12.3,
    },
  ],
  members: [
    {
      key: 'member:3',
      label: 'ops@example.com',
      detail: '3',
      requests: 900,
      prompt_tokens: 700_000,
      completion_tokens: 80_000,
      total_tokens: 780_000,
      cost_usd: 9.1,
    },
  ],
  models_truncated: false,
  projects_truncated: false,
  members_truncated: false,
  retention_days: 400,
};

function useUsage(body: Record<string, unknown>): void {
  server.use(http.get('*/admin/gateway/usage', () => HttpResponse.json(body)));
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('LlmProxyUsagePanel', () => {
  it('reports the window totals and every breakdown', async () => {
    useUsage(FULL_REPORT);
    renderAdminRoute(<LlmProxyUsagePanel />);

    expect(await screen.findByTestId('llm-proxy-usage-totals')).toHaveTextContent('$12.5000');
    // The prompt/completion split is a hint on the token tile rather than two
    // more tiles — it is read as a ratio, not as figures in their own right.
    expect(screen.getByTestId('llm-proxy-usage-totals')).toHaveTextContent('1,000,000');

    expect(await screen.findByTestId('llm-proxy-usage-daily')).toHaveTextContent('2026-08-23');
    expect(screen.getByTestId('llm-proxy-usage-models')).toHaveTextContent('gpt-4o');
    expect(screen.getByTestId('llm-proxy-usage-projects')).toHaveTextContent('Acme');
    expect(screen.getByTestId('llm-proxy-usage-members')).toHaveTextContent('ops@example.com');
  });

  // The core assertion. One breakdown failing must not silence the others, and
  // must not render as "nothing was spent under this heading".
  it('shows a failed section as a failure, and keeps the sections that worked', async () => {
    useUsage({
      ...FULL_REPORT,
      projects: [],
      projects_error: 'read the usage breakdown: query timeout',
    });
    renderAdminRoute(<LlmProxyUsagePanel />);

    const failure = await screen.findByTestId('llm-proxy-usage-projects-error');
    expect(failure).toHaveTextContent('query timeout');
    // The empty state is NOT also shown: it would say "no billed requests",
    // which is the false claim the error exists to prevent.
    expect(screen.queryByTestId('llm-proxy-usage-projects-empty')).toBeNull();
    // …and the sections that loaded are still there.
    expect(screen.getByTestId('llm-proxy-usage-models')).toHaveTextContent('gpt-4o');
  });

  it('says a section is genuinely empty when it is', async () => {
    useUsage({ ...FULL_REPORT, members: [] });
    renderAdminRoute(<LlmProxyUsagePanel />);

    expect(await screen.findByTestId('llm-proxy-usage-members-empty')).toBeVisible();
    expect(screen.queryByTestId('llm-proxy-usage-members-error')).toBeNull();
  });

  // A sub-cent spend and a true zero must not render as the same string: only
  // one of them means "no budget can stop this".
  it('separates a sub-cent spend from a zero one', async () => {
    useUsage({
      ...FULL_REPORT,
      models: [
        { ...FULL_REPORT.models[0], key: 'a', label: 'tiny', cost_usd: 0.00002 },
        { ...FULL_REPORT.models[0], key: 'b', label: 'free', cost_usd: 0 },
      ],
    });
    renderAdminRoute(<LlmProxyUsagePanel />);

    const table = await screen.findByTestId('llm-proxy-usage-models');
    expect(table).toHaveTextContent('<$0.0001');
    expect(table).toHaveTextContent('$0.0000');
  });

  // #1238: a metered tool row reuses the model columns (vendor as provider,
  // operation as model) and has no tokens, so it is labelled as the tool and
  // shows its billed units.
  it('labels a tool row and shows its units instead of tokens', async () => {
    useUsage({
      ...FULL_REPORT,
      models: [
        { ...FULL_REPORT.models[0], operation: 'llm' },
        {
          key: 'tavily/web_search#tool',
          label: 'web_search',
          detail: 'tavily',
          requests: 1,
          prompt_tokens: 0,
          completion_tokens: 0,
          total_tokens: 0,
          cost_usd: 0.008,
          operation: 'tool',
          units: 3,
          unit: 'credit',
        },
      ],
    });
    renderAdminRoute(<LlmProxyUsagePanel />);

    const table = await screen.findByTestId('llm-proxy-usage-models');
    expect(table).toHaveTextContent('Web search · Tavily');
    expect(table).toHaveTextContent('3 credits');
    expect(table).toHaveTextContent('gpt-4o');
  });

  it('discloses that member-less calls are outside the member breakdown', async () => {
    useUsage(FULL_REPORT);
    renderAdminRoute(<LlmProxyUsagePanel />);

    expect(
      await screen.findByText(/not under any member/, { exact: false }),
    ).toBeVisible();
  });

  it('refetches on a window change rather than filtering what it already has', async () => {
    const windows: string[] = [];
    server.use(
      http.get('*/admin/gateway/usage', ({ request }) => {
        windows.push(new URL(request.url).searchParams.get('window') ?? '');
        return HttpResponse.json(FULL_REPORT);
      }),
    );
    renderAdminRoute(<LlmProxyUsagePanel />);
    await screen.findByTestId('llm-proxy-usage-totals');

    await userEvent.click(screen.getByRole('combobox'));
    await userEvent.click(await screen.findByRole('option', { name: '7d' }));

    // The ledger is aggregated server-side; a client that narrowed a 24h payload
    // to produce a 7d figure would be reporting a subset of the wrong window.
    await screen.findByTestId('llm-proxy-usage-totals');
    expect(windows).toContain('7d');
  });

  it('shows the server refusal that replaces the whole report', async () => {
    useUsage({
      window: '24h',
      totals: {},
      daily: [],
      models: [],
      projects: [],
      members: [],
      retention_days: 400,
      error: 'this deployment has no database pool, so usage cannot be read.',
    });
    renderAdminRoute(<LlmProxyUsagePanel />);

    expect(await screen.findByTestId('llm-proxy-usage-error')).toHaveTextContent(
      'no database pool',
    );
  });
});
