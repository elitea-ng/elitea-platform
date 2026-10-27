/**
 * The desktop frame in a thin router: the folders sidebar and its threads,
 * the command palette, the host's `app://command`, the shortcut ownership
 * rule (host menu present → the page binds none), and the panes.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { readThreads, recordThread, WorkspaceIpcProvider } from '@/features/workspace';
import { HandoffIpcProvider, rememberThreadUuid } from '@/features/workspace-handoff';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { BROWSER_PLATFORM, type AppIpc } from '@/shared/desktop/appEvents';
import { createFakeAppIpc, MACOS_PLATFORM, type FakeAppIpc } from '@/shared/desktop/appEvents.fake';
import { installTestEventSource } from '@/shared/api/sse/testing';
import { createFakeHandoffIpc, type FakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { clearNamespace } from '@/shared/lib/storage';

import { server } from '../../../test/setup';

import { AppIpcProvider, DesktopFrame, useDesktopLayout } from '../index';

const globals = globalThis as unknown as Record<string, unknown>;
const APP: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };
const DOCS: Workspace = { id: 'w2', path: '/Users/me/docs', name: 'docs', project_id: null, is_git: false };

const PROJECTS = [
  { id: 42, name: 'Marketing', suspended: false },
  { id: 7, name: 'Sales', suspended: false },
];

function mount(options: { appIpc?: AppIpc | null; path?: string; workspaces?: Workspace[]; nextOpen?: Workspace; handoffIpc?: FakeHandoffIpc; onSelectProject?: (id: string, name: string) => void; projects?: typeof PROJECTS; permissions?: ReadonlySet<string> } = {}) {
  const rootRoute = createRootRoute({
    component: () => (
      <DesktopFrame permissions={options.permissions ?? new Set()} projects={options.projects ?? PROJECTS} selectedProjectId="42" onSelectProject={options.onSelectProject ?? (() => undefined)}>
        <Outlet />
      </DesktopFrame>
    ),
  });
  const page = (testId: string) => () => <p data-testid={testId} />;
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: page('home') }),
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: page('thread') }),
      createRoute({ getParentRoute: () => rootRoute, path: '/settings', component: page('settings') }),
      createRoute({ getParentRoute: () => rootRoute, path: '/elitea-catalog', component: page('catalog') }),
      createRoute({ getParentRoute: () => rootRoute, path: '/apps', component: page('apps') }),
      createRoute({ getParentRoute: () => rootRoute, path: '/skills', component: page('skills') }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: page('chat') }),
    ]),
    history: createMemoryHistory({ initialEntries: [options.path ?? '/workspaces'] }),
  });
  const ipc = createFakeWorkspaceIpc({ workspaces: options.workspaces ?? [APP, DOCS], ...(options.nextOpen ? { nextOpen: options.nextOpen } : {}) });
  const tree = (
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        {options.handoffIpc === undefined ? <RouterProvider router={router} /> : <HandoffIpcProvider ipc={options.handoffIpc}>{<RouterProvider router={router} />}</HandoffIpcProvider>}
      </WorkspaceIpcProvider>
    </AppProviders>
  );
  render(options.appIpc === null || options.appIpc === undefined ? tree : <AppIpcProvider ipc={options.appIpc}>{tree}</AppIpcProvider>);
  return { router, ipc };
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  clearNamespace();
  useDesktopLayout.setState({ sidebarOpen: true, changesOpen: true, paletteOpen: false, hostMenu: false, notice: null, session: null, platform: BROWSER_PLATFORM });
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('DesktopFrame sidebar', () => {
  it("lists the project's folders and their threads, newest first, and opens a thread", async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    recordThread('w1', { id: '78', title: 'quick overview', updatedAt: 2 });
    const user = userEvent.setup();
    const { router } = mount();

    const folders = await screen.findAllByTestId('shell-folder');
    // A folder with threads shows their count after its name; only one with no project names that.
    expect(folders.map((f) => within(f).getAllByRole('button')[0]?.textContent)).toEqual(['app2', 'docsNo project']);
    const threads = await within(folders[0] as HTMLElement).findAllByTestId('shell-thread');
    expect(threads.map((thread) => thread.textContent)).toEqual(['quick overview', 'fix the build']);

    await user.click(within(threads[1] as HTMLElement).getByRole('button'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces/w1'));
    expect(router.state.location.search).toEqual({ conversation: '77' });
    expect(within(threads[1] as HTMLElement).getByRole('button')).toHaveAttribute('aria-current', 'page');

    // A folder collapses to its row.
    await user.click(within(folders[0] as HTMLElement).getAllByRole('button')[0] as HTMLElement);
    expect(within(folders[0] as HTMLElement).queryAllByTestId('shell-thread')).toHaveLength(0);
  });

  it('lists a rewound thread\'s earlier attempts under it, even after the rewound thread moved up', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    recordThread('w1', { id: '78', title: 'quick overview', updatedAt: 2 });
    recordThread('w1', { id: '88', title: 'fix the build (fork)', updatedAt: 3 }, '77');
    recordThread('w1', { id: '78', title: 'quick overview', updatedAt: 4 });
    recordThread('w1', { id: '88', title: 'fix the build (fork)', updatedAt: 5 });
    mount();

    const folders = await screen.findAllByTestId('shell-folder');
    const threads = await within(folders[0] as HTMLElement).findAllByTestId('shell-thread');
    expect(threads.map((thread) => thread.textContent)).toEqual(['fix the build (fork)', 'Earlier attempt: fix the build', 'quick overview']);
  });

  it('badges a thread that runs in the cloud or is on its way back, and no other', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    recordThread('w1', { id: '78', title: 'quick overview', updatedAt: 2 });
    recordThread('w1', { id: '79', title: 'no uuid yet', updatedAt: 3 });
    rememberThreadUuid('w1', '77', 'u-77');
    rememberThreadUuid('w1', '78', 'u-78');
    const handoffIpc = createFakeHandoffIpc();
    handoffIpc.setStatusOf('u-77', { state: 'CLOUD_IDLE', epoch: 2, executor: 'cloud', read_only: true, sent_from_here: true });
    handoffIpc.setStatusOf('u-78', { state: 'RECLAIMING', epoch: 3, executor: 'returning', read_only: true, sent_from_here: true });
    mount({ handoffIpc });

    const folders = await screen.findAllByTestId('shell-folder');
    const rows = await within(folders[0] as HTMLElement).findAllByTestId('shell-thread');
    await waitFor(() => expect(rows.map((row) => row.textContent)).toEqual(['no uuid yet', 'quick overviewReturning', 'fix the buildIn cloud']));
    // One batched read for every visible row with a UUID, not a call per row.
    expect(handoffIpc.calls.statusManys).toEqual([['u-77', 'u-78']]);
    expect(handoffIpc.calls.statuses).toEqual([]);
  });

  it('subscribes each bound project to the live handoff stream, once each, and skips the unbound folders', async () => {
    const registry = installTestEventSource();
    try {
      const APP2: Workspace = { id: 'w4', path: '/Users/me/code/app2', name: 'app2', project_id: 42, is_git: true };
      mount({ handoffIpc: createFakeHandoffIpc(), workspaces: [APP, APP2, DOCS] });
      await screen.findAllByTestId('shell-folder');
      await waitFor(() => {
        expect(registry.getOpen().filter((source) => source.url === 'https://elitea.example/events/prompt_lib/42')).toHaveLength(1);
      });
      // The unbound folder (project_id null) opens no stream.
      expect(registry.getOpen().filter((source) => source.url.includes('/events/prompt_lib/'))).toHaveLength(1);
    } finally {
      registry.restore();
    }
  });

  it('opens no handoff stream without the handoff host', async () => {
    const registry = installTestEventSource();
    try {
      mount();
      await screen.findAllByTestId('shell-folder');
      expect(registry.getOpen().filter((source) => source.url.includes('/events/prompt_lib/'))).toHaveLength(0);
    } finally {
      registry.restore();
    }
  });

  it('opens a folder from the sidebar and lands in it', async () => {
    const user = userEvent.setup();
    const picked: Workspace = { id: 'w3', path: '/tmp/new', name: 'new', project_id: null, is_git: false };
    const { router } = mount({ nextOpen: picked });
    const sidebar = await screen.findByTestId('desktop-sidebar');
    await user.click(within(sidebar).getAllByRole('button', { name: 'Open folder' })[0] as HTMLElement);
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces/w3'));
    expect(await within(sidebar).findByText('new')).toBeInTheDocument();
  });

  it('reaches the Elitea pages and Settings; the title row names the page and has no way "back"', async () => {
    const user = userEvent.setup();
    const { router } = mount();
    const sidebar = await screen.findByTestId('desktop-sidebar');
    await user.click(within(within(sidebar).getByTestId('shell-elitea-applications')).getByRole('button'));
    expect(await screen.findByTestId('apps')).toBeInTheDocument();
    expect(screen.getByTestId('page-title')).toHaveTextContent('Applications');
    // Catalog and Settings live in the account menu at the sidebar's foot.
    await user.click(within(sidebar).getByTestId('shell-account'));
    await user.click(await screen.findByRole('menuitem', { name: 'Catalog' }));
    expect(await screen.findByTestId('catalog')).toBeInTheDocument();
    expect(screen.getByTestId('page-title')).toHaveTextContent('Catalog');
    expect(within(sidebar).queryByTestId('shell-elitea-catalog')).not.toBeInTheDocument();
    await user.click(within(sidebar).getByTestId('shell-account'));
    await user.click(await screen.findByRole('menuitem', { name: /^Settings/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));
    // Settings opens as a dialog over the page it came from, and closing goes back there.
    const dialog = await screen.findByTestId('settings-dialog');
    expect(within(dialog).getByTestId('settings')).toBeInTheDocument();
    // Behind it, a still copy of the Catalog page: inert, without the live page's test ids.
    expect(document.querySelector('[inert] main')).not.toBeNull();
    expect(screen.queryByTestId('catalog')).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/elitea-catalog'));
    expect(screen.queryByTestId('settings-dialog')).not.toBeInTheDocument();
    expect(screen.getByTestId('page-title')).toHaveTextContent('Catalog');
    // The sidebar is the way to everything: the title row carries no "back to Local work" button.
    const main = screen.getByRole('main');
    expect(within(main).queryByRole('button', { name: 'Local work' })).not.toBeInTheDocument();
    expect(within(main).getAllByTestId('title-bar')).toHaveLength(1);
    // With the sidebar hidden, the row offers it back, and still no "back" button.
    act(() => useDesktopLayout.getState().setSidebarOpen(false));
    expect(within(main).getByRole('button', { name: 'Show sidebar' })).toBeInTheDocument();
    expect(within(main).queryByRole('button', { name: 'Local work' })).not.toBeInTheDocument();
  });
});

describe('DesktopFrame Workshop group', () => {
  // Without permissions only Skills is offered of the five (agents, pipelines, toolkits and MCPs are gated).
  it('keeps agents, pipelines and the rest in a group that is closed until opened, and remembers it', async () => {
    const user = userEvent.setup();
    const { router } = mount();
    const sidebar = await screen.findByTestId('desktop-sidebar');
    const workshop = within(sidebar).getByTestId('shell-elitea-workshop');
    expect(within(sidebar).queryByTestId('shell-elitea-skills')).not.toBeInTheDocument();
    expect(within(workshop).getByRole('button', { name: 'Workshop' })).toHaveAttribute('aria-expanded', 'false');
    await user.click(within(workshop).getByRole('button', { name: 'Workshop' }));
    expect(localStorage.getItem('el.desktop.workshopOpen')).toBe('true');
    await user.click(within(within(sidebar).getByTestId('shell-elitea-skills')).getByRole('button'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills'));
    expect(await screen.findByTestId('skills')).toBeInTheDocument();
  });

  it('opens itself when one of its pages is on screen', async () => {
    mount({ path: '/skills' });
    const sidebar = await screen.findByTestId('desktop-sidebar');
    expect(await within(sidebar).findByTestId('shell-elitea-skills')).toBeInTheDocument();
  });
});

describe('DesktopFrame context menus', () => {
  it('pops the native menu on a thread row and acts on the choice', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    const appIpc: FakeAppIpc = createFakeAppIpc(MACOS_PLATFORM);
    const { router } = mount({ appIpc });
    const folders = await screen.findAllByTestId('shell-folder');
    const thread = await within(folders[0] as HTMLElement).findByTestId('shell-thread');
    appIpc.chooseFromContextMenu('chat');
    fireEvent.contextMenu(thread);
    await waitFor(() => expect(appIpc.calls.contextMenus).toHaveLength(1));
    expect((appIpc.calls.contextMenus[0] ?? []).map((item) => ('label' in item ? item.label : '-'))).toEqual(['Open', 'Open in chat', 'Copy Link', 'Rename…']);
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/77'));
  });

  it('keeps the rename field open and says so when the server refuses the rename', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    server.use(http.put('*/elitea_core/conversation/prompt_lib/:projectId/:conversationId', () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })));
    const appIpc: FakeAppIpc = createFakeAppIpc(MACOS_PLATFORM);
    mount({ appIpc });
    const folders = await screen.findAllByTestId('shell-folder');
    appIpc.chooseFromContextMenu('rename');
    fireEvent.contextMenu(await within(folders[0] as HTMLElement).findByTestId('shell-thread'));
    const field = await screen.findByTestId('thread-rename');
    fireEvent.change(field, { target: { value: 'Fix the CI build' } });
    fireEvent.keyDown(field, { key: 'Enter' });
    expect(await screen.findByTestId('thread-rename-error')).toBeInTheDocument();
    expect(screen.getByTestId('thread-rename')).toHaveValue('Fix the CI build');
    expect(readThreads('w1').find((x) => x.id === '77')?.title).toBe('fix the build');
  });

  it('renames a thread in place from its menu', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    const renamed: unknown[] = [];
    server.use(
      http.put('*/elitea_core/conversation/prompt_lib/:projectId/:conversationId', async ({ request, params }) => {
        renamed.push({ ...params, body: await request.json() });
        return HttpResponse.json({ id: 77, name: 'Fix the CI build' });
      }),
    );
    const appIpc: FakeAppIpc = createFakeAppIpc(MACOS_PLATFORM);
    mount({ appIpc });
    const folders = await screen.findAllByTestId('shell-folder');
    const thread = await within(folders[0] as HTMLElement).findByTestId('shell-thread');
    appIpc.chooseFromContextMenu('rename');
    fireEvent.contextMenu(thread);
    const field = await screen.findByTestId('thread-rename');
    expect(field).toHaveValue('fix the build');
    fireEvent.change(field, { target: { value: 'Fix the CI build' } });
    fireEvent.keyDown(field, { key: 'Enter' });
    await waitFor(() => expect(readThreads('w1').find((x) => x.id === '77')).toMatchObject({ title: 'Fix the CI build', named: true }));
    expect(renamed).toHaveLength(1);
    expect(renamed[0]).toMatchObject({ projectId: '42', conversationId: '77', body: { name: 'Fix the CI build' } });
    expect(await within(folders[0] as HTMLElement).findByText('Fix the CI build')).toBeInTheDocument();
  });

  it('pops the native menu on a folder row: reveal, copy, projects, remove', async () => {
    const appIpc: FakeAppIpc = createFakeAppIpc(MACOS_PLATFORM);
    mount({ appIpc });
    const folders = await screen.findAllByTestId('shell-folder');
    fireEvent.contextMenu(within(folders[0] as HTMLElement).getAllByRole('button')[0] as HTMLElement);
    await waitFor(() => expect(appIpc.calls.contextMenus).toHaveLength(1));
    const labels = (appIpc.calls.contextMenus[0] ?? []).map((item) => ('label' in item ? item.label : '-'));
    expect(labels).toEqual(['New thread', 'New thread in a worktree', '-', 'Reveal in Finder', 'Copy Path', '-', 'Project', '-', 'Remove from sidebar']);
    const submenu = (appIpc.calls.contextMenus[0] ?? []).find((item) => 'children' in item);
    expect(submenu !== undefined && 'children' in submenu ? submenu.children.map((c) => c.label) : []).toEqual(['✓ Marketing', 'Sales']);
  });

  it('leaves the browser alone without a host', async () => {
    mount({ appIpc: null });
    const folders = await screen.findAllByTestId('shell-folder');
    const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    within(folders[0] as HTMLElement).getAllByRole('button')[0]?.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});

describe('DesktopFrame sidebar layout', () => {
  it('heads the folders "Local work" and keeps each folder on one line, its project in the tooltip', async () => {
    mount();
    const sidebar = await screen.findByTestId('desktop-sidebar');
    expect(within(sidebar).getByRole('navigation', { name: 'Local work' })).toBeInTheDocument();
    expect(within(sidebar).getByRole('heading', { name: 'Local work' })).toBeInTheDocument();
    const [app] = await screen.findAllByTestId('shell-folder');
    const row = within(app as HTMLElement).getAllByRole('button')[0] as HTMLElement;
    expect(row).toHaveAttribute('title', '/Users/me/code/app\nProject: Marketing');
    expect(within(row).queryByText('Marketing')).toBeNull();
    // The empty-state row shows only when there is no folder.
    expect(within(sidebar).queryByRole('button', { name: 'Open a folder to work in' })).toBeNull();
  });

  it('clips the frame, so no pane can scroll the window itself', async () => {
    mount({});
    const frame = await screen.findByTestId('desktop-frame');
    // `hidden` would still let a pane's scrollIntoView shift the whole frame.
    expect(getComputedStyle(frame).overflow).toBe('clip');
  });

  it('offers "Open a folder" as the empty state', async () => {
    const user = userEvent.setup();
    const picked: Workspace = { id: 'w3', path: '/tmp/new', name: 'new', project_id: null, is_git: false };
    const { router } = mount({ workspaces: [], nextOpen: picked });
    await user.click(await screen.findByRole('button', { name: 'Open a folder to work in' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces/w3'));
  });

  it("has one project switcher; the folders listed are that project's and those with none", async () => {
    const user = userEvent.setup();
    const onSelectProject = vi.fn();
    const other: Workspace = { id: 'w9', path: '/Users/me/sales', name: 'sales-deck', project_id: 7, is_git: false };
    mount({ workspaces: [APP, DOCS, other], onSelectProject });
    const sidebar = await screen.findByTestId('desktop-sidebar');
    await within(sidebar).findAllByTestId('shell-folder');
    expect(within(sidebar).queryByText('sales-deck')).toBeNull();
    expect(within(sidebar).queryByRole('button', { name: 'Elitea' })).toBeNull();
    const switcher = within(sidebar).getByTestId('shell-project-switcher');
    expect(switcher).toHaveAccessibleName('Project: Marketing');
    await user.click(switcher);
    expect(await screen.findByRole('option', { name: 'Marketing' })).toHaveAttribute('aria-selected', 'true');
    await user.click(screen.getByRole('option', { name: 'Sales' }));
    expect(onSelectProject).toHaveBeenCalledWith('7', 'Sales');
  });

  it('opens the Local work page from its caption, and a new chat from the Chats caption', async () => {
    const user = userEvent.setup();
    const { router } = mount({ path: '/skills', permissions: new Set(['models.chat.folders.get']) });
    await user.click(await screen.findByTestId('shell-local-work'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces'));
    await user.click(screen.getByTestId('shell-chats-caption'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat'));
    expect(router.state.location.search).toEqual({ create: '1' });
  });

  it('searches the projects as the person types and draws a long list a page at a time', async () => {
    const user = userEvent.setup();
    const many = Array.from({ length: 120 }, (_, index) => ({ id: 1000 + index, name: `Team ${index}`, suspended: false }));
    const onSelectProject = vi.fn();
    mount({ projects: [...PROJECTS, ...many], onSelectProject });
    await user.click(await screen.findByTestId('shell-project-switcher'));
    const list = await screen.findByRole('listbox', { name: 'Projects' });
    expect(within(list).getAllByRole('option')).toHaveLength(50);
    const search = screen.getByTestId('project-picker-search');
    expect(search).toHaveFocus();
    await user.type(search, 'team 11');
    expect(within(list).getAllByRole('option').map((option) => option.textContent)).toEqual(['Team 11', ...Array.from({ length: 10 }, (_, index) => `Team ${110 + index}`)]);
    await user.keyboard('{ArrowDown}{Enter}');
    expect(onSelectProject).toHaveBeenCalledWith('1110', 'Team 110');
    await user.click(screen.getByTestId('shell-project-switcher'));
    await user.type(await screen.findByTestId('project-picker-search'), 'nothing like it');
    expect(screen.getByText('No projects match')).toBeInTheDocument();
  });

  it("leaves a folder's thread for the start screen when switching to another project", async () => {
    const user = userEvent.setup();
    const onSelectProject = vi.fn();
    const { router } = mount({ path: `/workspaces/${APP.id}`, onSelectProject });
    await user.click(await screen.findByTestId('shell-project-switcher'));
    await user.click(await screen.findByRole('option', { name: 'Sales' }));
    await waitFor(() => expect(onSelectProject).toHaveBeenCalledWith('7', 'Sales'));
    expect(router.state.location.pathname).toBe('/workspaces');
  });

  it('moves a folder to another project only after a confirmation; one with no project joins at once', async () => {
    const user = userEvent.setup();
    mount();
    const [app, docs] = await screen.findAllByTestId('shell-folder');
    await user.click(within(app as HTMLElement).getByLabelText('More for app'));
    await user.click(await screen.findByRole('menuitem', { name: 'Sales' }));
    const dialog = await screen.findByTestId('move-folder-dialog');
    expect(within(dialog).getByRole('heading')).toHaveTextContent('Move “app” to Sales?');
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(await screen.findAllByTestId('shell-folder')).toHaveLength(2);
    await user.click(within(app as HTMLElement).getByLabelText('More for app'));
    await user.click(await screen.findByRole('menuitem', { name: 'Sales' }));
    await user.click(within(await screen.findByTestId('move-folder-dialog')).getByRole('button', { name: 'Move' }));
    // Now Sales' folder: it leaves Marketing's sidebar.
    await waitFor(() => expect(screen.getAllByTestId('shell-folder')).toHaveLength(1));

    await user.click(within(docs as HTMLElement).getByLabelText('More for docs'));
    await user.click(await screen.findByRole('menuitem', { name: 'Marketing' }));
    expect(screen.queryByTestId('move-folder-dialog')).toBeNull();
    await waitFor(() => expect(within(screen.getByTestId('shell-folder')).queryByText('No project')).toBeNull());
  });
});

describe('DesktopFrame commands', () => {
  it('without a host, binds the shortcuts in the page: ⌘K palette, ⌘\\ sidebar, ⌘⌥\\ changes', async () => {
    const user = userEvent.setup();
    // An Elitea page: the frame draws the title row (with "Show sidebar" once it is hidden).
    mount({ appIpc: null, path: '/apps' });
    await screen.findByTestId('desktop-sidebar');

    await user.keyboard('{Meta>}k{/Meta}');
    expect(await screen.findByRole('combobox', { name: 'Command palette' })).toHaveFocus();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());

    fireEvent.keyDown(window, { key: '\\', code: 'Backslash', metaKey: true });
    await waitFor(() => expect(screen.queryByTestId('desktop-sidebar')).toBeNull());
    expect(screen.getByRole('button', { name: 'Show sidebar' })).toBeInTheDocument();

    // ⌥ changes the produced key on macOS; the shortcut reads the physical key.
    fireEvent.keyDown(window, { key: '«', code: 'Backslash', metaKey: true, altKey: true });
    expect(useDesktopLayout.getState().changesOpen).toBe(false);
  });

  it('with a host, its menu owns the shortcuts: the page binds none, and app://command drives the shell', async () => {
    const appIpc: FakeAppIpc = createFakeAppIpc(MACOS_PLATFORM);
    const user = userEvent.setup();
    const { router } = mount({ appIpc });
    await waitFor(() => expect(useDesktopLayout.getState().hostMenu).toBe(true));
    await waitFor(() => expect(appIpc.subscriberCount()).toBe(1));

    await user.keyboard('{Meta>}k{/Meta}');
    expect(screen.queryByRole('combobox', { name: 'Command palette' })).toBeNull();

    act(() => appIpc.emit({ id: 'command_palette' }));
    expect(await screen.findByRole('combobox', { name: 'Command palette' })).toBeInTheDocument();
    act(() => appIpc.emit({ id: 'command_palette' }));
    await waitFor(() => expect(screen.queryByRole('combobox', { name: 'Command palette' })).toBeNull());

    act(() => appIpc.emit({ id: 'workspace_opened', args: { workspace_id: 'w2' } }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces/w2'));

    act(() => appIpc.emit({ id: 'settings' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));

    act(() => appIpc.emit({ id: 'workspace_open_failed', args: { message: 'That folder is not readable.' } }));
    expect(await screen.findByText('That folder is not readable.')).toBeInTheDocument();

    act(() => appIpc.emit({ id: 'toggle_sidebar' }));
    await waitFor(() => expect(screen.queryByTestId('desktop-sidebar')).toBeNull());
  });

  it('opens a folder the host was handed before the page listened (a dock drop at launch)', async () => {
    const appIpc: FakeAppIpc = createFakeAppIpc(MACOS_PLATFORM);
    appIpc.queueBeforeReady({ id: 'workspace_opened', args: { workspace_id: 'w2' } });
    const { router } = mount({ appIpc });
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces/w2'));
  });

  it('draws a drag region clear of the traffic lights on macOS, and none with normal chrome', async () => {
    mount({ appIpc: createFakeAppIpc(MACOS_PLATFORM) });
    const sidebar = await screen.findByTestId('desktop-sidebar');
    await waitFor(() => expect(within(sidebar).getByTestId('title-bar')).toHaveAttribute('data-tauri-drag-region'));
    expect(getComputedStyle(within(sidebar).getByTestId('title-bar')).paddingLeft).toBe('90px');
  });

  it('runs palette entries: filters by every word, Enter runs the highlighted one', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    const user = userEvent.setup();
    const { router } = mount({ appIpc: null });
    await screen.findAllByTestId('shell-folder');

    await user.keyboard('{Meta>}k{/Meta}');
    const box = await screen.findByRole('combobox', { name: 'Command palette' });
    await user.type(box, 'fix build');
    const options = screen.getAllByRole('option');
    expect(options).toHaveLength(1);
    expect(options[0]).toHaveTextContent('fix the build');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces/w1'));
    expect(router.state.location.search).toEqual({ conversation: '77' });
    expect(screen.queryByRole('combobox', { name: 'Command palette' })).toBeNull();

    await user.keyboard('{Meta>}k{/Meta}');
    await user.type(await screen.findByRole('combobox', { name: 'Command palette' }), 'settings');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));
  });

  it('hands "new thread" and "plan mode" to the session on screen', async () => {
    const calls: string[] = [];
    const appIpc = createFakeAppIpc(MACOS_PLATFORM);
    mount({ appIpc });
    await waitFor(() => expect(appIpc.subscriberCount()).toBe(1));
    useDesktopLayout.getState().setSession({ workspaceId: 'w1', newThread: () => calls.push('new'), togglePlanMode: () => calls.push('plan') });
    act(() => appIpc.emit({ id: 'new_thread' }));
    expect(calls).toEqual(['new']);
  });
});
