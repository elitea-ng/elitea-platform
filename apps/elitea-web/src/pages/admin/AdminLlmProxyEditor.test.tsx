/**
 * Rendering + write-path guard for the LLM Proxy section editor.
 *
 * The properties asserted here are the ones this screen's hazards make worth
 * asserting, and none of them is visible to a status-code test:
 *
 *  1. **An unreachable gateway is never rendered as a healthy one.** The status
 *     route answers 200 with `reachable: false`, precisely so the screen can
 *     tell "the hop is down" from "my request failed" — which means a careless
 *     client would render a down gateway's empty counters as a live report.
 *  2. **A rejected or inert row is shown with the gateway's own reason.** These
 *     are the two states the governance page cannot show at all, and the reason
 *     string is the only thing that says what to change.
 *  3. **The unpriced list is surfaced, not buried, and says what it really
 *     costs.** A token model missing from the catalogue is billed at a rate the
 *     gateway invents — not zero — so the recorded spend is wrong rather than
 *     absent; only audio is genuinely unbilled. Telling an operator "billed at
 *     zero" would send them hunting for missing spend that is present and wrong.
 *  4. **The write body carries only identity and prices.** The server refuses
 *     unknown fields, so echoing a read row back would 400 — and a client that
 *     sent usage counters would be asserting values it does not own.
 *  5. **A save that prices nothing cannot be submitted**, because it would mark
 *     the row overridden — excluding it from the price sync permanently — while
 *     leaving it billed at the gateway's invented fallback rate for good.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminLlmProxyEditor } from './AdminLlmProxyEditor';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';

const REACHABLE_STATUS = {
  reachable: true,
  gateway: {
    enabled: true,
    rate_limits_enforceable: false,
    store: { has_database: true, last_success: '2026-08-23T10:00:00Z', refresh_interval: '30s' },
    definitions: {
      loaded_at: '2026-08-23T10:00:00Z',
      rows: 3,
      rejected: [
        {
          id: 'r1',
          type: 'routing_rule',
          name: 'cheap-first',
          reason: 'CEL compile error: undeclared reference to tokens_used',
        },
      ],
      inert: [
        { id: 'i1', type: 'budget', name: 'orphan-budget', reason: 'scope matches no project' },
      ],
    },
    rate_limiter: { refused: 4, degraded: 2 },
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
  unpriced: [
    {
      provider: 'anthropic',
      model_name: 'claude-opus-5',
      requests: 9,
      total_tokens: 500,
      cost_usd: 0,
    },
  ],
};

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function useGatewayHandlers(status: object = REACHABLE_STATUS): void {
  server.use(
    http.get('*/admin/gateway/status', () => HttpResponse.json(status)),
    http.get('*/admin/gateway/models', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json(CATALOGUE);
    }),
    http.put('*/admin/gateway/models', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json({ id: 'model-1', price_overridden: true });
    }),
    http.delete('*/admin/gateway/models/*', ({ request }) => {
      recorded.push({ method: 'DELETE', url: request.url, body: null });
      return HttpResponse.json({ id: 'model-1', price_overridden: false });
    }),
  );
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method !== 'GET');
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useGatewayHandlers();
});

afterEach(() => {
  resetGeneratedClient();
});

async function openModelsTab(): Promise<void> {
  const user = userEvent.setup();
  await user.click(await screen.findByRole('tab', { name: 'Models & pricing' }));
}

describe('Admin › LLM Proxy › Status', () => {
  it('names a rejected rule with the gateway’s own reason', async () => {
    renderAdminRoute(<AdminLlmProxyEditor />);

    expect(await screen.findByText('cheap-first')).toBeInTheDocument();
    // The reason, verbatim. It is the only text on any screen that says why the
    // rule an operator authored is enforcing nothing.
    expect(
      screen.getByText(/CEL compile error: undeclared reference to tokens_used/),
    ).toBeInTheDocument();
  });

  it('reports an inert row separately from a rejected one', async () => {
    renderAdminRoute(<AdminLlmProxyEditor />);

    // Well-formed and unenforceable is a different conversation from malformed,
    // so the two are never merged into one list.
    expect(await screen.findByText('orphan-budget')).toBeInTheDocument();
    expect(screen.getByTestId('llm-proxy-rejected-table')).toBeInTheDocument();
    expect(screen.getByTestId('llm-proxy-inert-table')).toBeInTheDocument();
  });

  it('warns that rate limits are unenforceable, which nothing else reports', async () => {
    renderAdminRoute(<AdminLlmProxyEditor />);

    expect(await screen.findByTestId('llm-proxy-status-ratelimits')).toBeInTheDocument();
    // Requests already admitted without their ceiling — an error, not a note.
    expect(screen.getByTestId('llm-proxy-status-degraded')).toBeInTheDocument();
  });

  it('renders an unreachable gateway as unreachable, not as an empty healthy one', async () => {
    recorded = [];
    useGatewayHandlers({ reachable: false, error: 'dial tcp 10.0.0.5:8443: connection refused' });

    renderAdminRoute(<AdminLlmProxyEditor />);

    expect(await screen.findByTestId('llm-proxy-status-unreachable')).toBeInTheDocument();
    // The transport's own sentence: "not configured" and "connection refused"
    // send an operator to different places.
    expect(screen.getByText(/connection refused/)).toBeInTheDocument();
    // No counters at all — a zero next to "Definitions loaded" would read as
    // "nothing is authored" when the truth is "nothing was asked".
    expect(screen.queryByText('Definitions loaded')).not.toBeInTheDocument();
  });
});

describe('Admin › LLM Proxy › Models', () => {
  it('surfaces a called-but-unpriced model as an error', async () => {
    renderAdminRoute(<AdminLlmProxyEditor />);
    await openModelsTab();

    const unpriced = await screen.findByTestId('llm-proxy-unpriced');
    expect(unpriced).toHaveTextContent('anthropic / claude-opus-5');
    // The consequence is stated, because "unpriced" alone reads as cosmetic —
    // and it is stated ACCURATELY. A token model here is billed at an invented
    // fallback rate, so its spend is wrong, not missing; only audio is unbilled.
    expect(unpriced).toHaveTextContent(/fallback rate/i);
    expect(unpriced).not.toHaveTextContent(/These requests are billed at zero/i);
  });

  it('lists a catalogued model with its price and observed usage', async () => {
    renderAdminRoute(<AdminLlmProxyEditor />);
    await openModelsTab();

    expect(await screen.findByTestId('llm-proxy-models-table')).toBeInTheDocument();
    expect(screen.getByText('gpt-5')).toBeInTheDocument();
    expect(screen.getByText('litellm')).toBeInTheDocument();
  });

  it('sends only identity and prices when saving an override', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminLlmProxyEditor />);
    await openModelsTab();

    await user.click(await screen.findByRole('button', { name: 'Edit price' }));
    const input = await screen.findByTestId('llm-proxy-price-input_cost_per_1m_tokens');
    await user.clear(input);
    await user.type(input, '2.5');
    await user.click(screen.getByTestId('llm-proxy-price-save'));

    await waitFor(() => expect(writes()).toHaveLength(1));
    const body = writes()[0]?.body as Record<string, unknown>;

    expect(body['provider']).toBe('openai');
    expect(body['model_name']).toBe('gpt-5');
    expect(body['input_cost_per_1m_tokens']).toBe(2.5);
    // The server refuses unknown fields. Echoing the read row back — its id,
    // usage counters, provenance — would be a 400, and would also be this
    // client asserting values it does not own.
    for (const forbidden of ['id', 'requests', 'total_tokens', 'cost_usd', 'source']) {
      expect(Object.hasOwn(body, forbidden)).toBe(false);
    }
  });

  it('refuses to submit an override that prices nothing', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminLlmProxyEditor />);
    await openModelsTab();

    await user.click(await screen.findByTestId('llm-proxy-add-price'));
    await user.type(await screen.findByTestId('llm-proxy-price-provider'), 'openai');
    await user.type(screen.getByTestId('llm-proxy-price-model'), 'gpt-6');

    // Identity is complete and no price is set. Saving would mark the row
    // overridden — permanently excluding it from the price sync — while leaving
    // it billed at the invented fallback rate, so the control is disabled
    // rather than deferring the refusal to a 400.
    expect(screen.getByTestId('llm-proxy-price-save')).toBeDisabled();
    expect(writes()).toHaveLength(0);
  });

  it('says so when the unpriced check could not run, rather than showing nothing', async () => {
    recorded = [];
    server.use(
      http.get('*/admin/gateway/status', () => HttpResponse.json(REACHABLE_STATUS)),
      http.get('*/admin/gateway/models', () =>
        HttpResponse.json({
          ...CATALOGUE,
          unpriced: [],
          unpriced_error: 'read unpriced models: timeout',
        }),
      ),
    );

    renderAdminRoute(<AdminLlmProxyEditor />);
    await openModelsTab();

    // An empty `unpriced` renders no alert at all, so without this the operator
    // would read a failed check as "every called model is priced" — the one
    // conclusion this panel exists to prevent.
    const failed = await screen.findByTestId('llm-proxy-unpriced-error');
    expect(failed).toHaveTextContent(/cannot say whether any exist/i);
    expect(failed).toHaveTextContent(/timeout/);
    expect(screen.queryByTestId('llm-proxy-unpriced')).not.toBeInTheDocument();
  });

  it('says a capped page is capped, so a short list is not read as complete', async () => {
    recorded = [];
    server.use(
      http.get('*/admin/gateway/status', () => HttpResponse.json(REACHABLE_STATUS)),
      http.get('*/admin/gateway/models', () =>
        HttpResponse.json({ ...CATALOGUE, truncated: true }),
      ),
    );

    renderAdminRoute(<AdminLlmProxyEditor />);
    await openModelsTab();

    expect(await screen.findByTestId('llm-proxy-truncated')).toBeInTheDocument();
  });

  it('offers to resume the sync only for a row that is actually overridden', async () => {
    renderAdminRoute(<AdminLlmProxyEditor />);
    await openModelsTab();

    await screen.findByTestId('llm-proxy-models-table');
    // The fixture row tracks upstream, so there is no override to clear. A
    // "Resume sync" button here would report success for a click that changed
    // nothing, which is how an operator concludes a synced row is pinned.
    expect(screen.queryByRole('button', { name: 'Resume sync' })).not.toBeInTheDocument();
  });
});

/*
 * Read-only mode (R3.4b, issue 2613). Each tab that writes is its own console
 * item: at `read` its notice shows, its writes are disabled, and the tab still
 * reads.
 */
describe('Admin › LLM Proxy › view only', () => {
  it('disables the catalogue writes at the read level of models-pricing', async () => {
    serveConsoleLevel('read', 'models-pricing');
    renderAdminRoute(<AdminLlmProxyEditor />);
    await openModelsTab();

    await screen.findByTestId('llm-proxy-models-table');
    expect(await findViewOnlyNotice()).toHaveAttribute('data-item-id', 'models-pricing');
    await waitFor(() => expect(screen.getByTestId('llm-proxy-add-price')).toBeDisabled());
    expect(enabledWriteControls()).toEqual([]);
    expect(writes()).toEqual([]);
  });

  it('disables the alert settings at the read level of budgets-alerts', async () => {
    server.use(
      http.get('*/admin/gateway/budget-alerts', () => HttpResponse.json({ enabled: true, threshold_pct: 80 })),
    );
    serveConsoleLevel('read', 'budgets-alerts');
    const user = userEvent.setup();
    renderAdminRoute(<AdminLlmProxyEditor />);
    await user.click(await screen.findByRole('tab', { name: 'Budget alerts' }));

    await findViewOnlyNotice();
    await waitFor(() => expect(screen.getByTestId('llm-proxy-alerts-save')).toBeDisabled());
    expect(screen.getByTestId('llm-proxy-alerts-enabled').querySelector('input')).toBeDisabled();
    expect(enabledWriteControls()).toEqual([]);
  });
});
