import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetConversationMockHandler, getGetConversationResponseMock } from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { ThreadTargetLine } from './ThreadTargetLine';

const THREAD = '6f1c2d3e-4a5b-4c6d-8e7f-901234567890';

function renderLine(ui: ReactNode): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({ component: () => <QueryClientProvider client={client}>{ui}</QueryClientProvider> });
  const router = createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: ['/'] }) });
  render(<RouterProvider router={router} />);
}

describe('ThreadTargetLine', () => {
  beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
  afterEach(() => resetGeneratedClient());

  it('names the thread from its own read, by UUID, and links to it', async () => {
    let asked = '';
    server.use(
      getGetConversationMockHandler(({ params }) => {
        asked = String(params['conversationId']);
        return getGetConversationResponseMock({ uuid: THREAD, name: 'A colleague’s thread' });
      }),
    );
    renderLine(<ThreadTargetLine projectId="1" target={{ target_kind: 'workspace_thread', conversation_uuid: THREAD }} />);
    await waitFor(() => expect(screen.getByTestId('thread-target-line')).toHaveTextContent('Runs on A colleague’s thread'));
    expect(asked).toBe(THREAD);
    expect(screen.getByRole('link', { name: 'A colleague’s thread' })).toHaveAttribute('href', expect.stringContaining(`conversation=${THREAD}`));
  });

  it('says "a handed-off thread" when the viewer cannot read the thread', async () => {
    let answered = false;
    server.use(
      http.get('*/elitea_core/conversation/prompt_lib/:projectId/:conversationId', () => {
        answered = true;
        return HttpResponse.json({ error: 'forbidden' }, { status: 403 });
      }),
    );
    renderLine(<ThreadTargetLine projectId="1" target={{ target_kind: 'workspace_thread', conversation_uuid: THREAD }} />);
    await waitFor(() => expect(answered).toBe(true));
    expect(screen.getByTestId('thread-target-line')).toHaveTextContent('Runs on a handed-off thread');
  });

  it('shows nothing for a target that is not a thread', async () => {
    renderLine(<ThreadTargetLine projectId="1" target={{ target_kind: 'pipeline' }} />);
    await waitFor(() => expect(document.body).toBeInTheDocument());
    expect(screen.queryByTestId('thread-target-line')).toBeNull();
  });
});
