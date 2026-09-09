/**
 * The four canvas gestures the streaming journey's own header used to list as
 * "not deterministic / no control on this app", stated where a control, a
 * request and a document can be observed as one gesture: the REAL chat route.
 *
 * `ChatWithEditors` -> `ChatPage` -> `ChatBox` -> `ChatMessageList` ->
 * `ApplicationAnswer` -> `AnswerContent` -> the transcript, and the canvas
 * editor drawer beside it.
 *
 *  1. CREATE FROM A SELECTION. Highlighting part of an answer offers "Create
 *     canvas", and the request it sends carries the BYTE range the server
 *     splits on. The range is the whole claim: the offsets go into a Go string
 *     slice, so a client that sent JavaScript character indexes would carve a
 *     range the route ACCEPTS and the reader did not choose. This case runs
 *     over an answer with multibyte text in front of the selection, where the
 *     two numbers differ.
 *  2. FULL SCREEN. The editor is a drawer with no expand control at all; it
 *     has one now, and Escape collapses it. Escape is the interesting half —
 *     the drawer closes on Escape by default, and closing this drawer SAVES,
 *     so one press must not both collapse the editor and end the session.
 *  3. TABLE EXPORT. A table canvas can leave as a file. Asserted on the BYTES
 *     handed to the download primitive, not on a control that was clicked: an
 *     export control wired to nothing looks identical from the outside.
 *  4. KEYBOARD UNDO/REDO. The header buttons were the only way to undo, and
 *     the table pane has no keymap of its own, so `Mod-z` in a table canvas
 *     did nothing whatsoever.
 *
 * Every one of these is a composition-root claim rather than a component one:
 * the halves each had passing tests before, and what was missing was the
 * wiring between them — the shape this repository keeps meeting.
 */
import type { ReactNode } from 'react';

import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { configure, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource } from '@/shared/api/sse/testing';
import { useEditorStateStore } from '@/shared/lib/editorState';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { ChatWithEditors } from './ChatWithEditors';

configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 120_000 });

installCodeMirrorTestPolyfills();

const BASE = '/api/v2';
const PROJECT = '77';
const CONVERSATION = 41;
const GROUP_ID = 901;
const GROUP_UUID = 'group-answer-1';
const TEXT_ITEM_ID = 9101;

/**
 * The answer the canvas is carved out of, with multibyte text BEFORE the
 * selection so a character index and a byte offset cannot agree.
 * `Résumé — δelta` is 14 characters and 18 bytes.
 */
const ANSWER_TEXT = 'Résumé — δelta then autotest carve me here and a tail';
const SELECTED = 'carve me here';

/** The expected offsets, measured with Node's own encoder rather than the app's. */
const EXPECTED_STARTS_AT = Buffer.byteLength(ANSWER_TEXT.slice(0, ANSWER_TEXT.indexOf(SELECTED)), 'utf8');
const EXPECTED_ENDS_AT = EXPECTED_STARTS_AT + Buffer.byteLength(SELECTED, 'utf8');

const CODE_CANVAS_UUID = 'b2a1f0de-0000-4000-8000-000000000011';
const CODE_DOCUMENT = "print('autotest canvas gaps')";
const TABLE_CANVAS_UUID = 'b2a1f0de-0000-4000-8000-000000000012';
const TABLE_DOCUMENT = '| Metric | Value |\n| --- | --- |\n| alpha | 1 |\n';

/** One canvas item, in the shape the repository projects it. */
function canvasItem(options: { uuid: string; name: string; type: string; language: string; content: string }): Record<string, unknown> {
  return {
    id: 9102,
    item_type: 'canvas_message',
    order_index: 1,
    item_details: {
      id: 9102,
      uuid: options.uuid,
      item_type: 'canvas_message',
      name: options.name,
      canvas_type: options.type,
      canvas_content: options.content,
      code_language: options.language,
      editors: [],
      latest_version: { id: 5, canvas_content: options.content, code_language: options.language, created_at: '2026-09-08T10:00:06Z' },
    },
  };
}

const CODE_CANVAS = canvasItem({
  uuid: CODE_CANVAS_UUID,
  name: 'Edit code',
  type: 'code',
  language: 'python',
  content: CODE_DOCUMENT,
});
const TABLE_CANVAS = canvasItem({
  uuid: TABLE_CANVAS_UUID,
  name: 'Edit table',
  type: 'table',
  language: 'markdown',
  content: TABLE_DOCUMENT,
});

/** What the transcript route serves right now — mutated by the create in case 1. */
let answerItems: readonly Record<string, unknown>[] = [];
/** Every create body the route received, so the RANGE can be read rather than assumed. */
let createBodies: Record<string, unknown>[] = [];

/**
 * The paginated messages route — the read the chat page actually makes. It
 * collapses a group's text into `content` and serves no text item, which is
 * exactly why the create has to resolve the row it splits from the details
 * read instead.
 */
function messagesBody(): Record<string, unknown> {
  return {
    items: [
      { id: 900, uid: 'group-user-1', role: 'user', content: 'make me a canvas', created_at: '2026-09-08T10:00:00Z' },
      {
        id: GROUP_ID,
        uid: GROUP_UUID,
        role: 'assistant',
        content: ANSWER_TEXT,
        created_at: '2026-09-08T10:00:05Z',
        message_items: answerItems,
      },
    ],
    total: 2,
    page: 0,
    page_size: 50,
    total_pages: 1,
  };
}

/** The conversation-details read, which DOES carry the text item and its id. */
function detailsBody(): Record<string, unknown> {
  return {
    id: String(CONVERSATION),
    uuid: 'conversation-uuid-41',
    name: 'A conversation with a canvas',
    participants: [],
    message_groups: [
      {
        id: GROUP_ID,
        uuid: GROUP_UUID,
        content: ANSWER_TEXT,
        message_items: [
          { id: TEXT_ITEM_ID, item_type: 'text_message', item_details: { content: ANSWER_TEXT } },
          ...answerItems.filter((item) => item['item_type'] === 'canvas_message'),
        ],
      },
    ],
  };
}

function chatHandlers() {
  return [
    http.get(`${BASE}/auth/permissions/prompt_lib/${PROJECT}`, () =>
      HttpResponse.json([{ name: PERMISSIONS.chat.folders.get, enabled: true }]),
    ),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 'u1', name: 'Ada', avatar: '', personal_project_id: PROJECT })),
    http.get(`${BASE}/elitea_core/folder/prompt_lib/${PROJECT}`, () =>
      HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [], total_folders: 0 }),
    ),
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${CONVERSATION}`, () => HttpResponse.json(detailsBody())),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/${CONVERSATION}`, () => HttpResponse.json(messagesBody())),
    http.get(`${BASE}/configurations/models/${PROJECT}`, () =>
      HttpResponse.json({ items: [{ id: 'model-1', name: 'model-1', project_id: PROJECT, default: true }] }),
    ),
    http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
    http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/:conversationId`, () =>
      HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 }),
    ),
    http.post(`${BASE}/elitea_core/canvases/prompt_lib/${PROJECT}`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      createBodies.push(body);
      // The server SPLITS the item and the transcript then serves a canvas
      // where the words were. Nothing is spliced client-side; the next read is
      // what the block comes from.
      answerItems = [canvasItem({ uuid: CODE_CANVAS_UUID, name: 'Edit code', type: 'code', language: 'markdown', content: SELECTED })];
      return HttpResponse.json({ uuid: CODE_CANVAS_UUID, id: 9102, canvas_content: SELECTED });
    }),
    http.post(`${BASE}/elitea_core/canvas/prompt_lib/:projectId/:canvasId/presence`, () =>
      HttpResponse.json({
        project_id: PROJECT,
        entity_id: CODE_CANVAS_UUID,
        entity_type: 'canvas',
        action: 'editors',
        canvas_uuid: CODE_CANVAS_UUID,
        message_group_uuid: GROUP_UUID,
        editors: [{ user_id: 'u1', user_name: 'ada@corp.internal', state: 'editing' }],
        ttl_seconds: 120,
      }),
    ),
    http.put(`${BASE}/elitea_core/canvas/prompt_lib/:projectId/:canvasId`, () => HttpResponse.json({ ok: true })),
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
    // The signed-in principal, so the editor recognises its OWN entry in the
    // presence roster and does not mount read-only against its only reader.
    context: { auth: { getUser: () => ({ id: 'u1' }) } },
  });
  render(
    <AppProviders>
      <RouterProvider router={router as never} />
    </AppProviders>,
  );
}

/** Highlights `text` inside the rendered answer, the way a drag would. */
function selectInAnswer(text: string): void {
  const item = screen.getByTestId('answer-text-item');
  const walker = document.createTreeWalker(item, NodeFilter.SHOW_TEXT);
  let node = walker.nextNode();
  while (node !== null) {
    const offset = (node.textContent ?? '').indexOf(text);
    if (offset >= 0) {
      const range = document.createRange();
      range.setStart(node, offset);
      range.setEnd(node, offset + text.length);
      const selection = document.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      // The selection is READ on the events a browser fires around one. The
      // subscription is the half that can silently never run.
      fireEvent.mouseUp(document);
      return;
    }
    node = walker.nextNode();
  }
  throw new Error(`the rendered answer holds no text node containing ${text}`);
}

let restoreScrollIntoView: (() => void) | undefined;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: { id: PROJECT, name: 'Project' } });
  answerItems = [];
  createBodies = [];
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
  // A module singleton: a test that leaves the canvas open hands the next one
  // a mutex that already believes an editor is up.
  useEditorStateStore.getState().setEditingCanvas(false);
  useSelectedProjectStore.setState({ project: null });
  resetGeneratedClient();
});

describe('the chat route: a canvas can be MADE from a selection', () => {
  it('offers no create control until something is highlighted', async () => {
    const eventSources = installTestEventSource();
    try {
      renderChatRoute();
      await screen.findByTestId('answer-text-item', {}, { timeout: 15_000 });
      expect(screen.queryByTestId('canvas-create-from-selection')).not.toBeInTheDocument();
    } finally {
      eventSources.restore();
    }
  });

  it('carves the highlighted range out, in BYTES, and the transcript re-renders with the block', async () => {
    const eventSources = installTestEventSource();
    try {
      renderChatRoute();
      await screen.findByTestId('answer-text-item', {}, { timeout: 15_000 });

      selectInAnswer(SELECTED);
      const create = await screen.findByTestId('canvas-create-from-selection', {}, { timeout: 10_000 });

      const user = userEvent.setup();
      await user.click(create);

      await waitFor(() => {
        expect(createBodies, 'the click must reach the create route').toHaveLength(1);
      });
      const body = createBodies[0] ?? {};
      expect(body['message_group_id'], 'the create takes the group ROW id, not the uuid the transcript renders').toBe(GROUP_ID);
      expect(body['message_item_id'], 'and the text item the split rewrites, resolved from the details read').toBe(TEXT_ITEM_ID);
      // THE CLAIM. `ANSWER_TEXT.indexOf(SELECTED)` is smaller than this: the
      // accented characters and the em dash in front of the selection cost
      // more bytes than they do characters, and the route would have accepted
      // the smaller number and split the answer in the wrong place.
      expect(ANSWER_TEXT.indexOf(SELECTED), 'the character index a naive client would send').toBeLessThan(EXPECTED_STARTS_AT);
      expect(body['canvas_content_starts_at']).toBe(EXPECTED_STARTS_AT);
      expect(body['canvas_content_ends_at']).toBe(EXPECTED_ENDS_AT);
      // Issue #879: `SELECTED` ('carve me here') is prose, not a fenced code
      // block — the create now asks for a `document` canvas, not `code`.
      expect(body['canvas_type'], 'a prose selection creates a document canvas, not a code one (#879)').toBe('document');
      expect(body['code_language']).toBe('document');
      expect(body['name']).toBe('Edit document');

      // …and the transcript shows what the server wrote, without a reload.
      const block = await screen.findByTestId('canvas-block', {}, { timeout: 15_000 });
      expect(within(block).getByTestId('canvas-block-content')).toHaveTextContent(SELECTED);
    } finally {
      eventSources.restore();
    }
  });
});

describe('the chat route: the canvas editor fills the screen and comes back', () => {
  it('expands on the control and collapses on Escape, without ending the editing session', async () => {
    answerItems = [CODE_CANVAS];
    const eventSources = installTestEventSource();
    try {
      renderChatRoute();
      const block = await screen.findByTestId('canvas-block', {}, { timeout: 15_000 });
      const user = userEvent.setup();
      await user.click(within(block).getByTestId('canvas-block-open'));

      const drawer = await screen.findByTestId('chat-canvas-editor', {}, { timeout: 15_000 });
      const root = await screen.findByTestId('canvas-editor-root', {}, { timeout: 15_000 });
      expect(root).toHaveAttribute('data-fullscreen', 'false');

      await user.click(screen.getByTestId('canvas-edit-fullscreen'));
      await waitFor(() => {
        expect(screen.getByTestId('canvas-editor-root')).toHaveAttribute('data-fullscreen', 'true');
      });

      fireEvent.keyDown(screen.getByTestId('canvas-editor-root'), { key: 'Escape' });
      await waitFor(() => {
        expect(screen.getByTestId('canvas-editor-root')).toHaveAttribute('data-fullscreen', 'false');
      });
      // The editing session survives it. The drawer closes on Escape by
      // default and this drawer's close is the canvas SAVE, so a press that
      // did both would end the edit the reader was only resizing.
      expect(drawer).toBeInTheDocument();
      expect(screen.getByTestId('canvas-editor-root')).toBeInTheDocument();
    } finally {
      eventSources.restore();
    }
  });

  it('starts the NEXT canvas at the normal size — the mode belongs to the session, not the document', async () => {
    answerItems = [CODE_CANVAS];
    const eventSources = installTestEventSource();
    try {
      renderChatRoute();
      const block = await screen.findByTestId('canvas-block', {}, { timeout: 15_000 });
      const user = userEvent.setup();
      await user.click(within(block).getByTestId('canvas-block-open'));
      await screen.findByTestId('canvas-editor-root', {}, { timeout: 15_000 });
      await user.click(screen.getByTestId('canvas-edit-fullscreen'));
      await waitFor(() => {
        expect(screen.getByTestId('canvas-editor-root')).toHaveAttribute('data-fullscreen', 'true');
      });

      await user.click(screen.getByTestId('canvas-edit-close'));
      await waitFor(() => {
        expect(screen.queryByTestId('canvas-editor-root')).not.toBeInTheDocument();
      });

      await user.click(within(await screen.findByTestId('canvas-block')).getByTestId('canvas-block-open'));
      const reopened = await screen.findByTestId('canvas-editor-root', {}, { timeout: 15_000 });
      expect(reopened).toHaveAttribute('data-fullscreen', 'false');
    } finally {
      eventSources.restore();
    }
  });
});

describe('the chat route: a table canvas can be exported', () => {
  /** Captures whatever `shared/lib/download` hands the browser. */
  function captureDownloads(): { readonly files: { name: string; blob: Blob }[]; restore: () => void } {
    const files: { name: string; blob: Blob }[] = [];
    let pending: Blob | undefined;
    const originalCreate = Object.getOwnPropertyDescriptor(URL, 'createObjectURL');
    const originalRevoke = Object.getOwnPropertyDescriptor(URL, 'revokeObjectURL');
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true,
      value: (blob: Blob) => {
        pending = blob;
        return 'blob:test';
      },
    });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: () => undefined });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function mockClick(this: HTMLAnchorElement) {
      if (pending !== undefined) files.push({ name: this.download, blob: pending });
    });
    return {
      files,
      restore: () => {
        click.mockRestore();
        if (originalCreate) Object.defineProperty(URL, 'createObjectURL', originalCreate);
        if (originalRevoke) Object.defineProperty(URL, 'revokeObjectURL', originalRevoke);
      },
    };
  }

  it('offers CSV and XLSX, and the CSV it saves is this grid’s own rows', async () => {
    answerItems = [TABLE_CANVAS];
    const eventSources = installTestEventSource();
    const downloads = captureDownloads();
    try {
      renderChatRoute();
      const block = await screen.findByTestId('canvas-block', {}, { timeout: 15_000 });
      const user = userEvent.setup();
      await user.click(within(block).getByTestId('canvas-block-open'));
      await screen.findByTestId('chat-table-canvas-grid', {}, { timeout: 15_000 });

      await user.click(screen.getByTestId('canvas-table-export'));
      // Both formats are offered — the reference had both, and shipping one
      // silently is the "disclosed gap that reads as a feature" shape.
      expect(await screen.findByTestId('canvas-table-export-xlsx')).toBeInTheDocument();
      await user.click(screen.getByTestId('canvas-table-export-csv'));

      await waitFor(() => {
        expect(downloads.files, 'the export control must reach the download primitive').toHaveLength(1);
      });
      const file = downloads.files[0];
      expect(file?.name).toBe('table.csv');
      const text = await file?.blob.text();
      expect(text).toContain('Metric,Value');
      expect(text).toContain('alpha,1');
    } finally {
      downloads.restore();
      eventSources.restore();
    }
  });
});

describe('the chat route: undo and redo answer the keyboard', () => {
  it('the TABLE pane, which has no keymap of its own: Mod-z reverses an added row and Mod-Shift-z puts it back', async () => {
    answerItems = [TABLE_CANVAS];
    const eventSources = installTestEventSource();
    try {
      renderChatRoute();
      const block = await screen.findByTestId('canvas-block', {}, { timeout: 15_000 });
      const user = userEvent.setup();
      await user.click(within(block).getByTestId('canvas-block-open'));
      const grid = await screen.findByTestId('chat-table-canvas-grid', {}, { timeout: 15_000 });

      const rowsBefore = within(grid).getAllByRole('row').length;
      await user.click(screen.getByRole('button', { name: 'Add row' }));
      await waitFor(() => {
        expect(within(grid).getAllByRole('row').length).toBeGreaterThan(rowsBefore);
      });

      // Dispatched FROM the grid, which is where a reader's hands are. The
      // handler is on the editor root, so this is the binding under test —
      // and before it existed this press did nothing at all.
      fireEvent.keyDown(grid, { key: 'z', ctrlKey: true });
      await waitFor(() => {
        expect(within(grid).getAllByRole('row').length).toBe(rowsBefore);
      });

      fireEvent.keyDown(grid, { key: 'z', ctrlKey: true, shiftKey: true });
      await waitFor(() => {
        expect(within(grid).getAllByRole('row').length).toBeGreaterThan(rowsBefore);
      });

      // The Windows spelling of redo reaches the same history.
      fireEvent.keyDown(grid, { key: 'z', ctrlKey: true });
      await waitFor(() => {
        expect(within(grid).getAllByRole('row').length).toBe(rowsBefore);
      });
      fireEvent.keyDown(grid, { key: 'y', ctrlKey: true });
      await waitFor(() => {
        expect(within(grid).getAllByRole('row').length).toBeGreaterThan(rowsBefore);
      });
    } finally {
      eventSources.restore();
    }
  });

  it('the CODE pane, from anywhere the pane’s own keymap cannot hear: Mod-z reverses the typing', async () => {
    answerItems = [CODE_CANVAS];
    const eventSources = installTestEventSource();
    try {
      renderChatRoute();
      const block = await screen.findByTestId('canvas-block', {}, { timeout: 15_000 });
      const user = userEvent.setup();
      await user.click(within(block).getByTestId('canvas-block-open'));
      const root = await screen.findByTestId('canvas-editor-root', {}, { timeout: 15_000 });

      const content = root.querySelector('.cm-content');
      if (!(content instanceof HTMLElement)) throw new Error('the canvas editor mounted no code pane');
      await user.click(content);
      await user.keyboard('XYZ');
      await waitFor(() => {
        expect(content.textContent ?? '').toContain(`XYZ${CODE_DOCUMENT}`);
      });

      // Pressed with the focus OUTSIDE CodeMirror — on the header, where the
      // pane's own `historyKeymap` never sees it. This is the case the toolbar
      // buttons covered and the keyboard did not.
      fireEvent.keyDown(screen.getByTestId('canvas-edit-undo'), { key: 'z', ctrlKey: true });
      await waitFor(() => {
        expect(content.textContent ?? '').not.toContain('XYZ');
      });
    } finally {
      eventSources.restore();
    }
  });
});
