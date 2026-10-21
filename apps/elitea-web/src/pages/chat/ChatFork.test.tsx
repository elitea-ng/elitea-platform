/**
 * The chat page's fork (client contract 1.11): "Fork from here" posts the
 * fork point and opens the new conversation, a 409 tells the person to wait
 * for the reply, and the "Forked from" line shows only when the server named
 * the source (the reader can still see it) and opens it.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider, useLocation } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { ForkErrorSnackbar, ForkedFromLink, useChatFork } from './ChatFork';
import { forkedFromOf } from './forkedFrom';

const globals = globalThis as unknown as Record<string, unknown>;
const at = new Date().toISOString();
let posted: { readonly id: string; readonly body: unknown }[];

function Harness({ details }: { readonly details: unknown }): React.JSX.Element {
  const location = useLocation();
  const fork = useChatFork('42', '7');
  return (
    <>
      <ForkedFromLink forkedFrom={forkedFromOf(details)} />
      <button type="button" onClick={() => fork.onForkFromMessage?.('group-uuid-3')}>
        fork-from-message
      </button>
      <ForkErrorSnackbar fork={fork} />
      <p data-testid="where">{location.pathname}</p>
      <Outlet />
    </>
  );
}

function mount(details: unknown) {
  const rootRoute = createRootRoute({ component: () => <Harness details={details} /> });
  const router = createRouter({
    routeTree: rootRoute.addChildren([createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => null })]),
    history: createMemoryHistory({ initialEntries: ['/chat/7'] }),
  });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return router;
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  posted = [];
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('useChatFork', () => {
  it('forks up to the message and opens the new conversation', async () => {
    server.use(
      http.post('*/elitea_core/conversation_fork/prompt_lib/:projectId/:conversationId', async ({ params, request }) => {
        posted.push({ id: String(params['conversationId']), body: await request.json() });
        return HttpResponse.json({ id: 91, uuid: '00000000-0000-4000-8000-000000000091', name: 'Plan (fork)', source: 'elitea', created_at: at, updated_at: at, duration: -1, message_groups_count: 3, is_pinned: false, is_archived: false, forked_from: { conversation_id: 7, message_id: 3 } }, { status: 201 });
      }),
    );
    const router = mount(undefined);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'fork-from-message' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/91'));
    expect(posted).toEqual([{ id: '7', body: { message_id: 'group-uuid-3' } }]);
  });

  it('tells the person to wait when the fork point is not settled (409), and stays', async () => {
    server.use(http.post('*/elitea_core/conversation_fork/prompt_lib/:projectId/:conversationId', () => HttpResponse.json({ error: 'fork point is not settled' }, { status: 409 })));
    const router = mount(undefined);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'fork-from-message' }));
    expect(await screen.findByText('Wait for the reply to finish, then fork again.')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/chat/7');
  });
});

describe('ForkedFromLink', () => {
  it('names the source and opens it', async () => {
    const router = mount({ forked_from: { conversation_id: 5, conversation_uuid: '00000000-0000-4000-8000-000000000005', name: 'Release plan' } });
    expect(await screen.findByTestId('chat-forked-from')).toHaveTextContent('Forked from Release plan');
    await userEvent.setup().click(screen.getByRole('button', { name: 'Release plan' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/5'));
  });

  it('shows nothing when the source is not named (deleted, or the reader cannot see it)', async () => {
    mount({ forked_from: { conversation_id: 5 } });
    await screen.findByRole('button', { name: 'fork-from-message' });
    expect(screen.queryByTestId('chat-forked-from')).toBeNull();
  });

  it('shows nothing for a conversation that is not a fork', async () => {
    mount({ id: '7', name: 'Plan' });
    await screen.findByRole('button', { name: 'fork-from-message' });
    expect(screen.queryByTestId('chat-forked-from')).toBeNull();
  });
});
