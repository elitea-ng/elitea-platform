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
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { useChatSessionStore } from '@/entities/conversation';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { folderApi } from '@/entities/folder';
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

/**
 * The router context the app supplies (`app/router-context.ts`'s
 * `AuthContext`), narrowed to the one accessor several feature slices read
 * the selected project through (`useSelectedProjectId`). Without it those
 * slices resolve NO project, their queries stay disabled and the permission
 * gates that depend on them answer "denied" — which looks exactly like a
 * feature that was never wired.
 */
const ROUTER_CONTEXT = { auth: { getSelectedProjectId: () => PROJECT } };

/** The two real chat routes, so `navigate({to:'/chat/$conversationId'})` resolves the same way it does in the app. */
function renderAt(initialEntry: string, component: () => ReactNode = () => <ChatPage />) {
  const rootRoute = createRootRoute({ component: (): ReactNode => <Outlet /> });
  const chatRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat', component });
  const conversationRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component });
  const router = createRouter({
    routeTree: rootRoute.addChildren([chatRoute, conversationRoute]),
    history: createMemoryHistory({ initialEntries: [initialEntry] }),
    context: ROUTER_CONTEXT,
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
/**
 * DEFECT: a conversation created by the first send was missing from the rail
 * beside it.
 *
 * The rail's grouped listing (`folderApi.useList`) is a cached query, and the
 * only writers that invalidated it were the FOLDER mutations. A conversation
 * is written by the first send, not by a button, so nothing told the listing
 * it was stale: the new conversation was in the route, in the transcript and
 * in the database, and absent from the list of conversations until something
 * else happened to refetch.
 *
 * The rail itself is mounted by `processes/chat`, one layer above this page,
 * so the observation here is the listing QUERY — the same query, through the
 * same hook the rail uses, sharing this page's cache. A second GET is the
 * invalidation.
 */
describe('ChatPage conversation rail refresh', () => {
  /** Mounted beside the page: the rail's own listing hook, on the rail's own key. */
  function RailListingProbe(): ReactNode {
    folderApi.useList({ projectId: PROJECT, params: { sort_by: 'updated_at', sort_order: 'desc' } });
    return null;
  }

  it('invalidates the rail listing when the first send creates the conversation', async () => {
    const eventSources = installTestEventSource();
    const originalScrollIntoView = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
    Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() });
    const listingRequests: string[] = [];
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/${PROJECT}`, ({ request }) => {
        listingRequests.push(request.url);
        return HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [], total_folders: 0 });
      }),
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/${PROJECT}`, () =>
        HttpResponse.json(
          { id: CONVERSATION, uuid: 'conversation-uuid-5', project_id: PROJECT, name: 'First turn', participants: [] },
          { status: 201 },
        ),
      ),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/${PROJECT}/${CONVERSATION}`, () => HttpResponse.json([])),
      http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
      http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
        HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 }),
      ),
      http.post(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/:conversationUuid`, () =>
        HttpResponse.json({ execution_id: 'execution-1', events_url: `${BASE}/executions/${PROJECT}/execution-1/events` }),
      ),
    );

    try {
      renderAt('/chat', () => (
        <>
          <ChatPage />
          <RailListingProbe />
        </>
      ));
      const user = userEvent.setup();
      const input = await screen.findByPlaceholderText('Type your message...');
      await waitFor(() => expect(listingRequests).toHaveLength(1), { timeout: 5000 });

      await user.type(input, 'First turn{Enter}');

      await waitFor(() => expect(listingRequests.length).toBeGreaterThan(1), { timeout: 5000 });
    } finally {
      if (originalScrollIntoView) Object.defineProperty(Element.prototype, 'scrollIntoView', originalScrollIntoView);
      else Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
      eventSources.restore();
    }
  });
});

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

/**
 * DEFECT (#312): an MCP authorization pause could not be resumed at all.
 *
 * `continueHitl`/`continueTokenLimit` were moved onto the REST continuation
 * route in PR #613, but MCP authorization stayed on `chat_continue_predict`
 * because `agent.continue.authorization.v1` needs an
 * `authorization_request_id` and nothing read that field off the
 * `mcp_authorization_required` frame. The socket client is a no-op stub
 * whenever `vite_socket_server` is empty — which is what the shipped
 * deployment serves — so pressing "Skip Auth" wiped the card, spun the bubble
 * and reached NO transport, leaving the run paused server-side.
 *
 * Mounted through the real page because both halves were individually fine:
 * the reducer stored the frame's metadata, and the route accepted the
 * contract; only the seam between the card and the continuation call was
 * missing, and no unit test spans it.
 */
describe('ChatPage MCP authorization continuation', () => {
  const AUTH_MESSAGE_ID = 'e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44';

  it('resumes over the REST authorization contract, carrying the id off the frame', async () => {
    const eventSources = installTestEventSource();
    const originalScrollIntoView = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
    Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() });
    const continuations: { readonly contract: string | null; readonly body: unknown }[] = [];
    server.use(
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/${PROJECT}`, () =>
        HttpResponse.json(
          { id: CONVERSATION, uuid: 'conversation-uuid-5', project_id: PROJECT, name: 'First turn', participants: [] },
          { status: 201 },
        ),
      ),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/${PROJECT}/${CONVERSATION}`, () => HttpResponse.json([])),
      http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
      http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
        HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 }),
      ),
      http.post(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/:conversationUuid`, () =>
        HttpResponse.json({ execution_id: 'execution-1', events_url: `${BASE}/executions/${PROJECT}/execution-1/events` }),
      ),
      http.post(
        `${BASE}/elitea_core/continue_predict/prompt_lib/${PROJECT}/:conversationUuid`,
        async ({ request }) => {
          continuations.push({
            contract: new URL(request.url).searchParams.get('execution_contract'),
            body: await request.json(),
          });
          return HttpResponse.json({
            execution_id: 'execution-2',
            events_url: `${BASE}/executions/${PROJECT}/execution-2/events`,
          });
        },
      ),
    );

    try {
      renderAt('/chat');
      const user = userEvent.setup();
      const input = await screen.findByPlaceholderText('Type your message...');
      await user.type(input, 'First turn{Enter}');
      await waitFor(() => expect(eventSources.getOpen()).toHaveLength(1), { timeout: 5000 });

      // The run starts, then pauses on an MCP toolkit that needs OAuth. Both
      // frames carry the SAME `message_id`, which is how the reducer finds the
      // answer they belong to.
      const frame = (payload: Record<string, unknown>): string =>
        JSON.stringify({ message_id: AUTH_MESSAGE_ID, question_id: null, ...payload });
      act(() => {
        eventSources.emit('execution.node_event', frame({ type: 'agent_start', content: '' }));
      });
      act(() => {
        eventSources.emit(
          'execution.node_event',
          frame({
            type: 'mcp_authorization_required',
            content: 'Authorization required.',
            response_metadata: {
              // The identity the route resumes by. `interrupt_id` wins the
              // COALESCE the server reads it back with.
              interrupt_id: 'mcp_auth_sharepoint-1',
              tool_call_id: 'call-9',
              tool_run_id: 'run-1',
              tool_name: 'sharepoint___search',
              toolkit_type: 'mcp',
              server_url: 'https://mcp.example.com',
              authorization_servers: ['https://auth.example.com'],
              authorization_requests: [{ interrupt_id: 'mcp_auth_sharepoint-1' }],
            },
          }),
        );
      });

      await user.click(await screen.findByRole('button', { name: 'Skip Auth' }));

      await waitFor(() => expect(continuations).toHaveLength(1), { timeout: 5000 });
      expect(continuations[0]?.contract).toBe('agent.continue.authorization.v1');
      expect(continuations[0]?.body).toMatchObject({
        project_id: Number(PROJECT),
        conversation_uuid: 'conversation-uuid-5',
        message_id: AUTH_MESSAGE_ID,
        authorization_request_id: 'mcp_auth_sharepoint-1',
        authorization_action: 'skip',
        mcp_tokens: {},
        ignored_mcp_servers: [],
      });
    } finally {
      if (originalScrollIntoView) Object.defineProperty(Element.prototype, 'scrollIntoView', originalScrollIntoView);
      else Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
      eventSources.restore();
    }
  });
});

/**
 * GAP G1: the conversation surface had no way to empty itself.
 *
 * Every part of the mechanism existed and was individually tested. `ChatBox`
 * exposes `onClear` on its imperative handle; `useChatBoxActions.handleClear`
 * opens the delete-all confirmation; `useDeleteMessageAlert`'s `ALL_MESSAGES`
 * sentinel routes the confirm to `clearChat`; `clearChat` issues
 * `DELETE /elitea_core/messages/prompt_lib/{projectId}/{conversationId}`. The
 * only caller of that handle was the pipeline editor's test-chat panel, so on
 * `/chat` nothing in the chain was reachable and a user could empty a
 * conversation only by deleting one message at a time — or by deleting the
 * conversation itself.
 *
 * Mounted through the real page for the reason the attachment case above is:
 * the defect lives in the seam, and every component on either side of it was
 * already green.
 */
describe('ChatPage clear history', () => {
  /*
   * jsdom has no `scrollIntoView`, and `ChatMessageList` calls it on its own
   * end-of-list ref as soon as a transcript renders. Unstubbed, the throw
   * takes the whole subtree down and the surface falls back to its empty
   * greeting — which reads exactly like "the messages never loaded".
   */
  let originalScrollIntoView: PropertyDescriptor | undefined;
  beforeEach(() => {
    originalScrollIntoView = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
    Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() });
  });
  afterEach(() => {
    if (originalScrollIntoView) Object.defineProperty(Element.prototype, 'scrollIntoView', originalScrollIntoView);
    else Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
  });

  function chatHandlers(onDelete: (conversationId: string) => void, groups: readonly unknown[]) {
    return [
      http.get(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
        HttpResponse.json({ items: groups, total: groups.length, page: 0, page_size: 50, total_pages: 1 }),
      ),
      http.delete(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/:conversationId`, ({ params }) => {
        onDelete(String(params.conversationId));
        return new HttpResponse(null, { status: 204 });
      }),
      http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
      http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
        HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 }),
      ),
    ];
  }

  it('clears the whole transcript from the composer, and only after the confirmation', async () => {
    const cleared: string[] = [];
    server.use(...chatHandlers((id) => cleared.push(id), [
      { id: 1, uid: 'message-1', role: 'user', content: 'autotest_first question' },
      { id: 2, uid: 'message-2', role: 'user', content: 'autotest_second question' },
    ]));

    renderAt(`/chat/${CONVERSATION}`);
    const user = userEvent.setup();

    await screen.findByText('autotest_first question');
    await screen.findByText('autotest_second question');

    const clear = await screen.findByTestId('chat-clear-history');
    expect(clear).toBeEnabled();
    await user.click(clear);

    // The confirmation is the whole point of the control: `handleClear` opens
    // a dialog and only `confirmDelete` reaches the mutation. A control wired
    // straight to `clearChat` would already have deleted by this line.
    await screen.findByText('Clear chat');
    expect(cleared).toEqual([]);

    await user.click(await screen.findByRole('button', { name: 'Delete' }));

    // Addressed by the conversation's UUID, which is what the delete-all route
    // takes — the serial id names a different row family and would 404.
    await waitFor(() => expect(cleared).toEqual(['conversation-uuid-5']), { timeout: 5000 });
    await waitFor(() => expect(screen.queryByText('autotest_first question')).toBeNull(), { timeout: 5000 });
  });

  it('offers the control but refuses it on an empty transcript', async () => {
    // Not "hides it": a control that vanished on an empty chat would be
    // indistinguishable from the unwired state this closes, and the baseline's
    // own `shouldDisableClear` disables rather than removes.
    const cleared: string[] = [];
    server.use(...chatHandlers((id) => cleared.push(id), []));

    renderAt(`/chat/${CONVERSATION}`);

    const clear = await screen.findByTestId('chat-clear-history');
    expect(clear).toBeDisabled();
    fireEvent.click(clear);
    expect(screen.queryByText('Clear chat')).toBeNull();
    expect(cleared).toEqual([]);
  });
});

/**
 * GAP G2: a conversation could gain an agent, and could not gain a person.
 *
 * `AddNewUserModal` — the picker, its user search and its "Add Selected"
 * action — was ported whole and had ZERO call sites anywhere in the app.
 * `ParticipantsWrapper` has always computed a `disabledAdd` flag (playback
 * state plus the caller's `configuration.users.users.view` grant) and threaded
 * it down to a control that did not exist, so the flag decided the state of
 * nothing. Meanwhile the rail rendered a Users section and the composer's "@"
 * list offered users to address — both of which only ever showed people put
 * there by some other route.
 *
 * The assertion that discriminates is the POST BODY: the picker could open,
 * accept a selection and close while attaching nothing, and no screen
 * assertion can tell that apart from a working one.
 */
describe('ChatPage add participant', () => {
  /*
   * A selected TEAM project, distinct from the reader's personal one.
   *
   * Both halves of this flow have to agree on which project they are in, and
   * they resolve it from different places: the page reads the app-shell
   * store, while the panel and the picker read the router context (their own
   * `useSelectedProjectId`). The shared fixtures make the caller's personal
   * project the same id as the route's, and `useParticipants` deliberately
   * skips the user listing there — a personal project has nobody else in it.
   */
  afterEach(() => useSelectedProjectStore.setState({ project: null }));

  it('attaches a person picked in the participants panel, by id alone', async () => {
    useSelectedProjectStore.setState({ project: { id: PROJECT, name: 'Team' } });
    const posted: unknown[] = [];
    const removed: string[] = [];
    let attached = false;
    const userParticipant = {
      id: '31',
      entity_name: 'user',
      entity_meta: { id: '42' },
      meta: { user_name: 'autotest_teammate' },
    };

    server.use(
      /*
       * A TEAM project, not the caller's own. `useParticipants` skips the
       * user listing outright while the selected project IS the reader's
       * personal one (`projectId !== privateProjectId`) — correctly, since a
       * personal project has nobody else in it — and the shared handler above
       * makes the two the same id.
       */
      http.get(`${BASE}/social/author`, () =>
        HttpResponse.json({ id: 'u1', name: 'Ada', avatar: '', personal_project_id: '99' }),
      ),
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${CONVERSATION}`, ({ request }) => {
        detailRequests.push(request.url);
        return HttpResponse.json({
          id: CONVERSATION,
          uuid: 'conversation-uuid-5',
          name: 'A conversation',
          participants: attached ? [userParticipant] : [],
        });
      }),
      // The grant the panel's `disabledAdd` reads. Without it the control is
      // rendered disabled, which is the correct product behaviour and would
      // make this journey unable to prove anything.
      http.get(`${BASE}/auth/permissions/prompt_lib/${PROJECT}`, () =>
        HttpResponse.json([{ name: 'configuration.users.users.view', enabled: true }]),
      ),
      http.get(`${BASE}/admin/users/default/${PROJECT}`, () =>
        HttpResponse.json({
          rows: [
            { id: '42', name: 'autotest_teammate', email: 'autotest_teammate@example.test' },
            { id: '43', name: 'autotest_other', email: 'autotest_other@example.test' },
          ],
          total: 2,
        }),
      ),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/${PROJECT}/${CONVERSATION}`, async ({ request }) => {
        posted.push(await request.json());
        attached = true;
        return HttpResponse.json([userParticipant]);
      }),
      http.delete(
        `${BASE}/elitea_core/participant/prompt_lib/${PROJECT}/${CONVERSATION}/:participantId`,
        ({ params }) => {
          removed.push(String(params.participantId));
          attached = false;
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
    const add = await screen.findByTestId('participants-add-button');
    expect(add).toBeEnabled();
    await user.click(add);

    await screen.findByTestId('add-participants-dialog');
    // The search narrows the directory rather than merely accepting text: the
    // second seeded person must leave the list.
    await user.type(screen.getByTestId('add-participants-search'), 'teammate');
    await waitFor(() => expect(screen.queryByTestId('add-participant-option-43')).toBeNull());

    await user.click(await screen.findByTestId('add-participant-option-42'));
    await user.click(screen.getByTestId('add-participants-confirm'));

    // The id ALONE. The server resolves the display name from the directory
    // into `meta.user_name`; a name sent from here would put this browser's
    // idea of the person onto everyone else's screen.
    await waitFor(
      () => expect(posted).toEqual([[{ entity_name: 'user', entity_meta: { id: '42' }, entity_settings: {} }]]),
      { timeout: 5000 },
    );

    // …and the attach reaches the rail the user reads it from, through the
    // conversation re-read the mutation invalidates — not through local state
    // this page patched.
    const row = await waitFor(() => screen.getByTestId('participant-item-42'), { timeout: 5000 });
    expect(row).toHaveAccessibleName('Mention autotest_teammate');

    // ── and out again ───────────────────────────────────────────────────────
    // The users row carried NO remove control before this change: every other
    // participant type has had one on its card since the rail landed, so a
    // person could be attached and never detached from this panel. Revealed on
    // hover, exactly as the sibling cards' action bar is.
    await user.hover(row);
    fireEvent.click(await screen.findByRole('button', { name: 'Remove user' }));

    // The confirmation names the PERSON. `DeleteParticipantButton` used to
    // read `entity_meta.name` only, which a REST-stored user row does not
    // carry, so it asked for consent to remove "Participant".
    const confirmDialog = await screen.findByRole('dialog');
    expect(confirmDialog).toHaveTextContent('autotest_teammate');
    fireEvent.click(within(confirmDialog).getByRole('button', { name: 'Remove' }));

    await waitFor(() => expect(removed).toEqual(['31']), { timeout: 5000 });
    await waitFor(() => expect(screen.queryByTestId('participant-item-42')).toBeNull(), { timeout: 5000 });
  });
});
