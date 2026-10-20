/**
 * The conversation lists of the chat surface inside the desktop frame (the
 * composition root the desktop app mounts: `DesktopFrame` around
 * `ChatWithEditors`).
 *
 * The sidebar's Chats rows are the one list: the page never shows its own
 * list in the frame, also while the sidebar is hidden or Chats is collapsed.
 * Opening the chat expands the Elitea section and the Chats disclosure. The
 * sidebar manages folders with the web list's hooks (New folder, Move to
 * folder…). The title row names the open conversation (`Chats ▸ folder ▸
 * name`) from the same rows, and renames it on a double-click. A message
 * moves a conversation that is not in a folder to the top of Today; one in a
 * folder stays there. The conversation the page creates reaches the sidebar.
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
  /** The date group the server puts it in; a message moves it to Today. */
  group?: string;
}

let conversations: StoredConversation[];
let folders: { id: string; name: string }[];
let folderPosts: unknown[];
let conversationPuts: { id: string; body: Record<string, unknown> }[];
let messagesSent: string[];

const wireRow = (c: StoredConversation, at: string) => ({ id: String(c.id), name: c.name, is_private: true, author_id: 1, updated_at: at, created_at: at });

const OLD = new Date(Date.now() - 20 * 86_400_000).toISOString();

function listing() {
  const at = new Date().toISOString();
  const ungrouped = conversations.filter((c) => c.folderId === null);
  const inGroup = (name: string) => ungrouped.filter((c) => (c.group ?? 'Today') === name).map((c) => wireRow(c, name === 'Today' ? at : OLD));
  return {
    pinned: { conversations: [] },
    date_groups: ['Today', 'This month'].map((name) => ({ name, conversations: inGroup(name) })).filter((group) => group.conversations.length > 0),
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
      HttpResponse.json(PERMITTED.map((name) => ({ name, enabled: true }))),
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
    http.post(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/:conversationUuid`, ({ params }) => {
      // The server's listing buckets by the last message: the conversation is now in Today.
      const id = Number(String(params['conversationUuid']).replace('conversation-uuid-', ''));
      messagesSent.push(String(id));
      conversations = conversations.map((c) => (c.id === id ? { ...c, group: 'Today' } : c));
      return HttpResponse.json({ execution_id: 'execution-1', events_url: `${BASE}/executions/${PROJECT}/execution-1/events` });
    }),
    http.get(`${BASE}/configurations/models/${PROJECT}`, () => HttpResponse.json({ items: [{ id: 'model-1', name: 'model-1', project_id: PROJECT, default: true }] })),
    http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
    http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/:conversationId`, () => HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 })),
  ];
}

const PERMITTED = [PERMISSIONS.chat.folders.get, PERMISSIONS.chat.folders.create, PERMISSIONS.chat.folders.update];

function mount(initialEntry = `/chat/${String(EXISTING)}`) {
  const frame = (children: ReactNode): ReactNode => (
    <DesktopFrame
      permissions={new Set(PERMITTED)}
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

/** The group (by its caption) or folder (by its name) whose list holds the sidebar row. */
function holderOf(name: string): string | undefined {
  const row = sidebarRow(name);
  const item = row?.closest('ul')?.closest('li');
  const caption = item?.querySelector('.shell-group-caption')?.textContent;
  if (caption !== undefined && caption !== null) return caption;
  const folder = item?.querySelector('[data-testid="shell-chats-folder"] .shell-row-main span:not([aria-hidden])');
  return folder?.textContent ?? undefined;
}

const title = (): string => screen.getByTestId('page-title').textContent ?? '';

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
  messagesSent = [];
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

describe('the desktop chat: the sidebar is the one list', () => {
  it('never shows the page list in the frame, also with the sidebar hidden', async () => {
    mount();
    expect(await screen.findByTestId('shell-chats', {}, { timeout: 10_000 })).toBeInTheDocument();
    await screen.findByPlaceholderText('Type your message...', {}, { timeout: 10_000 });
    expect(screen.queryByTestId('chat-conversation-sidebar')).not.toBeInTheDocument();

    act(() => useDesktopLayout.getState().setSidebarOpen(false));
    await waitFor(() => expect(screen.queryByTestId('shell-chats')).not.toBeInTheDocument());
    // The chat stays; no list takes the sidebar's place (as in Mail: ⌘\\ shows the sidebar again).
    expect(screen.getByPlaceholderText('Type your message...')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-conversation-sidebar')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Show sidebar' })).toBeInTheDocument();
  });

  it('expands a collapsed Elitea section and Chats disclosure on opening the chat; collapsing Chats there shows no page list', async () => {
    window.localStorage.setItem('el.desktop.eliteaOpen', 'false');
    window.localStorage.setItem('el.desktop.chatsOpen', 'false');
    const user = userEvent.setup();
    mount();

    expect(await screen.findByTestId('shell-chats', {}, { timeout: 10_000 })).toBeInTheDocument();
    expect(screen.getByTestId('shell-elitea-help')).toBeInTheDocument();

    await user.click(screen.getByTestId('shell-chats-toggle'));
    await waitFor(() => expect(screen.queryByTestId('shell-chats')).not.toBeInTheDocument());
    expect(screen.queryByTestId('chat-conversation-sidebar')).not.toBeInTheDocument();
  });
});

describe('the desktop chat: the title row and the selection', () => {
  it('names the open conversation in the title, and renames it on a double-click', async () => {
    const user = userEvent.setup();
    mount();
    await waitFor(() => expect(title()).toBe('ChatsRelease notes'), { timeout: 10_000 });
    await waitFor(() => expect(sidebarRow('Release notes')).toBeDefined(), { timeout: 10_000 });
    expect(within(sidebarRow('Release notes') as HTMLElement).getByRole('button', { name: 'Release notes' })).toHaveAttribute('aria-current', 'page');

    await user.dblClick(screen.getByTestId('title-conversation'));
    const field = await screen.findByTestId('title-rename');
    await user.clear(field);
    await user.type(field, 'Renamed notes{Enter}');
    await waitFor(() => expect(conversationPuts).toEqual([{ id: String(EXISTING), body: { name: 'Renamed notes' } }]));
    await waitFor(() => expect(title()).toBe('ChatsRenamed notes'));
    await waitFor(() => expect(sidebarRow('Renamed notes')).toBeDefined());
  });

  it('opens the folder that holds the open conversation, selects its row, and names the folder in the title', async () => {
    folders = [{ id: 'f1', name: 'Specs' }];
    conversations = [{ id: EXISTING, name: 'Release notes', folderId: 'f1' }];
    mount();
    await waitFor(() => expect(sidebarRow('Release notes')).toBeDefined(), { timeout: 10_000 });
    expect(screen.getByTestId('shell-chats-folder').querySelector('button')).toHaveAttribute('aria-expanded', 'true');
    expect(within(sidebarRow('Release notes') as HTMLElement).getByRole('button', { name: 'Release notes' })).toHaveAttribute('aria-current', 'page');
    await waitFor(() => expect(title()).toBe('ChatsSpecsRelease notes'));
  });
});

describe('the desktop chat: a message moves the chat to Today', () => {
  it('moves a conversation from an older group to the top of Today when a message is sent', async () => {
    conversations = [
      { id: 40, name: 'Fresh chat', folderId: null },
      { id: EXISTING, name: 'Release notes', folderId: null, group: 'This month' },
    ];
    const user = userEvent.setup();
    mount();
    // The open conversation's group opens for it.
    await waitFor(() => expect(holderOf('Release notes')).toBe('Previous 30 days'), { timeout: 10_000 });

    await user.type(await screen.findByPlaceholderText('Type your message...', {}, { timeout: 10_000 }), 'One more thing{Enter}');
    await waitFor(() => expect(messagesSent).toEqual([String(EXISTING)]), { timeout: 10_000 });
    await waitFor(() => expect(holderOf('Release notes')).toBe('Today'));
    const today = screen.getAllByTestId('shell-conversation').map((node) => node.textContent);
    expect(today.indexOf('Release notes')).toBeLessThan(today.indexOf('Fresh chat'));
    expect(within(sidebarRow('Release notes') as HTMLElement).getByRole('button', { name: 'Release notes' })).toHaveAttribute('aria-current', 'page');
  });

  it('keeps a conversation in its folder when a message is sent', async () => {
    folders = [{ id: 'f1', name: 'Specs' }];
    conversations = [{ id: EXISTING, name: 'Release notes', folderId: 'f1' }];
    const user = userEvent.setup();
    mount();
    await waitFor(() => expect(holderOf('Release notes')).toBe('Specs'), { timeout: 10_000 });

    await user.type(await screen.findByPlaceholderText('Type your message...', {}, { timeout: 10_000 }), 'One more thing{Enter}');
    await waitFor(() => expect(messagesSent).toEqual([String(EXISTING)]), { timeout: 10_000 });
    // Give the move a chance to happen: it must not.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50));
    });
    expect(holderOf('Release notes')).toBe('Specs');
    expect(screen.queryAllByTestId('shell-chats-group')).toHaveLength(0);
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
    // The folder row: its name and its count.
    await waitFor(() => expect(screen.getAllByTestId('shell-chats-folder').map((node) => node.textContent)).toEqual(['Specs0']));
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
});
