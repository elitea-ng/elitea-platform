/**
 * DEFECT: a notification deep link opened an empty new chat.
 *
 * `features/notifications/lib/routes.ts`'s `chatHref` builds every
 * `chat_user_added`/`chat_user_mentioned` link as
 * `/{projectId}/chat?conversation=<id>&message_id=<id>`, and the project
 * splat route strips the project segment, so the click lands on
 * `/chat?conversation=<id>&message_id=<id>` — a URL with NO path param.
 * `ChatPage` read the conversation only from `useParams`, so it saw
 * `undefined`, `useChatPageData` disabled its queries, and the user got a
 * blank new chat instead of the conversation they were mentioned in.
 *
 * The second half was dead as well: `chatSessionStore.messageIdToView` is
 * read by `ChatMessageList` and `useHighlightUserMessage`, but the only two
 * writers in the app both passed `''`, so `message_id` never scrolled
 * anywhere.
 *
 * These cases mount the real route tree, because the pure resolver
 * (`entities/conversation`'s `resolveConversationIdFromUrl`) already had a
 * green test while having zero production callers — which is exactly how
 * this shipped.
 */
import type { ReactNode } from 'react';

import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { useChatSessionStore } from '@/entities/conversation';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource } from '@/shared/api/sse/testing';
import { server } from '@/test/setup';

import ChatPage, { findActiveParticipantById } from './index';

/*
 * The conversation-detail request is the observable proof that the page
 * resolved a conversation at all: `useChatPageData` disables both the detail
 * and the message queries while the id is undefined. Recorded through MSW
 * rather than by substituting `ChatBox` — R-M1 allows only the network
 * boundary to be doubled.
 */
const detailRequests: string[] = [];

const BASE = '/api/v2';
const PROJECT = '77';
const CONVERSATION = '5';

describe('findActiveParticipantById', () => {
  const participants = [
    { id: '2', entity_name: 'dummy' },
    { id: '25', entity_name: 'toolkit' },
    { id: '26', entity_name: 'application' },
  ];

  it('restores an application participant', () => {
    expect(findActiveParticipantById(participants, '26')).toEqual(participants[2]);
  });

  it('rejects a stored toolkit participant', () => {
    expect(findActiveParticipantById(participants, '25')).toBeUndefined();
  });
});

function handlers() {
  return [
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 'u1', name: 'Ada', avatar: '', personal_project_id: PROJECT })),
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${CONVERSATION}`, ({ request }) => {
      detailRequests.push(request.url);
      return HttpResponse.json({ id: CONVERSATION, uuid: 'conversation-uuid-5', name: 'A conversation', participants: [] });
    }),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
      HttpResponse.json({ items: [], total: 0, page: 0, page_size: 50, total_pages: 1 }),
    ),
    // `ChatBox` reads the model catalogue on mount; answered so the run is
    // not full of unhandled-request noise.
    http.get(`${BASE}/configurations/models/${PROJECT}`, () =>
      HttpResponse.json({ items: [{ id: 'model-1', name: 'model-1', project_id: PROJECT, default: true }] }),
    ),
  ];
}

/** The two real chat routes, so `navigate({to:'/chat/$conversationId'})` resolves the same way it does in the app. */
function renderAt(initialEntry: string) {
  const rootRoute = createRootRoute({ component: (): ReactNode => <Outlet /> });
  const chatRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: ChatPage });
  const conversationRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: ChatPage });
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

beforeEach(() => {
  detailRequests.length = 0;
  useChatSessionStore.setState({ messageIdToView: '' });
  configureGeneratedClient({ baseUrl: BASE });
  server.use(...handlers());
});

afterEach(() => {
  resetGeneratedClient();
});

describe('ChatPage deep links', () => {
  it('opens the conversation named by `?conversation=`, not a blank new chat', async () => {
    renderAt(`/chat?conversation=${CONVERSATION}`);

    // A generous timeout: the detail request only starts after the author
    // query resolves, so this is two sequential round trips on a loaded
    // machine, not one.
    await waitFor(() => expect(detailRequests).toHaveLength(1), { timeout: 5000 });
  });

  it('canonicalises the URL to /chat/<id> and drops the consumed param, keeping message_id', async () => {
    const router = renderAt(`/chat?conversation=${CONVERSATION}&message_id=m1`);

    await waitFor(() => expect(router.state.location.pathname).toBe(`/chat/${CONVERSATION}`), { timeout: 5000 });
    const search = router.state.location.search as { readonly conversation?: string; readonly message_id?: string };
    expect(search.conversation).toBeUndefined();
    expect(search.message_id).toBe('m1');
  });

  it('publishes `?message_id=` into the store the message list scrolls by', async () => {
    renderAt(`/chat?conversation=${CONVERSATION}&message_id=m1`);

    await waitFor(() => expect(useChatSessionStore.getState().messageIdToView).toBe('m1'), { timeout: 5000 });
  });

  it('leaves a plain /chat URL as a new conversation', async () => {
    const router = renderAt('/chat');

    await waitFor(() => {
      expect(router.state.status).toBe('idle');
    });
    expect(router.state.location.pathname).toBe('/chat');
    expect(detailRequests).toHaveLength(0);
  });
});

/**
 * DEFECT: the composer took a file and showed nothing.
 *
 * `useAttachmentState` held the picked file, the "+" menu's "N left" counter
 * ticked down, and the send path uploaded it — but `ChatBox` never handed the
 * staged list to `NewChatInput`, and `NewChatInput` never handed
 * `slots.attachmentList` to `UserInput`. Both halves had green unit tests of
 * their own; the seam between them had none, so the user picked a file and got
 * no chip, no filename and no way to remove it.
 *
 * Mounted through the real page for that reason: this is a wiring defect, and
 * only a run that walks the whole composition root can see it.
 */
describe('ChatPage composer attachments', () => {
  it('shows a removable chip for a file staged on the next message', async () => {
    renderAt('/chat');
    const user = userEvent.setup();
    await screen.findByPlaceholderText('Type your message...');

    // The picker lives on the "+" menu's own "Attach Files" row, and that row
    // only exists while the menu is open — the always-mounted instance beside
    // it renders no DOM at all (see `AttachmentButton`'s `dropTargetOnly`).
    await user.click(screen.getByTestId('plus-menu-button'));
    const picker = await screen.findByTestId('plus-menu-attachments')
      .then((row) => row.parentElement?.querySelector<HTMLInputElement>('input[type="file"]') ?? null);
    expect(picker).not.toBeNull();
    const file = new File(['brief'], 'brief.txt', { type: 'text/plain' });
    Object.defineProperty(picker, 'files', { configurable: true, value: [file] });
    fireEvent.change(picker as HTMLInputElement);

    const chip = await screen.findByTestId('chat-attachment-chip-0');
    expect(chip).toHaveTextContent('brief.txt');

    await user.click(chip.querySelector('[data-testid="chat-attachment-remove-0"]') as Element);
    await waitFor(() => expect(screen.queryByTestId('chat-attachment-chip-0')).toBeNull());
  });
});

describe('ChatPage new-conversation promotion', () => {
  it('promotes the first persisted conversation into the route', async () => {
    const eventSources = installTestEventSource();
    const originalScrollIntoView = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
    Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() });
    const conversationCreates: string[] = [];
    const executionConversationIds: string[] = [];
    let executionNumber = 0;
    server.use(
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/${PROJECT}`, ({ request }) => {
        conversationCreates.push(request.url);
        return HttpResponse.json(
          { id: CONVERSATION, uuid: 'conversation-uuid-5', project_id: PROJECT, name: 'First turn', participants: [] },
          { status: 201 },
        );
      }),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/${PROJECT}/${CONVERSATION}`, () => HttpResponse.json([])),
      http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
      http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
        HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 }),
      ),
      http.post(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/:conversationUuid`, ({ params }) => {
        executionConversationIds.push(String(params.conversationUuid));
        executionNumber += 1;
        return HttpResponse.json({
          execution_id: `execution-${executionNumber}`,
          events_url: `${BASE}/executions/${PROJECT}/execution-${executionNumber}/events`,
        });
      }),
    );

    try {
      const router = renderAt('/chat');
      const user = userEvent.setup();
      const input = await screen.findByPlaceholderText('Type your message...');

      await user.type(input, 'First turn{Enter}');
      await waitFor(() => expect(router.state.location.pathname).toBe(`/chat/${CONVERSATION}`), { timeout: 5000 });
      await waitFor(() => expect(eventSources.getOpen()).toHaveLength(1));

      expect(conversationCreates).toHaveLength(1);
      expect(executionConversationIds).toEqual(['conversation-uuid-5']);
    } finally {
      if (originalScrollIntoView) Object.defineProperty(Element.prototype, 'scrollIntoView', originalScrollIntoView);
      else Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
      eventSources.restore();
    }
  });
});

/**
 * `ParticipantsWrapper` has always accepted a `renderContextBudget` slot and
 * has always threaded a real `conversationId` into it; nothing supplied the
 * slot, so the foot of the participants rail was empty. These cases assert the
 * page fills it — and that it does NOT for a conversation that has no server
 * state to report on.
 */
describe('ChatPage context budget slot', () => {
  const CONTEXT_STATUS_URL = `${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/${CONVERSATION}`;

  it('renders the context-budget panel for a real conversation', async () => {
    server.use(
      http.get(CONTEXT_STATUS_URL, () =>
        HttpResponse.json({
          current_tokens: 12000,
          max_tokens: 128000,
          message_groups_in_context: 4,
          strategy_name: 'sliding_window',
          context_analytics: { summaries_generated: 1 },
        }),
      ),
    );

    renderAt(`/chat/${CONVERSATION}`);

    await waitFor(() => expect(screen.getByTestId('context-budget-panel')).toBeTruthy(), { timeout: 5000 });
    expect(screen.getByTestId('context-budget-tokens').textContent).toBe('12\u00a0000 / 128\u00a0000 tokens');
    expect(screen.getByTestId('context-budget-stat-summaries').textContent).toBe('Summaries:1');
  });

  it('renders no panel on a plain /chat URL, where there is no conversation yet', async () => {
    const statusRequests: string[] = [];
    server.use(
      http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/*`, ({ request }) => {
        statusRequests.push(request.url);
        return HttpResponse.json({});
      }),
    );

    renderAt('/chat');

    await waitFor(() => expect(screen.queryByTestId('participants-container')).toBeTruthy(), { timeout: 5000 });
    expect(screen.queryByTestId('context-budget-panel')).toBeNull();
    expect(statusRequests).toHaveLength(0);
  });
});

describe('ChatPage participant removal', () => {
  it('removes an attached toolkit through the participant rail', async () => {
    const deletedParticipants: string[] = [];
    let toolkitAttached = true;
    const toolkitParticipant = {
      id: '25',
      entity_name: 'toolkit',
      entity_meta: { id: '20', name: 'rust_openapi_echo', project_id: PROJECT },
      entity_settings: { toolkit_type: 'openapi' },
    };

    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${CONVERSATION}`, ({ request }) => {
        detailRequests.push(request.url);
        return HttpResponse.json({
          id: CONVERSATION,
          uuid: 'conversation-uuid-5',
          name: 'A conversation',
          participants: toolkitAttached ? [toolkitParticipant] : [],
        });
      }),
      http.delete(
        `${BASE}/elitea_core/participant/prompt_lib/${PROJECT}/${CONVERSATION}/:participantId`,
        ({ params }) => {
          deletedParticipants.push(String(params.participantId));
          toolkitAttached = false;
          return new HttpResponse(null, { status: 204 });
        },
      ),
      http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
      http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
        HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 }),
      ),
    );

    renderAt(`/chat/${CONVERSATION}`);
    const user = userEvent.setup();

    await user.click(await screen.findByRole('button', { name: 'Expand participants' }));
    const toolkitName = await screen.findByText('rust_openapi_echo');
    await user.hover(toolkitName);
    fireEvent.click(await screen.findByRole('button', { name: 'Remove toolkit' }));
    await screen.findByText('Remove toolkit?');
    fireEvent.click(await screen.findByRole('button', { name: 'Remove' }));

    await waitFor(() => expect(deletedParticipants).toEqual(['25']), { timeout: 5000 });
    await waitFor(() => expect(screen.queryByText('rust_openapi_echo')).toBeNull(), { timeout: 5000 });
  });
});
