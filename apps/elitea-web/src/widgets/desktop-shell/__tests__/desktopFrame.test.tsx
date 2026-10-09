/**
 * The desktop frame in a thin router: the folders sidebar and its threads,
 * the command palette, the host's `app://command`, the shortcut ownership
 * rule (host menu present → the page binds none), and the panes.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { recordThread, WorkspaceIpcProvider } from '@/features/workspace';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { BROWSER_PLATFORM, type AppIpc } from '@/shared/desktop/appEvents';
import { createFakeAppIpc, MACOS_PLATFORM, type FakeAppIpc } from '@/shared/desktop/appEvents.fake';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { clearNamespace } from '@/shared/lib/storage';

import { AppIpcProvider, DesktopFrame, useDesktopLayout } from '../index';

const globals = globalThis as unknown as Record<string, unknown>;
const APP: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };
const DOCS: Workspace = { id: 'w2', path: '/Users/me/docs', name: 'docs', project_id: null, is_git: false };

function mount(options: { appIpc?: AppIpc | null; path?: string; workspaces?: Workspace[]; nextOpen?: Workspace } = {}) {
  const rootRoute = createRootRoute({
    component: () => (
      <DesktopFrame permissions={new Set()} projects={[{ id: 42, name: 'Marketing', suspended: false }]} selectedProjectId="42" onSelectProject={() => undefined}>
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
    ]),
    history: createMemoryHistory({ initialEntries: [options.path ?? '/workspaces'] }),
  });
  const ipc = createFakeWorkspaceIpc({ workspaces: options.workspaces ?? [APP, DOCS], ...(options.nextOpen ? { nextOpen: options.nextOpen } : {}) });
  const tree = (
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        <RouterProvider router={router} />
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
  it('lists the folders with their project and their threads, newest first, and opens a thread', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    recordThread('w1', { id: '78', title: 'quick overview', updatedAt: 2 });
    const user = userEvent.setup();
    const { router } = mount();

    const folders = await screen.findAllByTestId('shell-folder');
    expect(folders.map((f) => within(f).getAllByRole('button')[0]?.textContent)).toEqual(['appMarketing', 'docsNo project']);
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

  it('opens a folder from the sidebar and lands in it', async () => {
    const user = userEvent.setup();
    const picked: Workspace = { id: 'w3', path: '/tmp/new', name: 'new', project_id: null, is_git: false };
    const { router } = mount({ nextOpen: picked });
    const sidebar = await screen.findByTestId('desktop-sidebar');
    await user.click(within(sidebar).getAllByRole('button', { name: 'Open folder' })[0] as HTMLElement);
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces/w3'));
    expect(await within(sidebar).findByText('new')).toBeInTheDocument();
  });

  it('reaches the Elitea pages and Settings, and offers the way back to the folders', async () => {
    const user = userEvent.setup();
    const { router } = mount();
    const sidebar = await screen.findByTestId('desktop-sidebar');
    await user.click(within(within(sidebar).getByTestId('shell-elitea-catalog')).getByRole('button'));
    expect(await screen.findByTestId('catalog')).toBeInTheDocument();
    await user.click(within(sidebar).getByRole('button', { name: 'Settings' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings'));
    await user.click(screen.getByRole('button', { name: 'Folders' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces'));
  });
});

describe('DesktopFrame commands', () => {
  it('without a host, binds the shortcuts in the page: ⌘K palette, ⌘\\ sidebar, ⌘⌥\\ changes', async () => {
    const user = userEvent.setup();
    // An Elitea page: the frame draws the title row (with "Show sidebar" once it is hidden).
    mount({ appIpc: null, path: '/settings' });
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
