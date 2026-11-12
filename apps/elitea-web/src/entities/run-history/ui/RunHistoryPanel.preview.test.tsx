/**
 * The agent History tab's rows and detail (#1981): each run says what the user
 * asked and when, in the reader's local time, and opening one shows the whole
 * exchange. The agent page's own hidden test-chat conversations are listed too.
 */
import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within, type RenderResult } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { getListMessageTracesMockHandler } from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '../../../test/setup';

import { formatRunTimestamp } from '../lib/formatRunTimestamp';
import { RunHistoryPanel } from './RunHistoryPanel';

function renderPanel(ui: ReactElement): RenderResult {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

function row(id: number, createdAt: string): Record<string, unknown> {
  return {
    id,
    name: 'text generator',
    created_at: createdAt,
    updated_at: createdAt,
    duration: -1,
    message_groups_count: 2,
    is_pinned: false,
    is_archived: false,
    meta: null,
  };
}

function text(role: string, content: string): Record<string, unknown> {
  return { uuid: `${role}-${content}`, role, message_items: [{ item_type: 'text_message', item_details: { content } }] };
}

const GROUPS: Record<number, readonly Record<string, unknown>[]> = {
  1: [text('user', 'Write a haiku about rain'), text('assistant', 'Soft rain on the roof')],
  2: [text('user', 'Translate this to French'), text('assistant', 'Bonjour')],
};

function install(listCalls: URLSearchParams[]): void {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/elitea_core/conversations/prompt_lib/:projectId', ({ request }) => {
      const query = new URL(request.url).searchParams;
      listCalls.push(query);
      return HttpResponse.json(
        query.get('hidden') === 'only'
          ? { total: 1, rows: [row(2, '2026-11-12T17:00:00.000000')] }
          : { total: 1, rows: [row(1, '2026-11-12T16:30:11.816831')] },
      );
    }),
    http.get('*/elitea_core/conversation/prompt_lib/:projectId/:id', ({ params }) =>
      HttpResponse.json({ id: Number(params['id']), name: 'text generator', participants: [], meta: {}, message_groups: GROUPS[Number(params['id'])] ?? [] }),
    ),
    getListMessageTracesMockHandler({ total: 0, rows: [] }),
  );
}

afterEach(() => {
  resetGeneratedClient();
});

describe('RunHistoryPanel rows and detail (#1981)', () => {
  it('shows the first user message and a local timestamp instead of the title and raw ISO', async () => {
    install([]);
    renderPanel(<RunHistoryPanel projectId="1" entityName="application" entityId="9" onClose={vi.fn()} />);

    const rows = await screen.findAllByTestId('run-history-row');
    await waitFor(() => expect(within(rows[0]!).getByTestId('run-history-preview')).toHaveTextContent('Translate this to French'));
    // The newest run first, whichever list it came from.
    expect(within(rows[1]!).getByTestId('run-history-preview')).toHaveTextContent('Write a haiku about rain');
    expect(rows[1]).not.toHaveTextContent('text generator');
    expect(rows[1]).not.toHaveTextContent('2026-11-12T16:30:11');
    expect(rows[1]).toHaveTextContent(formatRunTimestamp('2026-11-12T16:30:11.816831'));
  });

  it('keeps the title while the message is unknown', async () => {
    install([]);
    server.use(http.get('*/elitea_core/conversation/prompt_lib/:projectId/:id', () => HttpResponse.json({ error: 'nope' }, { status: 500 })));
    renderPanel(<RunHistoryPanel projectId="1" entityName="application" entityId="9" onClose={vi.fn()} />);

    const rows = await screen.findAllByTestId('run-history-row');
    expect(within(rows[0]!).getByTestId('run-history-preview')).toHaveTextContent('text generator');
  });

  it("lists the agent page's hidden editor_test conversations beside the ordinary ones", async () => {
    const calls: URLSearchParams[] = [];
    install(calls);
    renderPanel(<RunHistoryPanel projectId="1" entityName="application" entityId="9" onClose={vi.fn()} />);

    expect(await screen.findAllByTestId('run-history-row')).toHaveLength(2);
    const hidden = calls.find((query) => query.get('hidden') === 'only');
    expect(hidden?.get('source')).toBe('editor_test');
    expect(hidden?.get('entity_name')).toBe('application');
    expect(hidden?.get('entity_meta_id')).toBe('9');
  });

  it('does not look for test chats on a toolkit', async () => {
    const calls: URLSearchParams[] = [];
    install(calls);
    renderPanel(<RunHistoryPanel projectId="1" entityName="toolkit" entityId="9" onClose={vi.fn()} />);

    await screen.findAllByTestId('run-history-row');
    expect(calls.some((query) => query.get('hidden') === 'only')).toBe(false);
  });

  it('shows the whole exchange, user message included, when a run is opened', async () => {
    const user = userEvent.setup();
    install([]);
    renderPanel(<RunHistoryPanel projectId="1" entityName="application" entityId="9" onClose={vi.fn()} />);

    await user.click((await screen.findAllByTestId('run-history-row'))[1]!);

    const entries = await screen.findAllByTestId('run-history-exchange-entry');
    expect(entries.map((entry) => [entry.getAttribute('data-role'), entry.textContent])).toEqual([
      ['user', expect.stringContaining('Write a haiku about rain')],
      ['assistant', expect.stringContaining('Soft rain on the roof')],
    ]);
  });
});
