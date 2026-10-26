/**
 * The composition root of the desktop's ONE right panel: the real frame with
 * the real Local work page (`WorkspaceSessionPage`, over the in-memory host)
 * and the chat's participants panel, as the desktop app mounts them.
 *
 * Proves the pages' tabs land in one panel (Changes, Approvals, Index on a
 * thread; Conversation on a chat — never both, never a second sidebar), that
 * a tab moves to a second, stacked group and back, closes and reopens from
 * the ⋯ menu, that the layout is remembered per kind of page, and that a tab
 * opens in a window of its own — live, titled, its chip marked — and comes
 * back to the panel when that window closes. A panel whose every tab was
 * closed comes back, tabs and all, from the toggle or ⌘⌥\.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { act, configure, fireEvent, getConfig, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { recordThread, WorkspaceIpcProvider } from '@/features/workspace';
import { IndexIpcProvider } from '@/features/workspace-index';
import { ChatParticipantsPanel } from '@/pages/chat/ChatParticipantsPanel';
import WorkspaceSessionPage from '@/pages/workspace/WorkspaceSessionPage';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { BROWSER_PLATFORM } from '@/shared/desktop/appEvents';
import { installExternalLinks } from '@/shared/desktop/externalLinks';
import { createFakeAppIpc, type FakeAppIpc } from '@/shared/desktop/appEvents.fake';
import { createFakeIndexIpc } from '@/shared/desktop/indexIpc.fake';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { clearNamespace } from '@/shared/lib/storage';
import { server } from '@/test/setup';
import { AppIpcProvider, DesktopFrame, useDesktopLayout } from '@/widgets/desktop-shell';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;
const APP: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };
const CONVERSATION = { id: '72', name: 'quick overview', participants: [{ id: 7, entity_name: 'user', entity_meta: { id: 3 }, meta: { name: 'Me' } }] };

vi.setConfig({ testTimeout: 30_000 });
let asyncUtilTimeout = 1000;
beforeAll(() => {
  asyncUtilTimeout = getConfig().asyncUtilTimeout;
  configure({ asyncUtilTimeout: 10_000 });
});
afterAll(() => configure({ asyncUtilTimeout }));

function mount(path: string): { router: ReturnType<typeof createRouter>; appIpc: FakeAppIpc } {
  const rootRoute = createRootRoute({
    component: () => (
      <DesktopFrame permissions={new Set()} projects={[{ id: 42, name: 'Marketing', suspended: false }]} selectedProjectId="42" onSelectProject={() => undefined}>
        <Outlet />
      </DesktopFrame>
    ),
  });
  const chat = (): React.JSX.Element => (
    <ChatParticipantsPanel
      activeConversation={CONVERSATION}
      activeParticipant={undefined}
      projectId="42"
      isMcpVisible={false}
      handlers={{ onSelectParticipant: () => undefined, onDeleteParticipant: () => undefined, onEditParticipant: () => undefined, onAddParticipants: () => undefined }}
    />
  );
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: WorkspaceSessionPage }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: chat }),
    ]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  const appIpc = createFakeAppIpc();
  render(
    <AppProviders>
      <AppIpcProvider ipc={appIpc}>
        <WorkspaceIpcProvider ipc={createFakeWorkspaceIpc({ workspaces: [APP] })}>
          <IndexIpcProvider ipc={createFakeIndexIpc({ statuses: { w1: { state: 'ready', files: 3, entities: 4 } } })}>
            <RouterProvider router={router as never} />
          </IndexIpcProvider>
        </WorkspaceIpcProvider>
      </AppIpcProvider>
    </AppProviders>,
  );
  return { router: router as never, appIpc };
}

const inspector = (): HTMLElement => screen.getByTestId('desktop-inspector');
const tabsOf = (group: number): string[] => within(screen.getByTestId(`inspector-tabs-${String(group)}`)).getAllByRole('tab').map((tab) => tab.textContent ?? '');
const persisted = (): { inspector: Record<string, { groups: { tabs: string[] }[] }> } => JSON.parse(window.localStorage.getItem('el.desktop.layout') ?? 'null') as never;

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  clearNamespace();
  recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
  useDesktopLayout.setState({
    sidebarOpen: true,
    changesOpen: true,
    inspectorMaximized: false,
    poppedTabs: [],
    inspector: {
      local: { groups: [{ tabs: ['changes', 'approvals', 'index'], active: 'changes' }], closed: [], split: 0.5, width: 360 },
      chat: { groups: [{ tabs: ['conversation'], active: 'conversation' }], closed: [], split: 0.5, width: 280 },
    },
    paletteOpen: false,
    hostMenu: false,
    notice: null,
    session: null,
    platform: BROWSER_PLATFORM,
  });
  server.use(
    http.get(`${BASE}/projects/project/default/1`, () => HttpResponse.json([{ id: '42', name: 'Marketing', suspended: false }])),
    http.get(`${BASE}/elitea_core/applications/prompt_lib/42`, () => HttpResponse.json({ rows: [], total: 0 })),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
  vi.restoreAllMocks();
});

describe('the desktop inspector, composed', () => {
  it('holds a thread\'s tabs in one panel, and only the chat\'s on a chat', async () => {
    const { router } = mount('/workspaces/w1?conversation=77');
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));
    expect(screen.getAllByTestId('desktop-inspector')).toHaveLength(1);
    // The thread draws no panel of its own any more.
    expect(screen.queryByTestId('session-panel')).toBeNull();
    expect(within(inspector()).getByText(/No changes yet/)).toBeInTheDocument();
    expect(within(inspector()).getByRole('tab', { name: 'Changes' })).toHaveAttribute('aria-selected', 'true');

    // ←/→ move between the tabs; the panel shows the one chosen.
    const changes = within(inspector()).getByRole('tab', { name: 'Changes' });
    changes.focus();
    fireEvent.keyDown(changes, { key: 'ArrowRight' });
    expect(within(inspector()).getByRole('tab', { name: 'Approvals' })).toHaveAttribute('aria-selected', 'true');
    expect(within(inspector()).getByRole('tab', { name: 'Approvals' })).toHaveFocus();
    expect(await within(inspector()).findByText(/Nothing was asked of you/)).toBeInTheDocument();
    fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'End' });
    expect(await within(inspector()).findByTestId('context-panel-empty')).toBeInTheDocument();

    await act(() => router.navigate({ to: '/chat/$conversationId', params: { conversationId: '72' } }));
    await waitFor(() => expect(tabsOf(0)).toEqual(['Conversation']));
    expect(await within(inspector()).findByTestId('participants-wrapper')).toHaveAttribute('data-appearance', 'inspector');
    expect(screen.getAllByTestId('desktop-inspector')).toHaveLength(1);
  });

  it('stacks a second group, moves tabs between the groups, closes and reopens one, and remembers it per page', async () => {
    const user = userEvent.setup();
    mount('/workspaces/w1?conversation=77');
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));

    // Right-click a chip: move it to a new bottom group.
    fireEvent.contextMenu(within(inspector()).getByRole('tab', { name: 'Index' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Move Index to bottom group' }));
    await waitFor(() => expect(tabsOf(1)).toEqual(['Index']));
    expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Context']);
    expect(within(screen.getByTestId('inspector-group-1')).getByTestId('index-panel')).toBeInTheDocument();
    expect(within(screen.getByTestId('inspector-group-0')).getByText(/No changes yet/)).toBeInTheDocument();
    expect(persisted().inspector['local']?.groups.map((group) => group.tabs)).toEqual([['changes', 'approvals'], ['index']]);

    // The splitter shares the height (keyboard).
    const splitter = screen.getByRole('separator', { name: 'Resize inspector groups' });
    fireEvent.keyDown(splitter, { key: 'ArrowDown' });
    expect(useDesktopLayout.getState().inspector.local.split).toBeCloseTo(0.55);

    // The keyboard way to the same menu: Shift+F10 on the chip, back to the top group; the bottom group collapses.
    fireEvent.keyDown(within(inspector()).getByRole('tab', { name: 'Index' }), { key: 'F10', shiftKey: true });
    await user.click(await screen.findByRole('menuitem', { name: 'Move Index to top group' }));
    await waitFor(() => expect(screen.queryByTestId('inspector-tabs-1')).toBeNull());
    expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']);
    expect(screen.queryByRole('separator', { name: 'Resize inspector groups' })).toBeNull();

    // × hides a tab; the ⋯ menu lists every tab and reopens it.
    await user.click(within(inspector()).getByRole('button', { name: 'Close Approvals' }));
    expect(tabsOf(0)).toEqual(['Changes', 'Index', 'Context']);
    await user.click(within(inspector()).getByRole('button', { name: 'Inspector options' }));
    const reopen = await screen.findByRole('menuitemcheckbox', { name: 'Approvals' });
    expect(reopen).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByRole('menuitemcheckbox', { name: 'Changes' })).toHaveAttribute('aria-checked', 'true');
    await user.click(reopen);
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));
    expect(within(inspector()).getByRole('tab', { name: 'Approvals' })).toHaveAttribute('aria-selected', 'true');

    // The chat keeps its own layout.
    expect(useDesktopLayout.getState().inspector.chat.groups).toEqual([{ tabs: ['conversation'], active: 'conversation' }]);
  });

  it('maps "Index settings…" to activating the Index tab, opening the panel', async () => {
    const user = userEvent.setup();
    mount('/workspaces/w1?conversation=77');
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));
    await user.click(within(inspector()).getByRole('button', { name: 'Hide inspector' }));
    await waitFor(() => expect(screen.queryByTestId('desktop-inspector')).toBeNull());
    act(() => useDesktopLayout.getState().showInspector('index'));
    expect(await within(inspector()).findByTestId('index-panel')).toBeInTheDocument();
    expect(within(inspector()).getByRole('tab', { name: 'Index' })).toHaveAttribute('aria-selected', 'true');
  });

  it('maximizes over the main column until Esc', async () => {
    const user = userEvent.setup();
    mount('/workspaces/w1?conversation=77');
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));
    await user.click(within(inspector()).getByRole('button', { name: 'Maximize' }));
    expect(inspector()).toHaveAttribute('data-maximized');
    expect(screen.getByRole('main', { hidden: true })).toHaveStyle({ display: 'none' });
    fireEvent.keyDown(window, { key: 'Escape' });
    await waitFor(() => expect(inspector()).not.toHaveAttribute('data-maximized'));
    expect(screen.getByRole('main')).toHaveStyle({ display: 'flex' });
  });

  it('opens a tab in a window of its own, live and titled, and takes it back when that window closes', async () => {
    const user = userEvent.setup();
    // The window the page opens: a real (jsdom) document with a window of its own.
    const frame = document.createElement('iframe');
    document.body.append(frame);
    const child = frame.contentDocument as Document;
    const popup = {
      document: child,
      closed: false,
      close() {
        this.closed = true;
      },
      focus: vi.fn(),
    };
    const open = vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    // The desktop's link handling, as `launchApp` installs it: the window's links go to the system browser too.
    const openExternal = vi.fn<(url: string) => Promise<void>>().mockResolvedValue();
    const uninstall = installExternalLinks({ openExternal }, 'https://elitea.example', document, window);
    const { appIpc } = mount('/workspaces/w1?conversation=77');
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));

    await user.click(within(inspector()).getByRole('button', { name: 'Open in new window' }));
    expect(open).toHaveBeenCalledWith('about:blank#elitea-panel=changes', 'elitea-panel-changes', expect.any(String));
    // The window shows the tab, from the page's own tree, under "<Tab> — <thread>".
    const body = await within(child.body).findByTestId('inspector-window-changes');
    expect(within(body).getByText(/No changes yet/)).toBeInTheDocument();
    expect(child.title).toBe('Changes — fix the build');
    expect(child.head.querySelector('style[data-mirror]')).not.toBeNull();
    const link = child.createElement('a');
    link.href = '/docs/changes';
    link.target = '_blank';
    body.append(link);
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    expect(openExternal).toHaveBeenCalledWith('https://elitea.example/docs/changes');
    // In the panel: the chip says where the tab is; a click on it raises that window.
    expect(within(inspector()).getByRole('tab', { name: 'Changes, in its own window' })).toBeInTheDocument();
    expect(within(inspector()).getByTestId('inspector-in-window')).toBeInTheDocument();
    await user.click(within(inspector()).getByRole('tab', { name: 'Changes, in its own window' }));
    expect(appIpc.calls.focusedWindows).toEqual(['panel-changes']);
    // The other tabs stay in the panel, live.
    await user.click(within(inspector()).getByRole('tab', { name: 'Approvals' }));
    expect(await within(inspector()).findByText(/Nothing was asked of you/)).toBeInTheDocument();

    // The window's document replaced under us (a reload): filled again, titled again.
    const reloaded = document.createElement('iframe');
    document.body.append(reloaded);
    popup.document = reloaded.contentDocument as Document;
    expect(await within(popup.document.body).findByTestId('inspector-window-changes')).toBeInTheDocument();
    expect(popup.document.title).toBe('Changes — fix the build');

    // Closing the window returns the tab.
    popup.close();
    await waitFor(() => expect(within(inspector()).getByRole('tab', { name: 'Changes' })).toBeInTheDocument());
    expect(useDesktopLayout.getState().poppedTabs).toEqual([]);
    await user.click(within(inspector()).getByRole('tab', { name: 'Changes' }));
    expect(await within(inspector()).findByText(/No changes yet/)).toBeInTheDocument();
    uninstall();
  });

  it('brings the panel back, tabs and all, after every tab was closed', async () => {
    const user = userEvent.setup();
    mount('/workspaces/w1?conversation=77');
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));
    const closeAll = async (): Promise<void> => {
      for (const name of ['Changes', 'Approvals', 'Index', 'Context']) await user.click(within(inspector()).getByRole('button', { name: `Close ${name}` }));
      await waitFor(() => expect(screen.queryByTestId('desktop-inspector')).toBeNull());
    };

    // Maximized, then every tab closed: the main column comes back with the toggle on it.
    await user.click(within(inspector()).getByRole('button', { name: 'Maximize' }));
    await closeAll();
    expect(useDesktopLayout.getState().inspectorMaximized).toBe(false);
    expect(screen.getByRole('main')).toHaveStyle({ display: 'flex' });
    await user.click(screen.getByRole('button', { name: 'Show inspector' }));
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));
    expect(inspector()).not.toHaveAttribute('data-maximized');

    // ⌘⌥\ (the shell's toggle) does the same, in one press.
    await closeAll();
    act(() => useDesktopLayout.getState().toggleInspector());
    await waitFor(() => expect(tabsOf(0)).toEqual(['Changes', 'Approvals', 'Index', 'Context']));
    // So does "Index settings…" (showInspector), which reopens the one tab asked for.
    await closeAll();
    act(() => useDesktopLayout.getState().showInspector('index'));
    await waitFor(() => expect(tabsOf(0)).toEqual(['Index']));
  });
});
