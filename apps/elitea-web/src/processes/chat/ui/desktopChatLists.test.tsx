/**
 * The conversation lists of the chat surface inside the desktop frame (the
 * composition root the desktop app mounts: `DesktopFrame` around
 * `ChatWithEditors`).
 *
 * There is always one list on screen: the sidebar's Chats rows while they are
 * mounted, else the page's own list (the sidebar hidden, or its Chats
 * disclosure collapsed). Opening the chat expands the Elitea section and the
 * Chats disclosure. The sidebar manages folders with the web list's hooks
 * (New folder, Move to folder…). Both lists read one cached listing, so a
 * conversation the page creates reaches the sidebar, and a rename in one list
 * reaches the other.
 */
import type { ReactNode } from 'react';

import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { WorkspaceIpcProvider } from '@/features/workspace';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { BROWSER_PLATFORM } from '@/shared/desktop/appEvents';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { clearNamespace } from '@/shared/lib/storage';
import { server } from '@/test/setup';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { DesktopFrame, useDesktopLayout } from '@/widgets/desktop-shell';

import { ChatWithEditors } from './ChatWithEditors';

const BASE = '/api/v2';
const PROJECT = '77';
const EXISTING = 41;
const CREATED = 42;
const globals = globalThis as unknown as Record<string, unknown>;

interface StoredConversation {
  id: number;
  name: string;
  folderId: string | null;
}

let conversations: StoredConversation[];
let folders: { id: string; name: string }[];
let folderPosts: unknown[];
let conversationPuts: { id: string; body: Record<string, unknown> }[];

const wireRow = (c: StoredConversation, at: string) => ({ id: String(c.id), name: c.name, is_private: true, author_id: 1, updated_at: at, created_at: at });

function listing() {
  const at = new Date().toISOString();
  return {
    pinned: { conversations: [] },
    date_groups: [{ name: 'Today', conversations: conversations.filter((c) => c.folderId === null).map((c) => wireRow(c, at)) }],
    folders: folders.map((folder) => ({ ...folder, conversations: conversations.filter((c) => c.folderId === folder.id).map((c) => wireRow(c, at)) })),
    total_folders: folders.length,
  };
}

function conversationHandlers(id: number) {
  return [
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${id}`, () =>
      HttpResponse.json({ id: String(id), uuid: `conversation-uuid-${String(id)}`, name: conversations.find((c) => c.id === id)?.name ?? '', participants: [] }),
    ),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/${id}`, () => HttpResponse.json({ items: [], total: 0, page: 0, page_size: 50, total_pages: 1 })),
  ];
}

function handlers() {
  return [
    http.get(`${BASE}/auth/permissions/prompt_lib/${PROJECT}`, () =>
      HttpResponse.json([
        { name: PERMISSIONS.chat.folders.get, enabled: true },
        { name: PERMISSIONS.chat.folders.create, enabled: true },
      ]),
    ),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 1, name: 'Ada', avatar: '', personal_project_id: PROJECT })),
    http.get(`${BASE}/elitea_core/folder/prompt_lib/${PROJECT}`, () => HttpResponse.json(listing())),
    http.post(`${BASE}/elitea_core/folder/prompt_lib/${PROJECT}`, async ({ request }) => {
      const body = (await request.json()) as { name: string };
      folderPosts.push(body);
      const folder = { id: `f${String(folders.length + 1)}`, name: body.name };
      folders = [folder, ...folders];
      return HttpResponse.json({ ...folder, conversations: [] });
    }),
    http.put(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/:id`, async ({ params, request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      const id = String(params['id']);
      conversationPuts.push({ id, body });
      conversations = conversations.map((c) =>
        String(c.id) !== id
          ? c
          : {
              ...c,
              ...(typeof body['name'] === 'string' ? { name: body['name'] } : {}),
              ...('folder_id' in body ? { folderId: body['folder_id'] === null ? null : String(body['folder_id'] as string | number) } : {}),
            },
      );
      return HttpResponse.json({ id, name: conversations.find((c) => String(c.id) === id)?.name });
    }),
    http.post(`${BASE}/elitea_core/conversations/prompt_lib/${PROJECT}`, () => {
      conversations = [{ id: CREATED, name: 'First turn', folderId: null }, ...conversations];
      return HttpResponse.json({ id: CREATED, uuid: 'conversation-uuid-42', project_id: PROJECT, name: 'First turn', participants: [] }, { status: 201 });
    }),
    http.post(`${BASE}/elitea_core/participants/prompt_lib/${PROJECT}/${CREATED}`, () => HttpResponse.json([])),
    ...conversationHandlers(EXISTING),
    ...conversationHandlers(CREATED),
    http.post(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/:conversationUuid`, () =>
      HttpResponse.json({ execution_id: 'execution-1', events_url: `${BASE}/executions/${PROJECT}/execution-1/events` }),
    ),
    http.get(`${BASE}/configurations/models/${PROJECT}`, () => HttpResponse.json({ items: [{ id: 'model-1', name: 'model-1', project_id: PROJECT, default: true }] })),
    http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
    http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/:conversationId`, () => HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 })),
  ];
}

function mount(initialEntry = `/chat/${String(EXISTING)}`) {
  const frame = (children: ReactNode): ReactNode => (
    <DesktopFrame
      permissions={new Set([PERMISSIONS.chat.folders.get, PERMISSIONS.chat.folders.create])}
      projects={[{ id: Number(PROJECT), name: 'Platform', suspended: false }]}
      selectedProjectId={PROJECT}
      onSelectProject={() => undefined}
    >
      {children}
    </DesktopFrame>
  );
  const rootRoute = createRootRoute({ component: (): ReactNode => frame(<Outlet />) });
  const surface = (): ReactNode => <ChatWithEditors />;
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: surface }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: surface }),
    ]),
    history: createMemoryHistory({ initialEntries: [initialEntry] }),
  });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={createFakeWorkspaceIpc({ workspaces: [] })}>
        <RouterProvider router={router as never} />
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
  return router;
}

const sidebarRow = (name: string): HTMLElement | undefined => screen.queryAllByTestId('shell-conversation').find((node) => node.textContent === name);

async function openRowMenu(user: ReturnType<typeof userEvent.setup>, name: string): Promise<void> {
  await waitFor(() => expect(sidebarRow(name)).toBeDefined(), { timeout: 10_000 });
  await user.click(within(sidebarRow(name) as HTMLElement).getByTestId('shell-conversation-more'));
}

let restoreScrollIntoView: (() => void) | undefined;
let eventSources: ReturnType<typeof installTestEventSource>;

beforeEach(() => {
  conversations = [{ id: EXISTING, name: 'Release notes', folderId: null }];
  folders = [];
  folderPosts = [];
  conversationPuts = [];
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  clearNamespace();
  eventSources = installTestEventSource();
  useSelectedProjectStore.setState({ project: { id: PROJECT, name: 'Platform' } });
  useDesktopLayout.setState({ sidebarOpen: true, changesOpen: true, paletteOpen: false, hostMenu: false, notice: null, session: null, platform: BROWSER_PLATFORM });
  server.use(...handlers());
  const original = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
  Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() });
  restoreScrollIntoView = () => {
    if (original) Object.defineProperty(Element.prototype, 'scrollIntoView', original);
    else Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
  };
});

afterEach(() => {
  restoreScrollIntoView?.();
  eventSources.restore();
  useSelectedProjectStore.setState({ project: null });
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('the desktop chat: one list of conversations on screen', () => {
  it('shows the page list while the sidebar is hidden, and drops it while the sidebar list is mounted', async () => {
    mount();
    expect(await screen.findByTestId('shell-chats', {}, { timeout: 10_000 })).toBeInTheDocument();
    expect(screen.queryByTestId('chat-conversation-sidebar')).not.toBeInTheDocument();

    act(() => useDesktopLayout.getState().setSidebarOpen(false));
    expect(await screen.findByTestId('chat-conversation-sidebar', {}, { timeout: 10_000 })).toBeInTheDocument();
    expect(screen.queryByTestId('shell-chats')).not.toBeInTheDocument();

    act(() => useDesktopLayout.getState().setSidebarOpen(true));
    await waitFor(() => expect(screen.queryByTestId('chat-conversation-sidebar')).not.toBeInTheDocument());
    expect(screen.getByTestId('shell-chats')).toBeInTheDocument();
  });

  it('expands a collapsed Elitea section and Chats disclosure on opening the chat; collapsing Chats there brings the page list back', async () => {
    window.localStorage.setItem('el.desktop.eliteaOpen', 'false');
    window.localStorage.setItem('el.desktop.chatsOpen', 'false');
    const user = userEvent.setup();
    mount();

    expect(await screen.findByTestId('shell-chats', {}, { timeout: 10_000 })).toBeInTheDocument();
    expect(screen.getByTestId('shell-elitea-help')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-conversation-sidebar')).not.toBeInTheDocument();

    await user.click(screen.getByTestId('shell-chats-toggle'));
    await waitFor(() => expect(screen.queryByTestId('shell-chats')).not.toBeInTheDocument());
    expect(await screen.findByTestId('chat-conversation-sidebar', {}, { timeout: 10_000 })).toBeInTheDocument();
  });
});

describe('the desktop chat: folders in the sidebar', () => {
  it('creates a folder from the Chats row', async () => {
    const user = userEvent.setup();
    mount();
    await screen.findByTestId('shell-chats', {}, { timeout: 10_000 });

    fireEvent.click(screen.getByTestId('shell-chats-new-folder'));
    await user.type(await screen.findByTestId('shell-chats-new-folder-name'), 'Specs{Enter}');

    await waitFor(() => expect(folderPosts).toEqual([{ name: 'Specs' }]));
    await waitFor(() => expect(screen.getAllByTestId('shell-chats-folder').map((node) => node.textContent)).toEqual(['Specs']));
  });

  it('moves a conversation into a folder from its menu', async () => {
    folders = [{ id: 'f1', name: 'Specs' }];
    const user = userEvent.setup();
    mount('/chat');

    await openRowMenu(user, 'Release notes');
    await user.click(await screen.findByRole('menuitem', { name: 'Move to folder…' }));
    await user.click(within(await screen.findByTestId('shell-move-to-folder')).getByText('Specs'));

    await waitFor(() => expect(conversationPuts).toEqual([{ id: String(EXISTING), body: { folder_id: 'f1' } }]));
    // The listing read again puts the row under its folder, out of the date group.
    await waitFor(() => expect(screen.queryAllByTestId('shell-chats-group')).toHaveLength(0));
    await user.click(screen.getByText('Specs'));
    expect(sidebarRow('Release notes')).toBeDefined();
  });
});

describe('the desktop chat: both lists read one listing', () => {
  it('shows the conversation the first send created in the sidebar', async () => {
    const user = userEvent.setup();
    mount('/chat');
    await waitFor(() => expect(sidebarRow('Release notes')).toBeDefined(), { timeout: 10_000 });

    await user.type(await screen.findByPlaceholderText('Type your message...', {}, { timeout: 10_000 }), 'First turn{Enter}');

    await waitFor(() => expect(sidebarRow('First turn')).toBeDefined(), { timeout: 10_000 });
    expect(sidebarRow('Release notes')).toBeDefined();
  });

  it('shows a rename made in the sidebar in the page list once the sidebar is hidden', async () => {
    const user = userEvent.setup();
    mount();

    await openRowMenu(user, 'Release notes');
    await user.click(await screen.findByRole('menuitem', { name: 'Rename' }));
    const field = await screen.findByTestId('shell-conversation-rename');
    await user.clear(field);
    await user.type(field, 'Renamed notes{Enter}');
    await waitFor(() => expect(conversationPuts).toEqual([{ id: String(EXISTING), body: { name: 'Renamed notes' } }]));
    await waitFor(() => expect(sidebarRow('Renamed notes')).toBeDefined());

    act(() => useDesktopLayout.getState().setSidebarOpen(false));
    const item = await screen.findByTestId(`conversation-item-${String(EXISTING)}`, {}, { timeout: 10_000 });
    await waitFor(() => expect(item).toHaveTextContent('Renamed notes'));
  });
});
