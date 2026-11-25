/**
 * Admin › LLM Proxy tabs in read-only mode (R3.4b, issue 2613): the providers
 * & models tab (providers, platform models and the platform default model) at
 * the read level of `providers-models`, and tool prices at the read level of
 * `tool-prices`. Each tab is its own console item, so the editor wraps each
 * one in its own section; these render the panels the same way.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminWritableSection } from './AdminWritable';
import { LlmProxyProvidersPanel } from './LlmProxyProvidersPanel';
import { LlmProxyToolPricesPanel } from './LlmProxyToolPricesPanel';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';

const PROVIDER = {
  id: 4,
  uuid: 'uuid-4',
  elitea_title: 'platform-openai',
  label: '',
  type: 'open_ai',
  status_ok: true,
  status_logs: '',
  endpoint: 'https://api.openai.com/v1',
  settings: {},
  secrets: [{ field: 'api_key', set: true, sealed: true }],
  created_at: '2026-08-01T00:00:00Z',
  updated_at: '2026-08-01T00:00:00Z',
};

const MODEL = {
  id: 11,
  uuid: 'uuid-11',
  elitea_title: 'gpt-4o',
  type: 'llm_model',
  section: 'llm',
  status_ok: true,
  status_logs: '',
  model_name: 'gpt-4o',
  credential_name: 'platform-openai',
  credential_resolves: true,
  low_tier: false,
  high_tier: false,
  created_at: '2026-08-01T00:00:00Z',
  updated_at: '2026-08-01T00:00:00Z',
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  server.use(
    http.get('*/admin/gateway/providers', () =>
      HttpResponse.json({ items: [PROVIDER], total: 1, public_project_id: 1, provider_types: ['open_ai'] }),
    ),
    http.get('*/admin/gateway/platform_models', () =>
      HttpResponse.json({
        items: [MODEL],
        total: 1,
        public_project_id: 1,
        model_types: ['llm_model'],
        credential_names: ['platform-openai'],
      }),
    ),
    http.get('*/admin/gateway/default_model', () =>
      HttpResponse.json({
        model_name: 'gpt-4o',
        model_project_id: 1,
        available: true,
        candidates: [{ name: 'gpt-4o', display_name: 'gpt-4o' }],
      }),
    ),
    http.get('*/admin/gateway/metered-prices', () =>
      HttpResponse.json({
        currency: 'USD',
        prices: [
          { provider: 'tavily', operation: 'web_search', unit: 'credit', price_per_unit_usd: '0.008', price_overridden: false },
        ],
      }),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Admin › LLM Proxy › view only', () => {
  it('disables providers, platform models and the default model at the read level', async () => {
    serveConsoleLevel('read', 'providers-models');
    renderAdminRoute(
      <AdminWritableSection itemId="providers-models">
        <LlmProxyProvidersPanel />
      </AdminWritableSection>,
    );

    await screen.findByTestId('llm-providers-table');
    await screen.findByTestId('platform-models-table');
    expect(await findViewOnlyNotice()).toHaveAttribute('data-item-id', 'providers-models');
    await waitFor(() => expect(screen.getByTestId('llm-providers-add')).toBeDisabled());
    expect(screen.getByTestId('platform-models-add')).toBeDisabled();
    expect(screen.getByTestId('admin-provider-check-4')).toBeDisabled();
    expect(screen.getByTestId('admin-provider-adopt-4')).toBeDisabled();
    expect(screen.getByTestId('platform-default-model-clear')).toBeDisabled();
    expect(enabledWriteControls()).toEqual([]);
  });

  it('keeps the providers tab writable at the write level', async () => {
    serveConsoleLevel('write', 'providers-models');
    renderAdminRoute(
      <AdminWritableSection itemId="providers-models">
        <LlmProxyProvidersPanel />
      </AdminWritableSection>,
    );

    await screen.findByTestId('llm-providers-table');
    await waitFor(() => expect(screen.getByTestId('llm-providers-add')).toBeEnabled());
    expect(screen.getByTestId('admin-provider-check-4')).toBeEnabled();
    expect(screen.queryByTestId('admin-view-only')).not.toBeInTheDocument();
  });

  it('disables the tool price fields at the read level of tool-prices', async () => {
    serveConsoleLevel('read', 'tool-prices');
    renderAdminRoute(
      <AdminWritableSection itemId="tool-prices">
        <LlmProxyToolPricesPanel />
      </AdminWritableSection>,
    );

    await screen.findByTestId('metered-price-tavily-web_search');
    await findViewOnlyNotice();
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'tavily web_search price' })).toBeDisabled());
    expect(enabledWriteControls()).toEqual([]);
  });
});
