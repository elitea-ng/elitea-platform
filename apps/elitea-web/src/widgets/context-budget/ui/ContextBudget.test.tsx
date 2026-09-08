/**
 * The container's whole job is gating plus one query, so this exercises the
 * real `contextManagementApi` route through msw rather than mocking the hook:
 * a widget that fetches from the wrong URL would still pass a mocked test.
 */
import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ContextBudget } from './ContextBudget';

const BASE = '/api/v2';
const STATUS_URL = `${BASE}/elitea_core/context_analytics/prompt_lib/7/42`;

/** `eliteaFetch` returns a `{data,status,headers}` envelope whose `data` IS the JSON body, so the handler serves the body unwrapped. */
const STATUS_BODY = {
  current_tokens: 12000,
  max_tokens: 128000,
  message_groups_in_context: 9,
  strategy_name: 'sliding_window',
  context_analytics: { summaries_generated: 2 },
};

function wrap(ui: ReactElement): ReactElement {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{ui}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('ContextBudget', () => {
  it('renders the panel from the conversation-scoped context status', async () => {
    server.use(http.get(STATUS_URL, () => HttpResponse.json(STATUS_BODY)));

    const { findByTestId } = renderWithTheme(wrap(<ContextBudget conversationId="42" projectId="7" />));

    expect((await findByTestId('context-budget-tokens')).textContent).toContain('tokens');
    expect((await findByTestId('context-budget-stat-summaries')).textContent).toBe('Summaries:2');
  });

  it.each([
    ['no conversation (new or playback chat)', { projectId: '7' }],
    ['no project', { conversationId: '42' }],
    ['an empty conversation id', { conversationId: '', projectId: '7' }],
  ])('renders nothing and issues no request with %s', async (_label, props) => {
    const handler = vi.fn(() => HttpResponse.json(STATUS_BODY));
    server.use(http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/*`, handler));

    const { container } = renderWithTheme(wrap(<ContextBudget {...props} />));

    await waitFor(() => {
      expect(container.textContent).toBe('');
    });
    expect(handler).not.toHaveBeenCalled();
  });

  it('renders nothing when the status request fails', async () => {
    server.use(http.get(STATUS_URL, () => new HttpResponse(null, { status: 500 })));

    const { container } = renderWithTheme(wrap(<ContextBudget conversationId="42" projectId="7" />));

    await waitFor(() => {
      expect(container.querySelector('[data-testid="context-budget-panel"]')).toBeNull();
    });
  });
});

/*
 * The pencil and what it writes.
 *
 * `PUT /social/author` is asserted as the request that REACHED the server, not
 * as "a mutation was called": this panel used to be read-only by a documented
 * decision, and the legacy suite's `chat/test_chat_interface.py::
 * test_edit_context_settings` skipped itself when the pencil was absent.
 */
describe('ContextBudget — editing the budget', () => {
  /** The profile route both the dialog's seed value and its save go through. */
  function authorHandlers(stored: Record<string, unknown> | undefined, sink?: unknown[]) {
    return [
      http.get(`${BASE}/social/author`, () => HttpResponse.json(stored ?? {})),
      http.put(`${BASE}/social/author`, async ({ request }) => {
        sink?.push(await request.json());
        return HttpResponse.json({ ok: true });
      }),
    ];
  }

  it('opens the editor seeded with the reader\'s own stored budget', async () => {
    server.use(
      http.get(STATUS_URL, () => HttpResponse.json(STATUS_BODY)),
      ...authorHandlers({ default_context_management: { max_context_tokens: 10_000 } }),
    );
    const user = userEvent.setup();
    renderWithTheme(wrap(<ContextBudget conversationId="42" projectId="7" />));

    await user.click(await screen.findByTestId('context-budget-edit-button'));

    expect(await screen.findByTestId('context-budget-edit-dialog')).toBeInTheDocument();
    // The reader's OWN default, not the resolved 128 000 the panel shows.
    await waitFor(() => expect(screen.getByTestId('context-budget-max-tokens-input')).toHaveValue('10000'));
  });

  it('falls back to the budget the server resolved when the reader has never saved one', async () => {
    server.use(http.get(STATUS_URL, () => HttpResponse.json(STATUS_BODY)), ...authorHandlers({}));
    const user = userEvent.setup();
    renderWithTheme(wrap(<ContextBudget conversationId="42" projectId="7" />));

    await user.click(await screen.findByTestId('context-budget-edit-button'));

    await waitFor(() => expect(screen.getByTestId('context-budget-max-tokens-input')).toHaveValue('128000'));
  });

  it('saves the new budget to the same profile field Settings › Memory writes', async () => {
    const sent: unknown[] = [];
    server.use(
      http.get(STATUS_URL, () => HttpResponse.json(STATUS_BODY)),
      ...authorHandlers({ name: 'Ada', default_context_management: { preserve_recent_messages: 7 } }, sent),
    );
    const user = userEvent.setup();
    renderWithTheme(wrap(<ContextBudget conversationId="42" projectId="7" />));

    await user.click(await screen.findByTestId('context-budget-edit-button'));
    const input = await screen.findByTestId('context-budget-max-tokens-input');
    await user.clear(input);
    await user.type(input, '32000');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toMatchObject({
      name: 'Ada',
      default_context_management: { max_context_tokens: 32_000, preserve_recent_messages: 7, enabled: true },
    });
    await waitFor(() => expect(screen.queryByTestId('context-budget-edit-dialog')).not.toBeInTheDocument());
  });

  /**
   * DEFECT this closes. The save invalidated `['GET',
   * '/elitea_core/context_analytics/prompt_lib']` — a URL-shaped key namespace
   * no query in this app registers under — so `invalidateQueries` matched
   * nothing, resolved successfully, and the panel went on reporting the OLD
   * budget until a reload. Every existing test above passed throughout: they
   * assert what the PUT carried, never that the panel followed it.
   *
   * The status route is served from the same mutable `budget` the PUT writes,
   * so a panel that merely re-rendered its cached payload cannot pass this: the
   * new number exists only in a SECOND GET.
   */
  it('refetches the status it reads, so the panel shows the budget the save just wrote', async () => {
    const NBSP = '\u00a0';
    let budget = 10_000;
    const statusRequests: string[] = [];
    server.use(
      http.get(STATUS_URL, ({ request }) => {
        statusRequests.push(request.url);
        return HttpResponse.json({ ...STATUS_BODY, current_tokens: 0, max_tokens: budget });
      }),
      http.get(`${BASE}/social/author`, () =>
        HttpResponse.json({ default_context_management: { max_context_tokens: budget } }),
      ),
      http.put(`${BASE}/social/author`, async ({ request }) => {
        const body = (await request.json()) as { default_context_management?: { max_context_tokens?: number } };
        budget = body.default_context_management?.max_context_tokens ?? budget;
        return HttpResponse.json({ ok: true });
      }),
    );
    const user = userEvent.setup();
    renderWithTheme(wrap(<ContextBudget conversationId="42" projectId="7" />));

    expect((await screen.findByTestId('context-budget-tokens')).textContent).toBe(`0 / 10${NBSP}000 tokens`);
    await waitFor(() => expect(statusRequests).toHaveLength(1));

    await user.click(screen.getByTestId('context-budget-edit-button'));
    const input = await screen.findByTestId('context-budget-max-tokens-input');
    await user.clear(input);
    await user.type(input, '32000');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(screen.queryByTestId('context-budget-edit-dialog')).not.toBeInTheDocument());
    // The second GET is the invalidation; the number is what it answered with.
    await waitFor(() => expect(statusRequests.length).toBeGreaterThan(1));
    await waitFor(() =>
      expect(screen.getByTestId('context-budget-tokens').textContent).toBe(`0 / 32${NBSP}000 tokens`),
    );
  });

  it('refuses a budget the server would refuse, before sending it', async () => {
    const sent: unknown[] = [];
    server.use(http.get(STATUS_URL, () => HttpResponse.json(STATUS_BODY)), ...authorHandlers({}, sent));
    const user = userEvent.setup();
    renderWithTheme(wrap(<ContextBudget conversationId="42" projectId="7" />));

    await user.click(await screen.findByTestId('context-budget-edit-button'));
    const input = await screen.findByTestId('context-budget-max-tokens-input');
    await user.clear(input);
    await user.type(input, '10');

    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(sent).toHaveLength(0);
  });

  it('reports a refused save inside the dialog and keeps what was typed', async () => {
    server.use(
      http.get(STATUS_URL, () => HttpResponse.json(STATUS_BODY)),
      http.get(`${BASE}/social/author`, () => HttpResponse.json({})),
      http.put(`${BASE}/social/author`, () => HttpResponse.json({ error: 'nope' }, { status: 400 })),
    );
    const user = userEvent.setup();
    renderWithTheme(wrap(<ContextBudget conversationId="42" projectId="7" />));

    await user.click(await screen.findByTestId('context-budget-edit-button'));
    const input = await screen.findByTestId('context-budget-max-tokens-input');
    await user.clear(input);
    await user.type(input, '32000');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByTestId('context-budget-edit-error')).toBeInTheDocument();
    expect(screen.getByTestId('context-budget-edit-dialog')).toBeInTheDocument();
    expect(screen.getByTestId('context-budget-max-tokens-input')).toHaveValue('32000');
  });

  it('offers no pencil in the collapsed rail, which has no room for it', async () => {
    server.use(http.get(STATUS_URL, () => HttpResponse.json(STATUS_BODY)), ...authorHandlers({}));
    const { findByTestId } = renderWithTheme(
      wrap(<ContextBudget conversationId="42" projectId="7" collapsed />),
    );

    await findByTestId('context-budget-collapsed');
    expect(screen.queryByTestId('context-budget-edit-button')).not.toBeInTheDocument();
  });
});
