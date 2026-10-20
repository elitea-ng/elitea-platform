/**
 * The island in the frame: it follows the host's three event channels, ranks
 * what it hears, opens the list on a click, and acts (cancel, go to the
 * thread).
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { WorkspaceIpcProvider } from '@/features/workspace';
import { HandoffIpcProvider } from '@/features/workspace-handoff';
import { IndexIpcProvider } from '@/features/workspace-index';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { createFakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';
import { createFakeIndexIpc } from '@/shared/desktop/indexIpc.fake';
import type { IndexStatus } from '@/shared/desktop/indexIpc';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { clearNamespace } from '@/shared/lib/storage';

import { ActivityIsland, DesktopFrame, useActivityFeed } from '../index';

const globals = globalThis as unknown as Record<string, unknown>;
const APP: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };
const building: IndexStatus = { state: 'building', files: 0, entities: 0, relations: 0, vectors: 0, last_run: null, changed_files: 0, error: null, policy_off: false, on_disk: true };

function mount() {
  const ipc = createFakeWorkspaceIpc({ workspaces: [APP] });
  const indexIpc = createFakeIndexIpc();
  const handoffIpc = createFakeHandoffIpc();
  const rootRoute = createRootRoute({
    component: () => (
      <DesktopFrame permissions={new Set()} projects={[{ id: 42, name: 'Marketing', suspended: false }]} selectedProjectId="42" onSelectProject={() => undefined}>
        <Outlet />
      </DesktopFrame>
    ),
  });
  const page = () => (
    <>
      <ActivityIsland idle="Index ready · 3 entities" />
      <p data-testid="thread" />
    </>
  );
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: page }),
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: page }),
    ]),
    history: createMemoryHistory({ initialEntries: ['/workspaces'] }),
  });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        <IndexIpcProvider ipc={indexIpc}>
          <HandoffIpcProvider ipc={handoffIpc}>
            <RouterProvider router={router} />
          </HandoffIpcProvider>
        </IndexIpcProvider>
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
  return { ipc, indexIpc, handoffIpc, router };
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  clearNamespace();
  useActivityFeed.getState().reset();
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('ActivityIsland', () => {
  it('says the idle line when nothing runs', async () => {
    mount();
    expect(await screen.findByTestId('activity-island')).toHaveTextContent('Index ready · 3 entities');
    expect(screen.getByTestId('activity-island')).toHaveAttribute('data-kind', 'idle');
  });

  it('follows a turn, then an approval over it, and goes back to idle when the turn is done', async () => {
    const { ipc } = mount();
    await screen.findByTestId('activity-island');
    await waitFor(() => expect(ipc.subscriberCount()).toBeGreaterThan(0));
    useActivityFeed.getState().registerTurn('t1', { workspaceId: 'w1', workspaceName: 'app', conversationId: '77' });
    const at = (seq: number) => ({ turn_id: 't1', seq });
    act(() => {
      ipc.emit({ ...at(1), kind: 'status', payload: { phase: 'running' } });
      ipc.emit({ ...at(2), kind: 'tool_call', payload: { call_id: 'c1', tool: 'run_command', args_summary: '', remote: false } });
    });
    const island = screen.getByTestId('activity-island');
    await waitFor(() => expect(island).toHaveTextContent('Running run_command'));
    expect(island).toHaveAttribute('data-kind', 'turn');

    act(() => ipc.emit({ ...at(3), kind: 'approval_request', payload: { request_id: 'r1', tool: 'run_command', title: 'Run cargo test', detail: '', reason: '', can_remember: false } }));
    await waitFor(() => expect(island).toHaveTextContent('Approve: run_command'));
    expect(island).toHaveAttribute('data-kind', 'approval');

    act(() => ipc.emit({ ...at(4), kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: [], changed_files: 0 } }));
    await waitFor(() => expect(island).toHaveTextContent('Index ready'));
  });

  it('lists everything running with progress, and cancels an index build', async () => {
    const user = userEvent.setup();
    const { ipc, indexIpc } = mount();
    await screen.findByTestId('activity-island');
    await waitFor(() => expect(ipc.subscriberCount()).toBeGreaterThan(0));
    act(() => {
      indexIpc.emit({ workspace_id: 'w1', phase: 'parsing', message: '[extract] Parsing 120 files', status: building });
      indexIpc.emit({ workspace_id: 'w1', phase: 'building', message: '[progress] Processed 40 files', status: building });
    });
    const island = screen.getByTestId('activity-island');
    await waitFor(() => expect(island).toHaveTextContent('Indexing 40/120'));
    await user.click(island);
    const popover = await screen.findByTestId('island-popover');
    const row = within(popover).getByTestId('island-row');
    expect(row).toHaveTextContent('Indexing 40/120');
    expect(within(row).getByRole('progressbar', { name: 'Indexing 40/120' })).toHaveAttribute('aria-valuenow', '33');
    await user.click(within(row).getByRole('button', { name: /Cancel Indexing/ }));
    await waitFor(() => expect(indexIpc.calls.cancelled).toEqual(['w1']));
  });

  it('opens the thread of an approval when it is clicked', async () => {
    const { ipc, router } = mount();
    await screen.findByTestId('activity-island');
    await waitFor(() => expect(ipc.subscriberCount()).toBeGreaterThan(0));
    useActivityFeed.getState().registerTurn('t1', { workspaceId: 'w1', workspaceName: 'app', conversationId: '77' });
    act(() => ipc.emit({ turn_id: 't1', seq: 1, kind: 'approval_request', payload: { request_id: 'r1', tool: 'run_command', title: 'Run it', detail: '', reason: '', can_remember: false } }));
    const island = screen.getByTestId('activity-island');
    await waitFor(() => expect(island).toHaveAttribute('data-kind', 'approval'));
    await userEvent.setup().click(island);
    await waitFor(() => expect(router.state.location.pathname).toBe('/workspaces/w1'));
    expect(router.state.location.search).toEqual({ conversation: '77' });
  });

  it('announces the state, not the ticking timer', async () => {
    const { ipc } = mount();
    await screen.findByTestId('activity-island');
    await waitFor(() => expect(ipc.subscriberCount()).toBeGreaterThan(0));
    useActivityFeed.getState().registerTurn('t1', { workspaceId: 'w1', workspaceName: 'app', conversationId: '77' });
    act(() => ipc.emit({ turn_id: 't1', seq: 1, kind: 'status', payload: { phase: 'running' } }));
    const island = screen.getByTestId('activity-island');
    await waitFor(() => expect(island).toHaveTextContent('Working…'));
    expect(island).not.toHaveAttribute('aria-live');
    const live = within(island).getByRole('status');
    expect(live).toHaveAttribute('aria-live', 'polite');
    expect(live).toHaveTextContent(/^Working…$/);
    // The timer is drawn but hidden from assistive technology.
    expect(island.querySelector('[aria-hidden="true"]')).not.toBeNull();
  });

  it('opens the list instead when an approval has no known folder to go to', async () => {
    const user = userEvent.setup();
    const { ipc } = mount();
    await screen.findByTestId('activity-island');
    await waitFor(() => expect(ipc.subscriberCount()).toBeGreaterThan(0));
    // No registerTurn: the host sent an approval of a turn this window never started.
    act(() => ipc.emit({ turn_id: 'stranger', seq: 1, kind: 'approval_request', payload: { request_id: 'r1', tool: 'run_command', title: 'Run it', detail: '', reason: '', can_remember: false } }));
    const island = screen.getByTestId('activity-island');
    await waitFor(() => expect(island).toHaveAttribute('data-kind', 'approval'));
    await user.click(island);
    expect(await screen.findByTestId('island-popover')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Review' })).toBeNull();
  });
});
