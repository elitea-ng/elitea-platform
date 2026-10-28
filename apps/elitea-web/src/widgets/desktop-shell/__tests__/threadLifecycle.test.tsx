/**
 * Archive and Delete on the sidebar's Local work threads: a folder thread
 * archives in one click (the conversation's own archive, hidden from Local
 * work, shown again after an unarchive); Delete asks, then deletes the
 * conversation, the host's history and the local record. A task worktree's
 * thread asks with "Also remove its worktree" checked, removes the worktree
 * with the parent folder's id, and asks again when the host refuses for
 * uncommitted work.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { readThreads, recordThread, WorkspaceIpcProvider } from '@/features/workspace';
import { HandoffIpcProvider } from '@/features/workspace-handoff';
import { WorktreeIpcProvider } from '@/features/workspace-worktrees';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { archivedThreadIds, unarchiveLocalThread } from '@/shared/desktop/archivedThreads';
import { createFakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { createFakeWorktreeIpc, fakeWorktreeEntry, type FakeWorktreeIpc } from '@/shared/desktop/worktreeIpc.fake';
import { clearNamespace } from '@/shared/lib/storage';
import { server } from '@/test/setup';

import { DesktopFrame, useDesktopLayout } from '../index';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;
const APP: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };
const UUID = '0b2c4d6e-0000-4000-8000-000000000001';
const TASK: Workspace = { id: 'wt-1', path: '/tmp/repo-worktrees/0b2c4d6e', name: 'app', project_id: 42, is_git: true, parent_id: 'w1', thread_uuid: UUID };

let archived: string[];
let deleted: string[];

function mount(path: string, worktrees: FakeWorktreeIpc = createFakeWorktreeIpc()) {
  const rootRoute = createRootRoute({
    component: () => (
      <DesktopFrame permissions={new Set()} projects={[{ id: 42, name: 'Marketing', suspended: false }]} selectedProjectId="42" onSelectProject={() => undefined}>
        <Outlet />
      </DesktopFrame>
    ),
  });
  const page = () => <p data-testid="page" />;
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: page }),
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: page }),
    ]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  const ipc = createFakeWorkspaceIpc({ workspaces: [APP, TASK] });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        <WorktreeIpcProvider ipc={worktrees}>
          <HandoffIpcProvider ipc={createFakeHandoffIpc()}>
            <RouterProvider router={router} />
          </HandoffIpcProvider>
        </WorktreeIpcProvider>
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
  return { ipc, router, worktrees };
}

async function choose(moreTestId: string, rowTestId: string, item: string): Promise<void> {
  const user = userEvent.setup();
  const row = await screen.findByTestId(rowTestId);
  await user.click(within(row).getByTestId(moreTestId));
  await user.click(await screen.findByRole('menuitem', { name: item }));
}

beforeEach(() => {
  archived = [];
  deleted = [];
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  clearNamespace();
  useDesktopLayout.setState({ sidebarOpen: true, notice: null, session: null });
  server.use(
    http.put(`${BASE}/elitea_core/conversation_archive/prompt_lib/42/:id`, ({ params }) => {
      archived.push(String(params['id']));
      return HttpResponse.json({ ok: true });
    }),
    http.delete(`${BASE}/elitea_core/conversation/prompt_lib/42/:id`, ({ params }) => {
      deleted.push(String(params['id']));
      return new HttpResponse(null, { status: 204 });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
  vi.restoreAllMocks();
});

describe('a folder thread', () => {
  it('archives in one click: the conversation is archived, the row leaves Local work, the open thread goes to the folder', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    const { router } = mount('/workspaces/w1?conversation=77');
    await choose('shell-thread-more', 'shell-thread', 'Archive');
    await waitFor(() => expect(archived).toEqual(['77']));
    await waitFor(() => expect(screen.queryByTestId('shell-thread')).toBeNull());
    expect(screen.queryByTestId('thread-lifecycle-dialog')).toBeNull();
    expect(archivedThreadIds().has('77')).toBe(true);
    await waitFor(() => expect(router.state.location.search).toEqual({}));
    expect(router.state.location.pathname).toBe('/workspaces/w1');
    // Unarchived (the Archived page), it is listed again.
    unarchiveLocalThread('77');
    expect(readThreads('w1').map((thread) => thread.id)).toEqual(['77']);
  });

  it('deletes after asking: the conversation, the host history and the local record', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    recordThread('w1', { id: '78', title: 'keep me', updatedAt: 2 });
    const { ipc } = mount('/workspaces');
    const history = vi.spyOn(ipc, 'deleteThreadHistory');
    const user = userEvent.setup();
    const rows = await screen.findAllByTestId('shell-thread');
    const row = rows.find((candidate) => candidate.textContent?.startsWith('fix the build')) as HTMLElement;
    await user.click(within(row).getByTestId('shell-thread-more'));
    await user.click(await screen.findByRole('menuitem', { name: 'Delete…' }));
    const dialog = await screen.findByTestId('thread-lifecycle-dialog');
    expect(within(dialog).getByText('Delete “fix the build”?')).toBeInTheDocument();
    // No worktree: nothing to remove with it.
    expect(within(dialog).queryByRole('checkbox')).toBeNull();
    await user.click(within(dialog).getByTestId('thread-lifecycle-confirm'));
    await waitFor(() => expect(deleted).toEqual(['77']));
    await waitFor(() => expect(history).toHaveBeenCalledWith('w1', '77'));
    await waitFor(() => expect(readThreads('w1').map((thread) => thread.id)).toEqual(['78']));
    await waitFor(() => expect(screen.getAllByTestId('shell-thread')).toHaveLength(1));
  });

  it('keeps the thread when the delete is cancelled or the server refuses it', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    server.use(http.delete(`${BASE}/elitea_core/conversation/prompt_lib/42/:id`, () => HttpResponse.json({ error: 'no' }, { status: 403 })));
    mount('/workspaces');
    const user = userEvent.setup();
    await choose('shell-thread-more', 'shell-thread', 'Delete…');
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByTestId('thread-lifecycle-dialog')).toBeNull());
    await choose('shell-thread-more', 'shell-thread', 'Delete…');
    await user.click(await screen.findByTestId('thread-lifecycle-confirm'));
    await waitFor(() => expect(useDesktopLayout.getState().notice).toBe('The thread could not be deleted. Try again.'));
    expect(readThreads('w1').map((thread) => thread.id)).toEqual(['77']);
  });
});

describe("a task worktree's thread", () => {
  const worktreeWith = (dirty: number) => createFakeWorktreeIpc({ entries: [fakeWorktreeEntry({ workspace_id: 'wt-1', thread_uuid: UUID, branch: 'elitea/0b2c4d6e', dirty })] });

  it('archives and removes the worktree by default (the parent folder and the thread uuid), and leaves it', async () => {
    recordThread('wt-1', { id: '90', title: 'parallel fix', updatedAt: 1 });
    const { worktrees, router } = mount('/workspaces/wt-1?conversation=90', worktreeWith(0));
    await choose('worktree-task-more', 'worktree-task', 'Archive');
    const dialog = await screen.findByTestId('thread-lifecycle-dialog');
    expect(within(dialog).getByRole('checkbox', { name: 'Also remove its worktree' })).toBeChecked();
    expect(within(dialog).getByText('The branch elitea/0b2c4d6e is kept when it has commits.')).toBeInTheDocument();
    await userEvent.setup().click(within(dialog).getByTestId('thread-lifecycle-confirm'));
    await waitFor(() => expect(archived).toEqual(['90']));
    await waitFor(() => expect(worktrees.calls.remove).toEqual([['w1', UUID, false]]));
    await waitFor(() => expect(screen.queryByTestId('worktree-task')).toBeNull());
    expect(router.state.location.pathname).toBe('/workspaces/w1');
  });

  it('deletes and keeps the worktree when the box is unchecked', async () => {
    recordThread('wt-1', { id: '90', title: 'parallel fix', updatedAt: 1 });
    const { worktrees } = mount('/workspaces', worktreeWith(0));
    const user = userEvent.setup();
    await choose('worktree-task-more', 'worktree-task', 'Delete…');
    await user.click(await screen.findByRole('checkbox', { name: 'Also remove its worktree' }));
    await user.click(screen.getByTestId('thread-lifecycle-confirm'));
    await waitFor(() => expect(deleted).toEqual(['90']));
    await waitFor(() => expect(screen.queryByTestId('thread-lifecycle-dialog')).toBeNull());
    expect(worktrees.calls.remove).toEqual([]);
    expect(await screen.findByTestId('worktree-task')).toHaveTextContent('New task');
  });

  it('asks about uncommitted work: Remove anyway forces it', async () => {
    recordThread('wt-1', { id: '90', title: 'parallel fix', updatedAt: 1 });
    const { worktrees } = mount('/workspaces', worktreeWith(2));
    const user = userEvent.setup();
    await choose('worktree-task-more', 'worktree-task', 'Archive');
    await user.click(await screen.findByTestId('thread-lifecycle-confirm'));
    expect(await screen.findByText('The worktree has uncommitted work')).toBeInTheDocument();
    await user.click(screen.getByTestId('thread-lifecycle-force'));
    await waitFor(() => expect(worktrees.calls.remove).toEqual([['w1', UUID, false], ['w1', UUID, true]]));
    await waitFor(() => expect(screen.queryByTestId('worktree-task')).toBeNull());
  });

  it('asks about uncommitted work: Keep the worktree keeps it', async () => {
    recordThread('wt-1', { id: '90', title: 'parallel fix', updatedAt: 1 });
    const { worktrees } = mount('/workspaces', worktreeWith(2));
    const user = userEvent.setup();
    await choose('worktree-task-more', 'worktree-task', 'Archive');
    await user.click(await screen.findByTestId('thread-lifecycle-confirm'));
    await user.click(await screen.findByRole('button', { name: 'Keep the worktree' }));
    await waitFor(() => expect(screen.queryByTestId('thread-lifecycle-dialog')).toBeNull());
    expect(worktrees.calls.remove).toEqual([['w1', UUID, false]]);
    expect(archived).toEqual(['90']);
    expect(await screen.findByTestId('worktree-task')).toHaveTextContent('New task');
  });
});
