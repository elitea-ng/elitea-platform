/**
 * The Models and spend area through the REAL admin router (ADR-0059 R3.5a,
 * issue 2535): each former tab of the LLM gateway editor has a route, and a
 * navigation item or its stated place inside one (Status under Usage, Budget
 * alerts under Budgets and alerts), and the old Configuration anchor
 * redirects to the page its tab became.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';
import { testTimeout } from '@/test/timeouts';

import { gatewayAnchorRoute } from '../configurationAnchors';
import { createAdminRouter } from '../router';
import { serveGatewayReads } from './gatewayFixtures';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const GOLDEN = join(process.cwd(), '../../services/elitea-main/internal/api/v2/adminaccess/testdata/console');

function golden(role: string): Record<string, unknown> {
  return JSON.parse(readFileSync(join(GOLDEN, `${role}.golden.json`), 'utf8')) as Record<string, unknown>;
}

function serveDocument(document: Record<string, unknown>): void {
  server.use(http.get('*/admin/console', () => HttpResponse.json(document)));
}

async function mountAdmin(path: string): Promise<ReturnType<typeof createAdminRouter>> {
  window.history.replaceState(null, '', `/admin/app${path}`);
  const router = createAdminRouter();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  await screen.findByTestId('admin-nav');
  return router;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  serveGatewayReads();
  serveDocument(golden('llm_manager'));
});

afterEach(() => {
  resetGeneratedClient();
  window.localStorage.clear();
});

/** Each former tab: its route, its page, the item active on it, and the tab strip it sits in. */
const FORMER_TABS = [
  { tab: 'Providers & models', path: '/models/providers', page: 'admin-llm-providers-page', item: 'providers-models' },
  { tab: 'Models & pricing', path: '/models/pricing', page: 'admin-llm-models-pricing-page', item: 'models-pricing' },
  { tab: 'Tools', path: '/models/tool-prices', page: 'admin-llm-tool-prices-page', item: 'tool-prices' },
  { tab: 'Usage', path: '/models/usage', page: 'admin-llm-usage-page', item: 'usage', strip: 'admin-usage-tab-usage' },
  { tab: 'Status', path: '/models/status', page: 'admin-llm-status-page', item: 'usage', strip: 'admin-usage-tab-status' },
  { tab: 'Logs', path: '/models/logs', page: 'admin-llm-logs-page', item: 'logs' },
  {
    tab: 'Budget alerts',
    path: '/budgets/alerts',
    page: 'admin-budget-alerts-page',
    item: 'budgets-alerts',
    strip: 'admin-budgets-alerts-tab-alerts',
  },
] as const;

describe('the seven gateway tabs as routes', () => {
  for (const former of FORMER_TABS) {
    it(`${former.tab} is ${former.path}, with ${former.item} active`, async () => {
      const router = await mountAdmin(former.path);
      expect(router.state.matches.map((match) => match.routeId)).toEqual(['__root__', former.path]);
      expect(await screen.findByTestId(former.page)).toBeInTheDocument();
      const item = screen.getByTestId(`admin-nav-item-${former.item}`);
      expect(item).toHaveAttribute('aria-current', 'page');
      expect(screen.getAllByRole('link', { current: 'page' })).toEqual([item]);
      if ('strip' in former) expect(screen.getByTestId(former.strip)).toHaveAttribute('aria-selected', 'true');
    }, testTimeout(30_000));
  }

  it('lists the seven items of decision 6 in Models and spend, in order', async () => {
    await mountAdmin('/models/providers');
    const area = await screen.findByTestId('admin-nav-area-models_spend');
    expect(
      within(area)
        .getAllByTestId(/^admin-nav-item-/)
        .map((link) => link.textContent),
    ).toEqual([
      'Providers and models',
      'Models and pricing',
      'Tool prices',
      'Usage',
      'Logs',
      'LLM Governance',
      'Budgets and alerts',
    ]);
    // Configuration no longer stands for the gateway tabs: the LLM manager has none of its sections.
    expect(screen.queryByTestId('admin-nav-item-configuration')).toBeNull();
  }, testTimeout(30_000));

  it('reaches Status from Usage through the strip, with Usage still active', async () => {
    const user = userEvent.setup();
    await mountAdmin('/models/usage');
    await user.click(await screen.findByTestId('admin-usage-tab-status'));
    await vi.waitFor(() => expect(window.location.pathname).toBe('/admin/app/models/status'));
    expect(await screen.findByTestId('admin-llm-status-page')).toBeInTheDocument();
    expect(screen.getByTestId('admin-nav-item-usage')).toHaveAttribute('aria-current', 'page');
  }, testTimeout(30_000));
});

describe('the old Configuration anchors', () => {
  it('maps the section, and each tab of it, to its page', () => {
    expect(gatewayAnchorRoute('#llm_proxy')).toBe('/models/providers');
    expect(gatewayAnchorRoute('llm_proxy')).toBe('/models/providers');
    expect(gatewayAnchorRoute('#llm_proxy/usage')).toBe('/models/usage');
    expect(gatewayAnchorRoute('#llm_proxy:alerts')).toBe('/budgets/alerts');
    expect(gatewayAnchorRoute('#llm_proxy/status')).toBe('/models/status');
    expect(gatewayAnchorRoute('#llm_proxy/models')).toBe('/models/pricing');
    expect(gatewayAnchorRoute('#llm_proxy/tools')).toBe('/models/tool-prices');
    expect(gatewayAnchorRoute('#llm_proxy/logs')).toBe('/models/logs');
    expect(gatewayAnchorRoute('#llm_proxy/unknown')).toBe('/models/providers');
    expect(gatewayAnchorRoute('#banner')).toBeUndefined();
    expect(gatewayAnchorRoute('')).toBeUndefined();
  });

  it('redirects /configuration#llm_proxy/usage to the Usage page', async () => {
    await mountAdmin('/configuration#llm_proxy/usage');
    await vi.waitFor(() => expect(window.location.pathname).toBe('/admin/app/models/usage'));
    expect(await screen.findByTestId('admin-llm-usage-page')).toBeInTheDocument();
  }, testTimeout(30_000));

  it('redirects /configuration#llm_proxy to Providers and models', async () => {
    await mountAdmin('/configuration#llm_proxy');
    await vi.waitFor(() => expect(window.location.pathname).toBe('/admin/app/models/providers'));
    expect(await screen.findByTestId('admin-llm-providers-page')).toBeInTheDocument();
  }, testTimeout(30_000));

  it('keeps the editor in Configuration while the six areas are off', async () => {
    serveDocument({ ...golden('super_admin'), admin_console_areas: false });
    server.use(
      http.get('*/admin/plugin_config_schemas/administration', () =>
        HttpResponse.json({
          sections: [
            {
              id: 'llm_proxy',
              title: 'LLM Proxy',
              managed_surface: 'llm_proxy',
              unavailable_reason: 'managed elsewhere',
              fields: [],
            },
          ],
        }),
      ),
    );
    await mountAdmin('/configuration#llm_proxy');
    expect(await screen.findByTestId('llm-proxy-tabs')).toBeInTheDocument();
    expect(window.location.pathname).toBe('/admin/app/configuration');
  }, testTimeout(30_000));

  it('turns the Configuration row into a link to the pages under the six areas', async () => {
    serveDocument(golden('super_admin'));
    server.use(
      http.get('*/admin/plugin_config_schemas/administration', () =>
        HttpResponse.json({
          sections: [
            {
              id: 'llm_proxy',
              title: 'LLM Proxy',
              managed_surface: 'llm_proxy',
              unavailable_reason: 'managed elsewhere',
              fields: [],
            },
          ],
        }),
      ),
    );
    await mountAdmin('/configuration');
    const moved = await screen.findByTestId('admin-configuration-moved');
    expect(moved).toHaveTextContent('Providers and models');
    expect(screen.queryByTestId('llm-proxy-tabs')).toBeNull();
    const sections = screen.getByRole('navigation', { name: 'Configuration sections' });
    expect(within(sections).getByRole('link', { name: /LLM Proxy/ })).toHaveAttribute(
      'href',
      '/admin/app/models/providers',
    );
  }, testTimeout(30_000));
});
