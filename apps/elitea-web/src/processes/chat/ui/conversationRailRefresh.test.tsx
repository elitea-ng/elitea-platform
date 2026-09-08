/**
 * The conversation the FIRST SEND creates has to reach the rail beside it.
 *
 * There is no route that creates an empty conversation from a button: the
 * shell's Create control resets the chat surface, and the row is written by the
 * first send (`useChatBoxSend`'s `createConversationForSend`). The rail is
 * mounted by `ChatWithEditors`, one layer above `pages/chat`, and reads a
 * CACHED grouped listing — so "the page told the rail" is a statement about two
 * units at once and is exactly what neither unit's own test can make.
 *
 * `pages/chat/index.test.tsx` already asserts the invalidation by counting
 * requests through a probe hook. That is the write side. This file mounts the
 * REAL rail (`ChatConversationSidebar` -> `Conversations` -> `DateGroup` ->
 * `ConversationItemRow`) beside the page and asserts the row the E2E journey
 * looks for — `conversation-item-{id}` — actually appears in it. A refetch that
 * lands in the cache and never reaches the rendered rail passes the request
 * count and fails here.
 */
import type { ReactNode } from 'react';

import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import type { AnyRouter } from '@tanstack/react-router';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import ChatPage from '@/pages/chat';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource } from '@/shared/api/sse/testing';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { ChatConversationSidebar } from './ChatConversationSidebar';
import { ChatWithEditors } from './ChatWithEditors';

const BASE = '/api/v2';
const PROJECT = '77';
/** The conversation that already exists, and the one the send creates. */
const EXISTING = 41;
const CREATED = 42;

/** The listing's own answer, mutated by the create so a refetch can be told from a re-render. */
let stored: readonly { id: number; name: string }[] = [];
let listingRequests = 0;

function railHandlers() {
  return [
    http.get(`${BASE}/auth/permissions/prompt_lib/${PROJECT}`, () =>
      HttpResponse.json([{ name: PERMISSIONS.chat.folders.get, enabled: true }]),
    ),
    http.get(`${BASE}/social/author`, () =>
      HttpResponse.json({ id: 'u1', name: 'Ada', avatar: '', personal_project_id: PROJECT }),
    ),
    http.get(`${BASE}/elitea_core/folder/prompt_lib/${PROJECT}`, () => {
      listingRequests += 1;
      return HttpResponse.json({
        pinned: { conversations: [] },
        date_groups: [
          {
            name: 'Today',
            conversations: stored.map((c) => ({ id: String(c.id), name: c.name, is_private: true, author_id: 1 })),
          },
        ],
        folders: [],
        total_folders: 0,
      });
    }),
    http.post(`${BASE}/elitea_core/conversations/prompt_lib/${PROJECT}`, () => {
      stored = [{ id: CREATED, name: 'First turn' }, ...stored];
      return HttpResponse.json(
        { id: CREATED, uuid: 'conversation-uuid-42', project_id: PROJECT, name: 'First turn', participants: [] },
        { status: 201 },
      );
    }),
    http.post(`${BASE}/elitea_core/participants/prompt_lib/${PROJECT}/${CREATED}`, () => HttpResponse.json([])),
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${EXISTING}`, () =>
      HttpResponse.json({ id: String(EXISTING), uuid: 'conversation-uuid-41', name: 'An existing conversation', participants: [] }),
    ),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/${EXISTING}`, () =>
      HttpResponse.json({ items: [], total: 0, page: 0, page_size: 50, total_pages: 1 }),
    ),
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${CREATED}`, () =>
      HttpResponse.json({ id: String(CREATED), uuid: 'conversation-uuid-42', name: 'First turn', participants: [] }),
    ),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/${CREATED}`, () =>
      HttpResponse.json({ items: [], total: 0, page: 0, page_size: 50, total_pages: 1 }),
    ),
    http.get(`${BASE}/configurations/models/${PROJECT}`, () =>
      HttpResponse.json({ items: [{ id: 'model-1', name: 'model-1', project_id: PROJECT, default: true }] }),
    ),
    http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
    http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/:conversationId`, () =>
      HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 }),
    ),
    http.post(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/:conversationUuid`, () =>
      HttpResponse.json({ execution_id: 'execution-1', events_url: `${BASE}/executions/${PROJECT}/execution-1/events` }),
    ),
  ];
}

/** The two real chat routes, with the rail mounted OUTSIDE them, as `ChatWithEditors` mounts it. */
function renderChatSurface(surface: () => ReactNode, initialEntry = '/chat'): AnyRouter {
  const rootRoute = createRootRoute({ component: (): ReactNode => <Outlet /> });
  const chatRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: surface });
  const conversationRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: surface });
  const router = createRouter({
    routeTree: rootRoute.addChildren([chatRoute, conversationRoute]),
    history: createMemoryHistory({ initialEntries: [initialEntry] }),
  });
  render(
    <AppProviders>
      <RouterProvider router={router as never} />
    </AppProviders>,
  );
  return router;
}

/** The pair the page/rail case mounts: the rail beside the page, no editors. */
const pageBesideRail = (): ReactNode => (
  <>
    <ChatConversationSidebar />
    <ChatPage />
  </>
);

let restoreScrollIntoView: (() => void) | undefined;

beforeEach(() => {
  stored = [{ id: EXISTING, name: 'An existing conversation' }];
  listingRequests = 0;
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: { id: PROJECT, name: 'Project' } });
  server.use(...railHandlers());
  const original = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
  Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() });
  restoreScrollIntoView = () => {
    if (original) Object.defineProperty(Element.prototype, 'scrollIntoView', original);
    else Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
  };
});

afterEach(() => {
  restoreScrollIntoView?.();
  useSelectedProjectStore.setState({ project: null });
  resetGeneratedClient();
});

describe('the chat surface: the rail beside the page', () => {
  it('shows the conversation the first send created, beside the one it was created from', async () => {
    const eventSources = installTestEventSource();
    try {
      renderChatSurface(pageBesideRail);

      // The rail is up and holds the conversation that already existed. The
      // Today group is expanded by `useDateGroupExpansion`'s own default.
      expect(await screen.findByTestId(`conversation-item-${EXISTING}`, {}, { timeout: 10_000 })).toBeInTheDocument();
      await waitFor(() => expect(listingRequests).toBe(1));

      const user = userEvent.setup();
      const input = await screen.findByPlaceholderText('Type your message...');
      await user.type(input, 'First turn{Enter}');

      // The row the E2E journey looks for, by the id the create response
      // carried — not merely a second GET.
      expect(await screen.findByTestId(`conversation-item-${CREATED}`, {}, { timeout: 10_000 })).toBeInTheDocument();
      expect(screen.getByTestId(`conversation-item-${EXISTING}`)).toBeInTheDocument();
    } finally {
      eventSources.restore();
    }
  });

  /**
   * The same claim, through the CREATE CONTROL — which is what the E2E journey
   * drives, and which differs in two ways this case reproduces: the reader
   * starts INSIDE another conversation, and `?create=1` REMOUNTS the chat
   * subtree (`useCreateChatReset` keys it). The rail sits outside that key, so
   * the invalidation the remounted page fires still has to land on the rail's
   * live listing.
   */
  it('shows it when the reader got there through the Create control, from another conversation', async () => {
    const eventSources = installTestEventSource();
    try {
      const router = renderChatSurface(() => <ChatWithEditors />, `/chat/${EXISTING}`);

      expect(await screen.findByTestId(`conversation-item-${EXISTING}`, {}, { timeout: 10_000 })).toBeInTheDocument();
      await waitFor(() => expect(listingRequests).toBe(1));

      // "+ Create -> Chat": the flag `widgets/create-button` writes.
      await act(async () => {
        await router.navigate({ to: '/chat', search: { create: '1' } } as never);
      });
      await waitFor(() => expect(router.state.location.pathname).toBe('/chat'));

      const user = userEvent.setup();
      const input = await screen.findByPlaceholderText('Type your message...');
      await user.type(input, 'First turn{Enter}');

      expect(await screen.findByTestId(`conversation-item-${CREATED}`, {}, { timeout: 10_000 })).toBeInTheDocument();
      expect(screen.getByTestId(`conversation-item-${EXISTING}`)).toBeInTheDocument();
    } finally {
      eventSources.restore();
    }
  });

  /**
   * DEFECT this closes: the row survives, so the announcement must too.
   *
   * `resolveConversationForSend` POSTs the conversation BEFORE any transport is
   * tried, so a refused turn leaves a real, committed conversation behind. The
   * send handler used to return a bare `{success:false}` on that path,
   * discarding `createdConversation` — the route stayed on `/chat`, the rail's
   * cached listing was never invalidated, and a conversation that existed in
   * the database appeared on no screen until a reload.
   *
   * This is not a rare branch: every deployment with no live transport takes
   * it, the E2E stack (`VITE_SOCKET_SERVER: ""`) included.
   */
  it('shows it even when the turn itself is refused — the row was committed before the transport was tried', async () => {
    const eventSources = installTestEventSource();
    server.use(
      http.post(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/:conversationUuid`, () =>
        HttpResponse.json({ error: 'no runtime plane' }, { status: 503 }),
      ),
    );
    try {
      renderChatSurface(pageBesideRail);

      expect(await screen.findByTestId(`conversation-item-${EXISTING}`, {}, { timeout: 10_000 })).toBeInTheDocument();
      await waitFor(() => expect(listingRequests).toBe(1));

      const user = userEvent.setup();
      const input = await screen.findByPlaceholderText('Type your message...');
      await user.type(input, 'First turn{Enter}');

      expect(await screen.findByTestId(`conversation-item-${CREATED}`, {}, { timeout: 10_000 })).toBeInTheDocument();
    } finally {
      eventSources.restore();
    }
  });
});
