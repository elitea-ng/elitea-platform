/**
 * Behaviour tests for `RunHistoryPanel` (issue #868): the empty state, and a
 * populated list that opens a conversation's trace on select — against MSW,
 * not a mocked hook, so the real generated `useListConversations`/
 * `useListMessageTraces` wiring is exercised the same way
 * `hook-envelope.test.tsx` proves the envelope shape for `useListApplications`.
 */
import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, type RenderResult } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../../test/setup';

import {
  getListConversationsMockHandler,
  getListMessageTracesMockHandler,
} from '@/shared/api/generated/chat/chat.msw';
import type { ConversationListing, MessageTraceListing } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { RunHistoryPanel } from './RunHistoryPanel';

/** `renderWithTheme` alone wraps `theme.vars.*` reads; a query hook also needs a real `QueryClientProvider`, so this wraps both. */
function renderPanel(ui: ReactElement): RenderResult {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

const EMPTY_LISTING: ConversationListing = { total: 0, rows: [] };

const ONE_RUN_LISTING: ConversationListing = {
  total: 1,
  rows: [
    {
      id: 42,
      name: 'A conversation with the agent',
      created_at: '2026-01-01T00:00:00.000000',
      updated_at: '2026-01-01T00:01:05.000000',
      duration: -1,
      message_groups_count: 3,
      meta: null,
    },
  ],
};

const TRACE_LISTING: MessageTraceListing = {
  total: 1,
  rows: [
    {
      id: 7,
      message_group_id: 1,
      kind: 'tool_call',
      tool_name: 'list_issues',
      parent_agent_name: null,
      parent_agent_call_id: null,
      started_at: '2026-01-01T00:00:10.000000',
      finished_at: '2026-01-01T00:00:12.000000',
      is_error: false,
      step_type: null,
      model_name: null,
      finish_reason: null,
      attrs: null,
    },
  ],
};

afterEach(() => {
  resetGeneratedClient();
});

describe('RunHistoryPanel', () => {
  it('shows the empty state when the entity has no conversations yet', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListConversationsMockHandler(EMPTY_LISTING));

    renderPanel(<RunHistoryPanel projectId="1" entityName="application" entityId="9" onClose={vi.fn()} />);

    await waitFor(() => expect(screen.getByText('No runs yet')).toBeInTheDocument());
    expect(screen.queryByTestId('run-history-row')).not.toBeInTheDocument();
  });

  it('lists a run and opens its trace on select', async () => {
    const user = userEvent.setup();
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getListConversationsMockHandler(ONE_RUN_LISTING),
      getListMessageTracesMockHandler(TRACE_LISTING),
    );

    renderPanel(<RunHistoryPanel projectId="1" entityName="application" entityId="9" onClose={vi.fn()} />);

    const row = await screen.findByTestId('run-history-row');
    expect(row).toHaveTextContent('A conversation with the agent');
    // Duration is derived client-side from created_at/updated_at (the
    // server's own `duration` field is a documented -1 placeholder).
    expect(row).toHaveTextContent('1m 5s');

    expect(screen.getByTestId('run-history-no-selection')).toBeInTheDocument();

    await user.click(row);

    const step = await screen.findByTestId('run-history-trace-step');
    expect(step).toHaveTextContent('list_issues');
    expect(screen.queryByTestId('run-history-no-selection')).not.toBeInTheDocument();
  });

  it('calls onClose when the close button is clicked', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListConversationsMockHandler(EMPTY_LISTING));

    renderPanel(<RunHistoryPanel projectId="1" entityName="toolkit" entityId="3" onClose={onClose} />);

    await waitFor(() => expect(screen.getByText('No runs yet')).toBeInTheDocument());
    await user.click(screen.getByTestId('run-history-close'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
