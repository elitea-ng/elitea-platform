/**
 * useIndexHistory.hooks.test.tsx — this hook needs both TanStack Router
 * context (`useSelectedProjectId`'s `useRouteContext`) and a real
 * `QueryClient` (`useIndexHistoryConversationDetailsQuery`), so — matching
 * `features/agents/ui/DeleteApplicationButton.test.tsx`'s own documented
 * reason for the same combination — this drives a real `RouterProvider`
 * with a probe component instead of `renderHook`'s bare wrapper.
 */
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useIndexesStore } from '../../model/indexesStore';

import { useIndexHistory } from './useIndexHistory.hooks';
import type { ProgressHistoryOptions } from './useIndexHistory.hooks';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useIndexesStore.setState({ tempIndexes: [], indexPatches: {}, toolkitScheduler: {}, selectedHistoryItem: null });
});

afterEach(() => {
  resetGeneratedClient();
});

function ProbeComponent(props: { options?: ProgressHistoryOptions | null }) {
  const result = useIndexHistory(props.options ?? null);
  return (
    <div>
      <span data-testid="index-history-mode">{String(result.isHistoryMode)}</span>
      <span data-testid="index-history-loading">{String(result.isHistoryLoading)}</span>
      <span data-testid="index-history-message-count">{result.historyMessages.length}</span>
      <span data-testid="index-history-need-generate">{String(result.needGenerateProgressingIndexHistory)}</span>
      <span data-testid="index-history-first-message">{result.historyMessages[0]?.content ?? ''}</span>
    </div>
  );
}

function renderProbe(props: { options?: ProgressHistoryOptions | null } = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ProbeComponent {...props} />
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  return render(<RouterProvider router={router} />);
}

describe('useIndexHistory', () => {
  it('is not in history mode and has no messages when no history item is selected', async () => {
    renderProbe();
    expect(await screen.findByTestId('index-history-mode')).toHaveTextContent('false');
    expect(screen.getByTestId('index-history-message-count')).toHaveTextContent('0');
  });

  it('enters history mode and fetches the conversation once a history item with a conversation_id is selected', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/proj-1/conv-1`, () =>
        HttpResponse.json({
          message_groups: [
            {
              id: 1,
              uuid: 'u1',
              author_participant_id: 'user-1',
              content: 'hello',
              created_at: '2024-01-01 00:00:00',
              sent_to_id: 'toolkit-1',
            },
          ],
          participants: [{ id: 'user-1', entity_name: 'user', meta: { user_name: 'Alice' } }],
        }),
      ),
    );

    useIndexesStore.getState().selectHistoryItem({ conversation_id: 'conv-1', state: 'completed' });
    renderProbe();

    expect(await screen.findByTestId('index-history-mode')).toHaveTextContent('true');
    await waitFor(() => expect(screen.getByTestId('index-history-message-count')).toHaveTextContent('1'));
  });

  it('falls back to a mock message when the selected history item has no conversation (scheduled reindex, conversation_id explicitly null)', async () => {
    useIndexesStore
      .getState()
      .selectHistoryItem({ conversation_id: null, state: 'completed', updated: 3, indexed: 10, updated_on: 1_700_000_000 });
    renderProbe();
    await waitFor(() => expect(screen.getByTestId('index-history-message-count')).toHaveTextContent('1'));
  });

  it('shows no messages when a history item is selected with conversation_id simply absent (not null) and no error', async () => {
    useIndexesStore.getState().selectHistoryItem({ state: 'completed' });
    renderProbe();
    expect(await screen.findByTestId('index-history-mode')).toHaveTextContent('true');
    expect(screen.getByTestId('index-history-message-count')).toHaveTextContent('0');
  });

  it('falls back to a mock failure message (with exception) when the selected history item errored', async () => {
    useIndexesStore.getState().selectHistoryItem({ conversation_id: null, state: 'failed', error: 'boom' });
    renderProbe();
    await waitFor(() => expect(screen.getByTestId('index-history-message-count')).toHaveTextContent('1'));
  });

  /**
   * Regression test for a real bug (found while writing tests for a sibling
   * consumer, `features/toolkits/lib/hooks/useToolkitChat.hooks.ts`'s
   * in-progress-index-recovery effect): during reindex recovery, no History
   * tab item is ever selected (`selectedHistoryItem` stays `null`, so
   * `isHistoryMode` is `false`), yet this hook still fires the SAME
   * conversation-details query for `progressHistoryOptions.conversationId`.
   * `historyMessages`'s `conversation` derivation used to be gated on
   * `isHistoryMode`, so it was forced to `null` here regardless of whether
   * the query had resolved -- silently discarding a successfully recovered
   * conversation's real messages (`historyMessages` always came out `[]`).
   * `needGenerateProgressingIndexHistory` is unaffected by that gate (it
   * only checks `conversationDetails`/`isConversationDetailsFetching`
   * directly), so it correctly flips `true` even while this bug was live --
   * which is exactly why the recovery flow's "fetch worked" signal fired
   * while the messages it was supposed to gate stayed empty.
   */
  it('recovers a real in-progress conversation and its messages even though no History item is selected (isHistoryMode stays false)', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/proj-1/conv-recovery`, () =>
        HttpResponse.json({
          message_groups: [
            {
              id: 1,
              uuid: 'u1',
              author_participant_id: 'user-1',
              content: 'recovered in-progress message',
              created_at: '2024-01-01 00:00:00',
              sent_to_id: 'toolkit-1',
            },
          ],
          participants: [{ id: 'user-1', entity_name: 'user', meta: { user_name: 'Alice' } }],
        }),
      ),
    );

    renderProbe({ options: { shouldRecover: true, conversationId: 'conv-recovery' } });

    expect(await screen.findByTestId('index-history-mode')).toHaveTextContent('false');
    await waitFor(() => expect(screen.getByTestId('index-history-need-generate')).toHaveTextContent('true'));
    expect(screen.getByTestId('index-history-message-count')).toHaveTextContent('1');
    expect(screen.getByTestId('index-history-first-message')).toHaveTextContent('recovered in-progress message');
  });
});
