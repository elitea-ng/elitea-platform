/**
 * The activity status of the Local work threads in the sidebar (client
 * contract 1.12 glyphs): the host's turns (through the workspace IPC fake),
 * the local unread marks, the cloud copy's server frames, and the folder's
 * "N needs you".
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { recordThread, WorkspaceIpcProvider } from '@/features/workspace';
import { HandoffIpcProvider, rememberThreadUuid } from '@/features/workspace-handoff';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { createFakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { resetConversationActivityOverlay } from '@/shared/lib/conversationActivity';
import { clearNamespace } from '@/shared/lib/storage';

import type { TrackedTurn } from '../model/activityFeed.store';
import { localThreadActivity, useLocalThreadMarks } from '../model/localThreadActivity';
import { DesktopFrame, useActivityFeed, useDesktopLayout } from '../index';

const globals = globalThis as unknown as Record<string, unknown>;
const APP: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };
let registry: TestEventSourceRegistry;

function mount(path: string) {
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
    ]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  const ipc = createFakeWorkspaceIpc({ workspaces: [APP] });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        <HandoffIpcProvider ipc={createFakeHandoffIpc()}>
          <RouterProvider router={router} />
        </HandoffIpcProvider>
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
  return { ipc, router };
}

async function threadRow(title: string): Promise<HTMLElement> {
  const rows = await screen.findAllByTestId('shell-thread');
  const row = rows.find((candidate) => candidate.textContent?.startsWith(title));
  if (row === undefined) throw new Error(`no thread row ${title}`);
  return row;
}

const glyphOf = (row: HTMLElement): string | null => within(row).queryByTestId('conversation-activity-glyph')?.getAttribute('data-kind') ?? null;

beforeEach(() => {
  registry = installTestEventSource();
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  clearNamespace();
  useActivityFeed.getState().reset();
  useLocalThreadMarks.reset();
  useDesktopLayout.setState({ sidebarOpen: true, notice: null, session: null });
  vi.spyOn(document, 'hasFocus').mockReturnValue(true);
});

afterEach(() => {
  registry.restore();
  resetGeneratedClient();
  resetConfigForTests();
  resetConversationActivityOverlay();
  delete globals['elitea_ui_config'];
  vi.restoreAllMocks();
});

describe('Local work thread activity', () => {
  it('follows the host turn: running, then needs you (counted on the folder), then unread off screen, read when opened', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    recordThread('w1', { id: '78', title: 'quick overview', updatedAt: 2 });
    const user = userEvent.setup();
    const { ipc, router } = mount('/workspaces/w1?conversation=78');
    await threadRow('fix the build');
    await waitFor(() => expect(ipc.subscriberCount()).toBeGreaterThan(0));

    useActivityFeed.getState().registerTurn('t1', { workspaceId: 'w1', workspaceName: 'app', conversationId: '77' });
    act(() => ipc.emit({ turn_id: 't1', seq: 1, kind: 'status', payload: { phase: 'running' } }));
    await waitFor(async () => expect(glyphOf(await threadRow('fix the build'))).toBe('running'));
    expect(within(await threadRow('fix the build')).getByTestId('conversation-activity-glyph')).toHaveTextContent('Running');

    act(() => ipc.emit({ turn_id: 't1', seq: 2, kind: 'approval_request', payload: { request_id: 'r1', tool: 'run_command', title: 'Run it', detail: '', reason: '', can_remember: false } }));
    await waitFor(async () => expect(glyphOf(await threadRow('fix the build'))).toBe('needs_you'));
    const folder = screen.getAllByTestId('shell-folder')[0] as HTMLElement;
    expect(within(folder).getByTestId('activity-needs-you')).toHaveTextContent('1 needs you');

    // The turn finishes while thread 78 is open: 77 is unread, with a bold title.
    act(() => ipc.emit({ turn_id: 't1', seq: 3, kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: [], changed_files: 0 } }));
    await waitFor(async () => expect(glyphOf(await threadRow('fix the build'))).toBe('unread'));
    expect(within(folder).queryByTestId('activity-needs-you')).toBeNull();
    expect(within(await threadRow('fix the build')).getByText('fix the build')).toHaveAttribute('data-unread', 'true');

    // A turn of the open thread finishes on screen: read at once.
    useActivityFeed.getState().registerTurn('t2', { workspaceId: 'w1', workspaceName: 'app', conversationId: '78' });
    act(() => {
      ipc.emit({ turn_id: 't2', seq: 1, kind: 'status', payload: { phase: 'running' } });
      ipc.emit({ turn_id: 't2', seq: 2, kind: 'done', payload: { committed: true, conversation_id: '78', message_ids: [], changed_files: 0 } });
    });
    await waitFor(async () => expect(glyphOf(await threadRow('quick overview'))).toBe('done'));

    // Opening 77 reads it.
    await user.click(within(await threadRow('fix the build')).getAllByRole('button')[0] as HTMLElement);
    await waitFor(() => expect(router.state.location.search).toEqual({ conversation: '77' }));
    await waitFor(async () => expect(glyphOf(await threadRow('fix the build'))).toBe('done'));
    expect(useLocalThreadMarks.getState().marks['77']?.unread).toBe(false);
  });

  it('shows the server state of a thread copied to the cloud, from the project stream', async () => {
    recordThread('w1', { id: '79', title: 'in the cloud', updatedAt: 3 });
    rememberThreadUuid('w1', '79', 'u-79');
    mount('/workspaces');
    await threadRow('in the cloud');
    await waitFor(() => expect(registry.getOpen().filter((source) => source.url === 'https://elitea.example/events/prompt_lib/42')).toHaveLength(1));
    const at = new Date().toISOString();
    act(() => {
      registry.emit('conversation_activity', JSON.stringify({ conversation_id: 900, conversation_uuid: 'u-79', state: 'needs_you', since: at, last_activity_at: at }));
    });
    await waitFor(async () => expect(glyphOf(await threadRow('in the cloud'))).toBe('needs_you'));
  });
});

describe('the folder count and the marks', () => {
  it('counts a collapsed folder\'s threads that need the person from the feed, without its thread list', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    const user = userEvent.setup();
    const { ipc } = mount('/workspaces');
    await threadRow('fix the build');
    await waitFor(() => expect(ipc.subscriberCount()).toBeGreaterThan(0));
    const folder = screen.getAllByTestId('shell-folder')[0] as HTMLElement;
    // Collapse the folder: its threads are no longer shown.
    await user.click(within(folder).getAllByRole('button')[0] as HTMLElement);
    await waitFor(() => expect(screen.queryAllByTestId('shell-thread')).toHaveLength(0));

    useActivityFeed.getState().registerTurn('t1', { workspaceId: 'w1', workspaceName: 'app', conversationId: '77' });
    act(() => {
      ipc.emit({ turn_id: 't1', seq: 1, kind: 'status', payload: { phase: 'running' } });
      ipc.emit({ turn_id: 't1', seq: 2, kind: 'approval_request', payload: { request_id: 'r1', tool: 'run_command', title: 'Run it', detail: '', reason: '', can_remember: false } });
    });
    await waitFor(() => expect(within(folder).getByTestId('activity-needs-you')).toHaveTextContent('1 needs you'));
  });

  it('keeps the unread marks and the recent read ones, the newest 200 at most', () => {
    const day = 24 * 60 * 60 * 1000;
    const marks = useLocalThreadMarks.getState();
    marks.finished('old-read', Date.now() - 2 * day, true);
    marks.finished('old-unread', Date.now() - 2 * day, false);
    marks.finished('recent-read', Date.now(), true);
    expect(Object.keys(useLocalThreadMarks.getState().marks).sort()).toEqual(['old-unread', 'recent-read']);
    for (let index = 0; index < 205; index += 1) useLocalThreadMarks.getState().finished(`t${index}`, Date.now() - day + index, false);
    const kept = useLocalThreadMarks.getState().marks;
    expect(Object.keys(kept)).toHaveLength(200);
    expect(kept['recent-read']).toBeDefined();
    expect(kept['old-unread']).toBeUndefined();
  });
});

describe('localThreadActivity', () => {
  const turn: TrackedTurn = { turnId: 't', workspaceId: 'w1', workspaceName: 'app', conversationId: '5', phase: 'running', startedAt: 1, tool: null, steps: 0, approvals: [], commit: 'none', finished: false, changedFiles: 0, usage: null };

  it('reads running, needs you, and the unread mark', () => {
    expect(localThreadActivity('5', undefined, { t: turn }, {}, undefined).state).toBe('running');
    expect(localThreadActivity('5', undefined, { t: { ...turn, approvals: [{ requestId: 'r', tool: 'x', title: 'x' }] } }, {}, undefined).state).toBe('needs_you');
    const done = localThreadActivity('5', undefined, { t: { ...turn, finished: true } }, {}, { finishedAt: 10, unread: true });
    expect(done).toMatchObject({ state: 'idle', unread: true, lastActivityAt: new Date(10).toISOString() });
    expect(localThreadActivity('6', undefined, { t: turn }, {}, undefined)).toMatchObject({ state: 'idle', unread: false });
  });
});
