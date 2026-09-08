/**
 * A stored canvas can be REACHED from the transcript (issue 853).
 *
 * ## Why this file exists at the composition root and nowhere lower
 *
 * Every half of this was already built and unit-tested on its own, and the
 * product still had no way to open a canvas:
 *
 *  * `features/chat-messages`' `Canvas` — the block with the open control —
 *    was ported complete and mounted by nothing.
 *  * `processes/chat`'s `useCanvasEditing` holds the editor's open state and
 *    its save, and has its own tests for both.
 *  * `ChatWithEditors` mounts `CanvasEditor` in a drawer whenever that state
 *    says a block is open.
 *
 * What was missing was the WIRING: the answer renderer filtered
 * `message_items` down to `text_message`, so a `canvas_message` item reached
 * no component at all, and no click anywhere on the chat surface could set
 * the state the drawer reads. That is the shape this repository keeps meeting
 * — both halves correct, the composition root empty — and a test that mounts
 * either half alone cannot state it. So this one mounts the REAL route:
 * `ChatWithEditors` -> `ChatPage` -> `ChatBox` -> `ChatMessageList` ->
 * `ApplicationAnswer` -> `AnswerMessageItems` -> `Canvas`, over a real
 * conversation read whose answer carries a canvas item in the shape the
 * server actually serves it.
 *
 * ## What it asserts, and why each one is a separate claim
 *
 *  1. The canvas item RENDERS: its stored document and its name are on the
 *     screen. Before, the item was dropped silently — the reader saw the two
 *     halves of their answer with the middle missing and no error.
 *  2. The text around it survives, IN ORDER. The create route rewrites one
 *     text item into text / canvas / text, so a renderer that appended the
 *     canvas after the text would show the answer rearranged.
 *  3. Clicking the open control OPENS THE EDITOR, with THIS item's document
 *     in it. A control that flips a flag but hands the editor the wrong
 *     document is the same lost-edit failure by another route, so the
 *     assertion is on the text in the editor, not on the drawer appearing.
 *  4. The opened block is replaced by the editing placeholder in the
 *     transcript, which is what stops the same document being shown twice
 *     and edited in the copy that cannot save.
 */
import type { ReactNode } from 'react';

import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { configure, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource } from '@/shared/api/sse/testing';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { ChatWithEditors } from './ChatWithEditors';

// Mounting a whole route through msw is slow enough that the library's 1 s
// default expires before the first paint on a loaded machine.
configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

const BASE = '/api/v2';
const PROJECT = '77';
const CONVERSATION = 41;

/** The document the canvas holds, and the words that must stay on either side of it. */
const CANVAS_UUID = 'b2a1f0de-0000-4000-8000-000000000001';
const CANVAS_NAME = 'Edit code';
const CANVAS_DOCUMENT = "print('autotest canvas opener')";
const HEAD_TEXT = 'AUTOTESTHEAD before the block';
const TAIL_TEXT = 'AUTOTESTTAIL after the block';

/**
 * The canvas item, in the shape the repository projects it
 * (`canvasItemDetails`): the flat keys AND `latest_version`, with the item's
 * own uuid INSIDE the details rather than beside `item_type`. Written out in
 * full rather than trimmed to what the renderer happens to read, so a renderer
 * that starts reading a different key still meets real data.
 */
const CANVAS_ITEM = {
  id: 9102,
  item_type: 'canvas_message',
  order_index: 1,
  item_details: {
    id: 9102,
    uuid: CANVAS_UUID,
    item_type: 'canvas_message',
    name: CANVAS_NAME,
    canvas_type: 'code',
    canvas_content: CANVAS_DOCUMENT,
    code_language: 'python',
    editors: [],
    latest_version: { id: 5, canvas_content: CANVAS_DOCUMENT, code_language: 'python', created_at: '2026-09-08T10:00:06Z' },
  },
};

/**
 * The transcript, as the route the chat page ACTUALLY reads answers it.
 *
 * This is the paginated messages route, not the conversation-details one, and
 * the difference is the whole reason this fixture is shaped the way it is: the
 * page's rows come from here (`useChatPageData`), the route collapses a
 * group's `text_message` items into `content` and serves NO text item, and it
 * serves the non-text items — attachments, and now canvases — in
 * `message_items`. A fixture built from the details read would have proved the
 * renderer works on a payload the page never receives.
 */
function messagesBody(): Record<string, unknown> {
  return {
    items: [
      {
        id: 900,
        uid: 'group-user-1',
        role: 'user',
        content: 'make me a canvas',
        created_at: '2026-09-08T10:00:00Z',
      },
      {
        id: 901,
        uid: 'group-answer-1',
        role: 'assistant',
        content: `${HEAD_TEXT}\n${TAIL_TEXT}`,
        created_at: '2026-09-08T10:00:05Z',
        message_items: [CANVAS_ITEM],
      },
    ],
    total: 2,
    page: 0,
    page_size: 50,
    total_pages: 1,
  };
}

function chatHandlers() {
  return [
    http.get(`${BASE}/auth/permissions/prompt_lib/${PROJECT}`, () =>
      HttpResponse.json([{ name: PERMISSIONS.chat.folders.get, enabled: true }]),
    ),
    http.get(`${BASE}/social/author`, () =>
      HttpResponse.json({ id: 'u1', name: 'Ada', avatar: '', personal_project_id: PROJECT }),
    ),
    http.get(`${BASE}/elitea_core/folder/prompt_lib/${PROJECT}`, () =>
      HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [], total_folders: 0 }),
    ),
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
      HttpResponse.json({ id: String(CONVERSATION), uuid: 'conversation-uuid-41', name: 'A conversation with a canvas', participants: [] }),
    ),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/${CONVERSATION}`, () => HttpResponse.json(messagesBody())),
    http.get(`${BASE}/configurations/models/${PROJECT}`, () =>
      HttpResponse.json({ items: [{ id: 'model-1', name: 'model-1', project_id: PROJECT, default: true }] }),
    ),
    http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
    http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/:conversationId`, () =>
      HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 }),
    ),
  ];
}

/** The real `/chat/$conversationId` route, rendering the real composition root. */
function renderChatRoute(): void {
  const rootRoute = createRootRoute({ component: (): ReactNode => <Outlet /> });
  const chatRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: () => <ChatWithEditors /> });
  const conversationRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/chat/$conversationId',
    component: () => <ChatWithEditors />,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([chatRoute, conversationRoute]),
    history: createMemoryHistory({ initialEntries: [`/chat/${String(CONVERSATION)}`] }),
  });
  render(
    <AppProviders>
      <RouterProvider router={router as never} />
    </AppProviders>,
  );
}

let restoreScrollIntoView: (() => void) | undefined;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: { id: PROJECT, name: 'Project' } });
  server.use(...chatHandlers());
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

describe('the chat route: a stored canvas has an opener', () => {
  it('renders the canvas block in the answer, beside the words the answer keeps', async () => {
    const eventSources = installTestEventSource();
    try {
      renderChatRoute();

      const block = await screen.findByTestId('canvas-block', {}, { timeout: 15_000 });
      expect(within(block).getByTestId('canvas-block-title')).toHaveTextContent(CANVAS_NAME);
      expect(within(block).getByTestId('canvas-block-content')).toHaveTextContent(CANVAS_DOCUMENT);

      // The words on either side of the canvas are still on screen. On this
      // route they arrive as the group's aggregated `content`, and the rule
      // that renders it — "only when no TEXT item states it" — is the one a
      // canvas-only item list breaks if it is written as "only when there are
      // no items at all". That is why both are asserted together.
      // `application-answer`, not `chat-answer-content`: the LAST row's body
      // is `skill-test-last-response` instead, and this fixture's answer is
      // the last row.
      const answer = screen.getByTestId('application-answer');
      const rendered = answer.textContent ?? '';
      expect(rendered).toContain(HEAD_TEXT);
      expect(rendered).toContain(TAIL_TEXT);
      expect(rendered).toContain(CANVAS_DOCUMENT);
    } finally {
      eventSources.restore();
    }
  });

  it('opens the canvas editor on this item, with this item’s document in it', async () => {
    const eventSources = installTestEventSource();
    try {
      renderChatRoute();

      const block = await screen.findByTestId('canvas-block', {}, { timeout: 15_000 });
      const open = within(block).getByTestId('canvas-block-open');
      expect(open, 'a stored canvas must offer an open control').toBeEnabled();

      const user = userEvent.setup();
      await user.click(open);

      // The drawer, and the document IN it — not merely the drawer. An opener
      // that reaches the editor with the wrong (or an empty) document is the
      // lost-edit failure this whole area exists to stop.
      const drawer = await screen.findByTestId('chat-canvas-editor', {}, { timeout: 15_000 });
      await waitFor(() => {
        expect(drawer.textContent ?? '').toContain(CANVAS_DOCUMENT);
      });

      // …and the transcript stops showing a second, unsaveable copy of it.
      await waitFor(() => {
        expect(screen.getByTestId('editing-placeholder')).toBeInTheDocument();
      });
      expect(screen.queryByTestId('canvas-block-content')).not.toBeInTheDocument();
    } finally {
      eventSources.restore();
    }
  });
});
