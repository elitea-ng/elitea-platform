/**
 * The Models and spend pages (ADR-0059 R3.5a, issue 2535) at `read` and at
 * `write` of their own console items. Each page mounts the panel its
 * gateway tab mounted, unchanged; a page that writes reads its item's
 * level, as its tab did (R3.4b, issue 2613).
 */
import type { ComponentType } from 'react';

import { screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import {
  AdminBudgetAlerts,
  AdminLlmLogs,
  AdminLlmModelsPricing,
  AdminLlmProviders,
  AdminLlmStatus,
  AdminLlmToolPrices,
  AdminLlmUsage,
} from './ModelsSpendPages';
import { serveGatewayReads } from './__tests__/gatewayFixtures';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';

interface PageCase {
  readonly name: string;
  readonly Page: ComponentType;
  readonly testId: string;
  readonly heading: string;
  /** A landmark only the loaded panel renders. */
  readonly landmark: string;
  /** The console item whose level the page's writes read; absent for a page that only reads. */
  readonly itemId?: string;
}

const PAGES: readonly PageCase[] = [
  {
    name: 'Providers and models',
    Page: AdminLlmProviders,
    testId: 'admin-llm-providers-page',
    heading: 'Providers and models',
    landmark: 'llm-providers-table',
    itemId: 'providers-models',
  },
  {
    name: 'Models and pricing',
    Page: AdminLlmModelsPricing,
    testId: 'admin-llm-models-pricing-page',
    heading: 'Models and pricing',
    landmark: 'llm-proxy-models-table',
    itemId: 'models-pricing',
  },
  {
    name: 'Tool prices',
    Page: AdminLlmToolPrices,
    testId: 'admin-llm-tool-prices-page',
    heading: 'Tool prices',
    landmark: 'metered-price-tavily-web_search',
    itemId: 'tool-prices',
  },
  {
    name: 'Usage',
    Page: AdminLlmUsage,
    testId: 'admin-llm-usage-page',
    heading: 'Usage',
    landmark: 'llm-proxy-usage-totals',
  },
  {
    name: 'Gateway status',
    Page: AdminLlmStatus,
    testId: 'admin-llm-status-page',
    heading: 'Gateway status',
    landmark: 'llm-proxy-status-ratelimits',
  },
  { name: 'Logs', Page: AdminLlmLogs, testId: 'admin-llm-logs-page', heading: 'Logs', landmark: 'llm-logs-empty' },
  {
    name: 'Budget alerts',
    Page: AdminBudgetAlerts,
    testId: 'admin-budget-alerts-page',
    heading: 'Budget alerts',
    landmark: 'llm-proxy-alerts-save',
    itemId: 'budgets-alerts',
  },
];

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  serveGatewayReads();
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Models and spend pages', () => {
  for (const page of PAGES) {
    const item = page.itemId ?? 'usage';

    it(`${page.name}: renders its panel at write, with no view-only notice`, async () => {
      serveConsoleLevel('write', item);
      renderAdminRoute(<page.Page />);
      expect(await screen.findByTestId(page.testId)).toBeInTheDocument();
      expect(screen.getByRole('heading', { level: 1, name: page.heading })).toBeInTheDocument();
      expect(await screen.findByTestId(page.landmark)).toBeInTheDocument();
      expect(screen.queryByTestId('admin-view-only')).not.toBeInTheDocument();
    });

    it(`${page.name}: renders its panel at read${page.itemId === undefined ? '' : ', every write disabled'}`, async () => {
      serveConsoleLevel('read', item);
      renderAdminRoute(<page.Page />);
      expect(await screen.findByTestId(page.landmark)).toBeInTheDocument();
      if (page.itemId === undefined) {
        // A page with no write has no notice: there is nothing it could not do.
        expect(screen.queryByTestId('admin-view-only')).not.toBeInTheDocument();
        return;
      }
      expect(await findViewOnlyNotice()).toHaveAttribute('data-item-id', page.itemId);
      await waitFor(() => expect(enabledWriteControls()).toEqual([]));
    });
  }
});
