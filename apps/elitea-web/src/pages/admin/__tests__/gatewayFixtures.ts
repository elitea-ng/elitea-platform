/**
 * One answer per read of the Models and spend pages (ADR-0059 R3.5a, issue
 * 2535), in the shapes the panels' own tests use. The gateway routes are
 * passthrough routes with no schema in `v2.yaml`, so there is no contract
 * example to build them from; the console document the pages read their
 * level from is the contract's (`serveConsoleLevel`, the golden documents).
 */
import { HttpResponse, http } from 'msw';

import { server } from '@/test/setup';

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

const PLATFORM_MODEL = {
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

const STATUS = {
  reachable: true,
  gateway: {
    enabled: true,
    rate_limits_enforceable: false,
    store: { has_database: true, last_success: '2026-08-23T10:00:00Z', refresh_interval: '30s' },
    definitions: { loaded_at: '2026-08-23T10:00:00Z', rows: 3, rejected: [], inert: [] },
    rate_limiter: { refused: 0, degraded: 0 },
  },
};

const CATALOGUE = {
  window: '24h',
  total: 1,
  items: [
    {
      id: 'model-1',
      provider: 'openai',
      model_name: 'gpt-5',
      input_cost_per_1m_tokens: 1.25,
      output_cost_per_1m_tokens: 10,
      input_cost_per_1m_seconds: null,
      output_cost_per_1m_seconds: null,
      input_cost_per_1m_characters: null,
      output_cost_per_1m_characters: null,
      source: 'litellm',
      price_overridden: false,
      requests: 40,
      total_tokens: 8000,
      cost_usd: 0.5,
    },
  ],
  unpriced: [],
};

const TOTALS = {
  requests: 12,
  prompt_tokens: 900,
  completion_tokens: 100,
  total_tokens: 1000,
  cost_usd: 1.5,
  models: 1,
  projects: 1,
};

const USAGE = {
  window: '24h',
  totals: TOTALS,
  daily: [],
  models: [],
  projects: [],
  members: [],
  models_truncated: false,
  projects_truncated: false,
  members_truncated: false,
  retention_days: 400,
};

const LOGS = {
  items: [],
  window: '24h',
  summary: { requests: 0, failed: 0, median_ms: 0, p95_ms: 0 },
  retention_days: 30,
};

/** Serves every read of the seven pages. */
export function serveGatewayReads(): void {
  server.use(
    http.get('*/admin/gateway/providers', () =>
      HttpResponse.json({ items: [PROVIDER], total: 1, public_project_id: 1, provider_types: ['open_ai'] }),
    ),
    http.get('*/admin/gateway/platform_models', () =>
      HttpResponse.json({
        items: [PLATFORM_MODEL],
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
    http.get('*/admin/gateway/status', () => HttpResponse.json(STATUS)),
    http.get('*/admin/gateway/models', () => HttpResponse.json(CATALOGUE)),
    http.get('*/admin/gateway/usage', () => HttpResponse.json(USAGE)),
    http.get('*/admin/gateway/logs', () => HttpResponse.json(LOGS)),
    http.get('*/admin/gateway/budget-alerts', () => HttpResponse.json({ enabled: true, threshold_pct: 80 })),
  );
}
