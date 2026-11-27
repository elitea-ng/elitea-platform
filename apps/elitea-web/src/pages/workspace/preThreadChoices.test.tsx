/**
 * Every choice made in a folder's new thread before its first message is the thread's from its first turn (#3144):
 * the mode (Full auto, Bypass, Plan), "Run to completion", "Respect AGENTS.md" and "Work in". With "Work in › New
 * worktree" the message moves to the worktree's own new thread, a new session of another workspace: before #3144 it
 * started in Act there, with the folder's Run to completion and AGENTS.md respected. Full auto is still confirmed when
 * picked and never kept (ADR-0061): carried into the thread being created only.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { act, configure, getConfig, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, onTestFinished, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { WorkspaceIpcProvider } from '@/features/workspace';
import { WorktreeIpcProvider } from '@/features/workspace-worktrees';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { createFakeWorktreeIpc } from '@/shared/desktop/worktreeIpc.fake';
import type { WorktreeIpc } from '@/shared/desktop/worktreeIpc';
import { testTimeout } from '@/test/timeouts';
import { InspectorScope, useDesktopLayout } from '@/widgets/desktop-shell';

import { server } from '../../test/setup';
import { setPendingRespect } from './pendingRespect';
import WorkspaceSessionPage from './WorkspaceSessionPage';

// The organisation allows Full auto (`full_auto_status`, the host's only command this test answers).
const invoke = vi.fn((command: string) => (command === 'full_auto_status' ? Promise.resolve({ allowed: true }) : Promise.reject(new Error(command))));

const BASE = '/api/v2';
const FOLDER: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };
const globals = globalThis as unknown as Record<string, unknown>;

vi.setConfig({ testTimeout: testTimeout(30_000) });
let asyncUtilTimeout = 1000;
beforeAll(() => {
  asyncUtilTimeout = getConfig().asyncUtilTimeout;
  configure({ asyncUtilTimeout: testTimeout(10_000) });
});
afterAll(() => configure({ asyncUtilTimeout }));

function forgetKept(): void {
  for (const id of ['w1', ...Object.keys(window.localStorage)]) {
    window.localStorage.removeItem(`el.desktop.workspace.${id}.runToCompletion`);
    window.localStorage.removeItem(`el.desktop.workspace.${id}.bypass`);
    window.localStorage.removeItem(`el.desktop.workspace.${id}.bypassConfirmed`);
    window.localStorage.removeItem(`el.desktop.workspace.${id}.workIn`);
  }
  setPendingRespect('w1', true);
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/projects/project/default/1`, () => HttpResponse.json([{ id: '42', name: 'Marketing', suspended: false }])),
    http.get(`${BASE}/elitea_core/version/prompt_lib/:project/:app/:version`, ({ params }) => HttpResponse.json({ id: Number(params['version']), name: 'base', llm_settings: { model_name: 'gpt-4o' } })),
    http.get(`${BASE}/configurations/models/:project`, () => HttpResponse.json({ items: [{ name: 'gpt-4o', project_id: 42 }], total: 1 })),
    http.get(`${BASE}/elitea_core/application_skills/prompt_lib/42/9`, () => HttpResponse.json({ items: [], total: 0, page: 1, page_size: 0, total_pages: 1 })),
    http.get(`${BASE}/elitea_core/skills/prompt_lib/42`, () => HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100, total_pages: 0 })),
    http.get(`${BASE}/elitea_core/applications/prompt_lib/42`, () =>
      HttpResponse.json({ rows: [{ id: '5', name: 'Coder', tags: [], created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', owner_id: '1', is_forked: false, meta: null, has_interrupt: false }], total: 1 }),
    ),
    http.get(`${BASE}/elitea_core/application/prompt_lib/42/5`, () =>
      HttpResponse.json({ id: '5', name: 'Coder', description: '', icon: '', owner_id: '1', created_at: '2026-01-01T00:00:00Z', versions: [{ id: '9', name: 'base', status: 'published', agent_type: 'openai', created_at: '2026-01-01T00:00:00Z' }] }),
    ),
    http.get(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ rows: [], total: 0 })),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })),
    http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ id: 77, name: 'fix the build' })),
    http.post(`${BASE}/elitea_core/participants/prompt_lib/42/77`, () => HttpResponse.json([])),
  );
  forgetKept();
  vi.stubGlobal('__TAURI_INTERNALS__', { invoke });
});

afterEach(() => {
  vi.unstubAllGlobals();
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

/** The worktree host: each worktree it makes is a workspace of its own, under the folder. */
function worktreeHost(ipc: FakeWorkspaceIpc): WorktreeIpc {
  const fake = createFakeWorktreeIpc();
  return {
    ...fake,
    create: async (workspaceId, threadUuid, base) => {
      const entry = await fake.create(workspaceId, threadUuid, base);
      ipc.addWorkspace({ id: entry.workspace_id, path: entry.path, name: 'app', project_id: 42, is_git: true, parent_id: workspaceId, thread_uuid: threadUuid });
      return entry;
    },
  };
}

function mount(ipc: FakeWorkspaceIpc): void {
  const rootRoute = createRootRoute();
  const session = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: WorkspaceSessionPage });
  const router = createRouter({ routeTree: rootRoute.addChildren([session]), history: createMemoryHistory({ initialEntries: ['/workspaces/w1'] }) });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        <WorktreeIpcProvider ipc={worktreeHost(ipc)}>
          <InspectorScope>
            <RouterProvider router={router} />
          </InspectorScope>
        </WorktreeIpcProvider>
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
}

async function pickMode(user: ReturnType<typeof userEvent.setup>, name: string | RegExp): Promise<void> {
  await user.click(await screen.findByTestId('mode-picker'));
  await user.click(await screen.findByRole('menuitemradio', { name }));
}

async function confirmFullAuto(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await pickMode(user, /^Full auto/u);
  await user.click(await screen.findByTestId('workspace-full-auto-confirm'));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Run this thread in Full auto?' })).toBeNull());
  expect(screen.getByTestId('mode-picker')).toHaveTextContent('Full auto');
}

/** "Work in" is under the Participants tab's Thread settings (#3145). */
async function chooseWorktree(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  act(() => useDesktopLayout.getState().showInspector('participants'));
  await user.click(await screen.findByTestId('work-in-chip'));
  await user.click(await screen.findByTestId('work-in-worktree'));
}

async function sendFirst(user: ReturnType<typeof userEvent.setup>, ipc: FakeWorkspaceIpc, text: string): Promise<void> {
  await user.type(screen.getByTestId('chat-message-input'), text);
  await user.click(screen.getByRole('button', { name: 'Send' }));
  await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
}

/** The folder's new thread is ready to take a message (its agent read). */
async function ready(): Promise<ReturnType<typeof userEvent.setup>> {
  const user = userEvent.setup();
  await waitFor(() => expect(screen.getByTestId('agent-chip')).toHaveTextContent('Coder'));
  return user;
}

describe('the choices made before the first message carry into the thread it creates (#3144)', () => {
  it('a new worktree thread starts in the confirmed Full auto, with Run to completion off and AGENTS.md not respected', async () => {
    onTestFinished(forgetKept);
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    mount(ipc);
    const user = await ready();

    await confirmFullAuto(user);
    await user.click(screen.getByTestId('workspace-run-to-completion-toggle'));
    setPendingRespect('w1', false);
    await chooseWorktree(user);
    await sendFirst(user, ipc, 'fix the build');

    const started = ipc.calls.started[0];
    expect(started?.workspace_id).toMatch(/^wt-/u);
    expect(started).toMatchObject({ plan_mode: false, bypass_approvals: true, full_auto: true });
    expect(started?.unattended).toBeUndefined();
    // "Respect AGENTS.md" off reached the worktree's thread before its first turn.
    expect(ipc.calls.respects).toEqual([{ workspaceId: started?.workspace_id, conversationId: '77', respect: false }]);
    // The worktree's thread shows the mode it runs in: Full auto, not Act.
    await waitFor(() => expect(screen.getByTestId('mode-picker')).toHaveTextContent('Full auto'));
    // Full auto is never kept: nothing of it is in storage, for the folder or the worktree.
    expect(Object.keys(window.localStorage).filter((key) => /full.?auto/iu.test(key))).toEqual([]);
  });

  it('a new worktree thread starts in Plan mode when Plan was chosen', async () => {
    onTestFinished(forgetKept);
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    mount(ipc);
    const user = await ready();

    await pickMode(user, 'Plan mode (no changes)');
    await chooseWorktree(user);
    await sendFirst(user, ipc, 'plan the fix');

    expect(ipc.calls.started[0]).toMatchObject({ plan_mode: true });
    expect(ipc.calls.started[0]?.bypass_approvals).toBeUndefined();
    await waitFor(() => expect(screen.getByTestId('mode-picker')).toHaveTextContent('Plan'));
  });

  it('a new worktree thread starts in Bypass with the Run to completion opt-ins chosen in the folder', async () => {
    onTestFinished(forgetKept);
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    mount(ipc);
    const user = await ready();

    await pickMode(user, 'Bypass mode (no approvals)');
    const dialog = await screen.findByRole('dialog', { name: 'Run without approvals?' });
    await user.click(within(dialog).getByRole('button', { name: 'Turn on bypass mode' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Run without approvals?' })).toBeNull());
    await user.click(screen.getByTestId('workspace-run-to-completion-open'));
    const popover = await screen.findByRole('dialog', { name: 'Run to completion' });
    await user.click(within(popover).getByRole('checkbox', { name: 'Hook asks' }));
    await user.keyboard('{Escape}');
    await chooseWorktree(user);
    await sendFirst(user, ipc, 'fix it');

    expect(ipc.calls.started[0]).toMatchObject({ plan_mode: false, bypass_approvals: true, unattended: { hook_asks: true } });
    expect(ipc.calls.started[0]?.full_auto).toBeUndefined();
    await waitFor(() => expect(screen.getByTestId('mode-picker')).toHaveTextContent('Bypass'));
  });

  it('a new worktree thread runs its agent on the model chosen for it before the first message (#3145)', async () => {
    onTestFinished(forgetKept);
    server.use(http.get(`${BASE}/configurations/models/:project`, () => HttpResponse.json({ items: [{ name: 'gpt-4o', project_id: 42 }, { name: 'gpt-5', project_id: 42 }], total: 2 })));
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    mount(ipc);
    const user = await ready();

    act(() => useDesktopLayout.getState().showInspector('participants'));
    // The row re-renders once the agent's version is read: its chip is read afresh, not held from before.
    const chip = (): HTMLElement => within(screen.getByTestId('thread-settings-model')).getByTestId('model-chip');
    await waitFor(() => expect(chip()).toHaveTextContent('gpt-4o'));
    await user.click(chip());
    await user.click(await screen.findByRole('menuitem', { name: /gpt-5/u }));
    await chooseWorktree(user);
    await sendFirst(user, ipc, 'fix the build');

    expect(ipc.calls.started[0]?.workspace_id).toMatch(/^wt-/u);
    expect(ipc.calls.started[0]?.agent_models).toEqual([{ application_id: 5, name: 'gpt-5' }]);
  });

  it('a thread in the folder itself keeps the confirmed Full auto from its first turn on', async () => {
    onTestFinished(forgetKept);
    window.localStorage.setItem('el.desktop.workspace.w1.workIn', 'folder');
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    mount(ipc);
    const user = await ready();

    await confirmFullAuto(user);
    await sendFirst(user, ipc, 'fix the build');

    expect(ipc.calls.started[0]).toMatchObject({ workspace_id: 'w1', full_auto: true });
    expect(screen.getByTestId('mode-picker')).toHaveTextContent('Full auto');
  });
});
