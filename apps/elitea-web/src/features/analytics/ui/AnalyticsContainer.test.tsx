import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { RenderResult } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';
import { AnalyticsContainer } from './AnalyticsContainer';

const BASE = '/api/v2';
const USAGE_URL = `${BASE}/elitea_core/analytics/prompt_lib/7`;

function renderScreen(ui: ReactElement): RenderResult {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

// `llm_calls` deliberately renders (via `fmtNum`) as "4.3K", not a raw
// 1-2 digit number — the real `AnalyticsContainer` embeds a REAL, live
// `DateTimePicker` showing the actual current wall-clock date/time, whose
// day/hour/minute spinbutton sections are also plain 1-2 digit text nodes
// in the same document; a small KPI number risks colliding with whatever
// the clock happens to read at the moment the test runs (a real flake
// this suite hit once: `llm_calls: 42` collided with a "42" minutes
// value). A four-digit, K-formatted value can't collide with any
// day (≤31), hour (≤23), minute/second (≤59), or month field.
function usageResponse(overrides: Partial<Record<string, unknown>> = {}): Record<string, unknown> {
  return {
    kpis: {
      total_project_users: 10,
      ai_active_users: 3,
      adoption_rate: 30,
      llm_calls: 4321,
      total_tokens: 8765,
    },
    top_ai_users: [],
    daily_activity: [],
    models: [],
    models_truncated: false,
    ...overrides,
  };
}

const COSTS_URL = `${BASE}/elitea_core/analytics_costs/prompt_lib/7`;

/**
 * The COST tile's text, formatted the way the component formats it.
 *
 * NOT a hardcoded "$12.50". `AnalyticsContainer`'s `currencyFormat` is
 * `Intl.NumberFormat(undefined, {style: 'currency', currency: 'USD'})` — the
 * FIRST argument is `undefined`, so the currency symbol is whatever the host's
 * default locale renders USD as: "$12.50" under `en-US`, "US$12.50" under
 * `en-CA`/`en-GB`, "12,50 $US" under `fr-FR`. The literal passed in CI (which
 * runs `en-US`) and failed on any developer machine whose `LANG` is not, with
 * the same DOM showing a `17/09/2026` date picker beside it as the tell.
 *
 * Re-deriving it here keeps the assertion exact — wrong digits, a wrong
 * currency or an unformatted number all still fail — without asserting the
 * host's locale, which this test is not about. The formatter is duplicated
 * rather than exported: exporting it would make a `knip`-visible symbol whose
 * only consumer is this line, and the contract being pinned is "the same
 * Intl options", not "the same object".
 */
function expectedUsd(amount: number): string {
  return new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 2 }).format(amount);
}

/**
 * A cost breakdown. `spendAvailable` is the point: /analytics_costs ALWAYS
 * emits `total_cost` — `0.00000000` when the write-back path has persisted
 * nothing — and publishes `spend_available` so that "no spend yet" stays
 * distinguishable from "no data".
 */
function costsResponse(totalCost: number, spendAvailable: boolean): Record<string, unknown> {
  return {
    kpis: {
      total_cost: totalCost,
      currency: 'USD',
      periods: spendAvailable ? 1 : 0,
      spend_available: spendAvailable,
      window_days: 1,
    },
    periods: [],
    by_scope: [],
    periods_truncated: false,
    date_from: '2026-08-01T00:00:00Z',
    date_to: '2026-08-02T00:00:00Z',
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AnalyticsContainer', () => {
  describe('the cost tile', () => {
    it('renders the spend when the write-back path has persisted some', async () => {
      server.use(
        http.get(USAGE_URL, () => HttpResponse.json(usageResponse())),
        http.get(COSTS_URL, () => HttpResponse.json(costsResponse(12.5, true))),
      );
      const { findByText } = renderScreen(<AnalyticsContainer projectId="7" />);
      expect(await findByText('COST')).toBeInTheDocument();
      expect(await findByText(expectedUsd(12.5))).toBeInTheDocument();
    });

    /**
     * The fabricated-zero claim, one tile wide. `total_cost` is present and
     * `0` for a project whose spend has never been measured, so reading the
     * number alone renders "COST / $0.00 / billed spend" — a measurement,
     * where the KPI row beside it goes to some length to omit a tile rather
     * than print a zero for a figure nothing produced.
     */
    it('omits the tile entirely when no spend has been measured', async () => {
      server.use(
        http.get(USAGE_URL, () => HttpResponse.json(usageResponse())),
        http.get(COSTS_URL, () => HttpResponse.json(costsResponse(0, false))),
      );
      const { findByText, queryByText } = renderScreen(<AnalyticsContainer projectId="7" />);
      // Wait for the row to paint before asserting an absence.
      await findByText('LLM CALLS');
      expect(queryByText('COST')).not.toBeInTheDocument();
      // Same reason as `expectedUsd`'s own comment: a hardcoded "$0.00" is
      // absent under a non-`en-US` default locale whatever the tile renders,
      // so the assertion would pass vacuously there.
      expect(queryByText(expectedUsd(0))).not.toBeInTheDocument();
    });

    /**
     * The money read is one tile of a dashboard the rest of which is fine.
     * Taking the whole Overview down because it failed would trade a missing
     * tile for a blank screen.
     */
    it('leaves the rest of the overview alone when the cost read fails', async () => {
      server.use(
        http.get(USAGE_URL, () => HttpResponse.json(usageResponse())),
        http.get(COSTS_URL, () => HttpResponse.json({ error: 'boom' }, { status: 500 })),
      );
      const { findByText, queryByText } = renderScreen(<AnalyticsContainer projectId="7" />);
      expect(await findByText('4.3K')).toBeInTheDocument();
      expect(queryByText('Failed to load analytics data.')).not.toBeInTheDocument();
      expect(queryByText('COST')).not.toBeInTheDocument();
    });
  });

  it('renders the header title', () => {
    server.use(http.get(USAGE_URL, () => HttpResponse.json(usageResponse())));
    const { getByText } = renderScreen(<AnalyticsContainer projectId="7" />);
    expect(getByText('Analytics')).toBeInTheDocument();
  });

  it('renders the project label when projectName is provided', () => {
    server.use(http.get(USAGE_URL, () => HttpResponse.json(usageResponse())));
    const { getByText } = renderScreen(
      <AnalyticsContainer
        projectId="7"
        projectName="Demo Project"
      />,
    );
    expect(getByText('Project: Demo Project')).toBeInTheDocument();
  });

  it('omits the project label when projectName is not provided', () => {
    server.use(http.get(USAGE_URL, () => HttpResponse.json(usageResponse())));
    const { queryByText } = renderScreen(<AnalyticsContainer projectId="7" />);
    expect(queryByText(/^Project:/)).not.toBeInTheDocument();
  });

  it('fetches and renders Overview KPIs by default', async () => {
    server.use(http.get(USAGE_URL, () => HttpResponse.json(usageResponse())));
    const { findByText } = renderScreen(<AnalyticsContainer projectId="7" />);
    expect(await findByText('4.3K')).toBeInTheDocument();
  });

  it('shows the load-error message when the usage query fails', async () => {
    server.use(http.get(USAGE_URL, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { findByText } = renderScreen(<AnalyticsContainer projectId="7" />);
    expect(await findByText('Failed to load analytics data.')).toBeInTheDocument();
  });

  it('switches to the Agents tab and renders its content', async () => {
    server.use(
      http.get(USAGE_URL, () => HttpResponse.json(usageResponse())),
      http.get(`${BASE}/elitea_core/analytics_agents/prompt_lib/7`, () => HttpResponse.json({ items: [] })),
    );
    const user = userEvent.setup();
    const { getByRole, findByText } = renderScreen(<AnalyticsContainer projectId="7" />);
    await user.click(getByRole('tab', { name: 'Agents' }));
    expect(await findByText('Agent Activity')).toBeInTheDocument();
  });

  it('switches to the Guide tab (no data fetch needed)', async () => {
    server.use(http.get(USAGE_URL, () => HttpResponse.json(usageResponse())));
    const user = userEvent.setup();
    const { getByRole, findByText } = renderScreen(<AnalyticsContainer projectId="7" />);
    await user.click(getByRole('tab', { name: 'Guide' }));
    expect(await findByText('Overview Tab')).toBeInTheDocument();
  });

  it('switches to the Health tab and renders once usage data resolves', async () => {
    server.use(http.get(USAGE_URL, () => HttpResponse.json(usageResponse())));
    const user = userEvent.setup();
    const { getByRole, findByText } = renderScreen(<AnalyticsContainer projectId="7" />);
    await user.click(getByRole('tab', { name: 'Health' }));
    expect(await findByText('No health data available.')).toBeInTheDocument();
  });

  it('navigates from the Overview leaderboard to the Users tab with the clicked user preselected', async () => {
    server.use(
      http.get(USAGE_URL, () =>
        HttpResponse.json(
          usageResponse({
            top_ai_users: [
              {
                user_id: 'u1',
                email: 'alice@example.com',
                run_count: 3,
                total_tokens: 12,
                last_active_at: '2026-08-20T10:00:00Z',
              },
            ],
          }),
        ),
      ),
      http.get(`${BASE}/elitea_core/analytics_users/prompt_lib/7`, () => HttpResponse.json({ items: [] })),
      http.get(`${BASE}/elitea_core/analytics_user_detail/prompt_lib/7`, ({ request }) => {
        expect(new URL(request.url).searchParams.get('user_id')).toBe('u1');
        // The detail branch answers WITHOUT a kpis block — the per-entity
        // split it would need is the one dimension the request log does not
        // carry — so the fixture omits it rather than echoing the usage one.
        return HttpResponse.json({ entity_name: 'alice@example.com', agents: [], tools: [], daily_usage: [] });
      }),
    );
    const user = userEvent.setup();
    const { findByText, getByRole } = renderScreen(<AnalyticsContainer projectId="7" />);
    await user.click(await findByText('alice@example.com'));
    // Landed directly on the User Detail screen (Users tab is now active,
    // and the clicked user is preselected via `pendingUserId`).
    expect(getByRole('tab', { name: 'Users', selected: true })).toBeInTheDocument();
    expect(await findByText('alice@example.com')).toBeInTheDocument();
  });

  it('changes the date-range preset selection', async () => {
    server.use(http.get(USAGE_URL, () => HttpResponse.json(usageResponse())));
    const user = userEvent.setup();
    const { getByRole } = renderScreen(<AnalyticsContainer projectId="7" />);
    const sevenDayButton = getByRole('button', { name: 'Last 7d' });
    await user.click(sevenDayButton);
    expect(sevenDayButton).toHaveAttribute('aria-pressed', 'true');
  });

  it('does not fetch while projectId is undefined (a genuine Wave-1 gap — see this component\'s header)', () => {
    let hit = false;
    server.use(
      http.get(`${BASE}/elitea_core/analytics/prompt_lib/*`, () => {
        hit = true;
        return HttpResponse.json(usageResponse());
      }),
    );
    renderScreen(<AnalyticsContainer projectId={undefined} />);
    expect(hit).toBe(false);
  });
});
