/**
 * The chat surface inside the desktop frame (the composition root the desktop
 * app mounts) against the same surface on the web.
 *
 * In the frame, the conversations are the sidebar's (its Chats rows), so the
 * page drops its in-page list; the participants, agents and context budget
 * move to the frame's full-height inspector, which the title bar toggles (and
 * ⌘⌥\), remembered per viewer. On the web nothing moves: the list column and
 * the participants rail stay in the page.
 */
import type { ReactNode } from 'react';

import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
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
const CONVERSATION = 41;
const globals = globalThis as unknown as Record<string, unknown>;

function handlers() {
  const at = new Date().toISOString();
  return [
    http.get(`${BASE}/auth/permissions/prompt_lib/${PROJECT}`, () => HttpResponse.json([{ name: PERMISSIONS.chat.folders.get, enabled: true }])),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 1, name: 'Ada', avatar: '', personal_project_id: PROJECT })),
    http.get(`${BASE}/elitea_core/folder/prompt_lib/${PROJECT}`, () =>
      HttpResponse.json({
        pinned: { conversations: [] },
        date_groups: [{ name: 'Today', total: 1, offset: 1, conversations: [{ id: String(CONVERSATION), name: 'Release notes', author_id: 1, updated_at: at, created_at: at }] }],
        folders: [],
        total_folders: 0,
      }),
    ),
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/${PROJECT}/${CONVERSATION}`, () =>
      HttpResponse.json({
        id: String(CONVERSATION),
        uuid: 'conversation-uuid-41',
        name: 'Release notes',
        participants: [{ id: 3, entity_name: 'application', entity_meta: { id: 5, project_id: PROJECT }, meta: { name: 'Coder' } }],
      }),
    ),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/${PROJECT}/${CONVERSATION}`, () => HttpResponse.json({ items: [], total: 0, page: 0, page_size: 50, total_pages: 1 })),
    http.get(`${BASE}/configurations/models/${PROJECT}`, () => HttpResponse.json({ items: [{ id: 'model-1', name: 'model-1', project_id: PROJECT, default: true }] })),
    http.get(`${BASE}/configurations/tts_voices/${PROJECT}`, () => HttpResponse.json({ items: [] })),
    http.get(`${BASE}/elitea_core/context_analytics/prompt_lib/${PROJECT}/:conversationId`, () => HttpResponse.json({ current_tokens: 0, max_tokens: 0, message_groups_in_context: 0 })),
    http.get(`${BASE}/elitea_core/application/prompt_lib/${PROJECT}/:id`, () => HttpResponse.json({ id: 5, name: 'Coder', description: '', versions: [] })),
  ];
}

function mount(frame: (children: ReactNode) => ReactNode): void {
  const rootRoute = createRootRoute({ component: (): ReactNode => frame(<Outlet />) });
  const surface = (): ReactNode => <ChatWithEditors />;
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: surface }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: surface }),
    ]),
    history: createMemoryHistory({ initialEntries: [`/chat/${CONVERSATION}`] }),
  });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={createFakeWorkspaceIpc({ workspaces: [] })}>
        <RouterProvider router={router as never} />
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
}

const inDesktopFrame = (children: ReactNode): ReactNode => (
  <DesktopFrame permissions={new Set([PERMISSIONS.chat.folders.get])} projects={[{ id: Number(PROJECT), name: 'Platform', suspended: false }]} selectedProjectId={PROJECT} onSelectProject={() => undefined}>
    {children}
  </DesktopFrame>
);

let restoreScrollIntoView: (() => void) | undefined;
let eventSources: ReturnType<typeof installTestEventSource>;

beforeEach(() => {
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

const persistedLayout = (): unknown => JSON.parse(window.localStorage.getItem('el.desktop.layout') ?? 'null');

describe('the chat surface in the desktop frame', () => {
  it('drops the in-page conversation list and hosts the participants in the inspector', async () => {
    mount(inDesktopFrame);

    const inspector = await screen.findByTestId('desktop-inspector', {}, { timeout: 10_000 });
    const participants = await within(inspector).findByTestId('participants-wrapper', {}, { timeout: 10_000 });
    expect(participants).toHaveAttribute('data-appearance', 'inspector');
    expect(within(inspector).getByText('Conversation')).toBeInTheDocument();
    // The conversations are the sidebar's now.
    expect(await screen.findByTestId('shell-chats')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-conversation-sidebar')).not.toBeInTheDocument();
  });

  it('hides and shows the inspector from its own button, the title bar and ⌘⌥\\, and remembers it', async () => {
    const user = userEvent.setup();
    mount(inDesktopFrame);
    const inspector = await screen.findByTestId('desktop-inspector', {}, { timeout: 10_000 });
    await within(inspector).findByTestId('participants-wrapper', {}, { timeout: 10_000 });
    expect(screen.queryByTestId('inspector-toggle')).not.toBeInTheDocument();

    await user.click(within(inspector).getByRole('button', { name: 'Hide inspector' }));
    await waitFor(() => expect(screen.queryByTestId('desktop-inspector')).not.toBeInTheDocument());
    await waitFor(() => expect(persistedLayout()).toMatchObject({ changesOpen: false }));

    await user.click(screen.getByTestId('inspector-toggle'));
    expect(await screen.findByTestId('desktop-inspector')).toBeInTheDocument();
    await waitFor(() => expect(persistedLayout()).toMatchObject({ changesOpen: true }));

    fireEvent.keyDown(window, { key: '«', code: 'Backslash', metaKey: true, altKey: true });
    await waitFor(() => expect(screen.queryByTestId('desktop-inspector')).not.toBeInTheDocument());
  });
});

describe('the chat surface on the web', () => {
  it('keeps the in-page conversation list and the participants rail', async () => {
    mount((children) => children);

    expect(await screen.findByTestId('chat-conversation-sidebar', {}, { timeout: 10_000 })).toBeInTheDocument();
    const participants = await screen.findByTestId('participants-wrapper', {}, { timeout: 10_000 });
    expect(participants).not.toHaveAttribute('data-appearance', 'inspector');
    expect(screen.queryByTestId('desktop-inspector')).not.toBeInTheDocument();
  });
});
