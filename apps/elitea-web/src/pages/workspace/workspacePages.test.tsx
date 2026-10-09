/**
 * The Workspace pages, driven through the real hooks (generated API clients
 * over MSW) and the in-memory host fake. The routes here are a thin test
 * router; the composition against the REAL route tree is
 * `routes/__tests__/workspacesRoute.test.tsx`.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { WorkspaceIpcProvider } from '@/features/workspace';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';

import { server } from '../../test/setup';
import WorkspaceSessionPage from './WorkspaceSessionPage';
import WorkspacesPage from './WorkspacesPage';

const BASE = '/api/v2';
const PUBLIC_PROJECT_ID = '1';
const globals = globalThis as unknown as Record<string, unknown>;

const FOLDER: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: null, is_git: true };

function mount(ipc: FakeWorkspaceIpc, path: string): void {
  const rootRoute = createRootRoute();
  const list = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: WorkspacesPage });
  const session = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: WorkspaceSessionPage });
  const chat = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => <p>chat page</p> });
  const router = createRouter({
    routeTree: rootRoute.addChildren([list, session, chat]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        <RouterProvider router={router} />
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: PUBLIC_PROJECT_ID,
  };
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/projects/project/default/${PUBLIC_PROJECT_ID}`, () =>
      HttpResponse.json([
        { id: PUBLIC_PROJECT_ID, name: 'Public', suspended: false },
        { id: '42', name: 'Marketing', suspended: false },
      ]),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
  vi.unstubAllEnvs();
});

describe('WorkspacesPage', () => {
  it('lists the opened folders with a git badge and the bound project, and opens a new one', async () => {
    const ipc = createFakeWorkspaceIpc({
      workspaces: [{ ...FOLDER, project_id: 42 }],
      nextOpen: { id: 'w2', path: '/tmp/notes', name: 'notes', project_id: null, is_git: false },
    });
    const user = userEvent.setup();
    mount(ipc, '/workspaces');

    const rows = await screen.findAllByTestId('workspace-row');
    expect(within(rows[0] as HTMLElement).getByText('app')).toBeInTheDocument();
    expect(within(rows[0] as HTMLElement).getByText('/Users/me/code/app')).toBeInTheDocument();
    expect(within(rows[0] as HTMLElement).getByText('Git')).toBeInTheDocument();
    await waitFor(() => expect(within(rows[0] as HTMLElement).getByRole('combobox')).toHaveTextContent('Marketing'));

    await user.click(screen.getByRole('button', { name: 'Open folder' }));
    const after = await screen.findAllByTestId('workspace-row');
    expect(after).toHaveLength(2);
    expect(within(after[1] as HTMLElement).queryByText('Git')).toBeNull();
    // An unbound folder cannot start a session yet.
    expect(within(after[1] as HTMLElement).getByRole('button', { name: 'Open' })).toBeDisabled();
  });

  it('binds a project to a folder through the project picker', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    const user = userEvent.setup();
    mount(ipc, '/workspaces');

    const row = await screen.findByTestId('workspace-row');
    await user.click(within(row).getByRole('combobox'));
    await user.click(await screen.findByRole('option', { name: 'Marketing' }));

    await waitFor(async () => expect((await ipc.list())[0]?.project_id).toBe(42));
    await waitFor(() => expect(within(screen.getByTestId('workspace-row')).getByRole('button', { name: 'Open' })).toBeEnabled());
  });

  it('removes a folder', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    const user = userEvent.setup();
    mount(ipc, '/workspaces');

    await user.click(await screen.findByRole('button', { name: 'Remove app' }));
    expect(await screen.findByText('No folders yet')).toBeInTheDocument();
    expect(await ipc.list()).toEqual([]);
  });
});

describe('WorkspaceSessionPage', () => {
  function serveProject(): void {
    server.use(
      http.get(`${BASE}/elitea_core/applications/prompt_lib/42`, () =>
        HttpResponse.json({ rows: [{ id: '5', name: 'Coder', tags: [], created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', owner_id: '1', is_forked: false, meta: null, has_interrupt: false }], total: 1 }),
      ),
      http.get(`${BASE}/elitea_core/application/prompt_lib/42/5`, () =>
        HttpResponse.json({
          id: '5',
          name: 'Coder',
          description: '',
          icon: '',
          owner_id: '1',
          created_at: '2026-01-01T00:00:00Z',
          versions: [{ id: '9', name: 'base', status: 'published', agent_type: 'openai', created_at: '2026-01-01T00:00:00Z' }],
        }),
      ),
      http.get(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ rows: [], total: 0 })),
      http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })),
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ id: 77, name: 'fix the build' })),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/42/77`, () => HttpResponse.json([])),
    );
  }

  it('asks to bind a project first when the folder has none', async () => {
    mount(createFakeWorkspaceIpc({ workspaces: [FOLDER] }), '/workspaces/w1');
    expect(await screen.findByText('Bind a project to this folder first.')).toBeInTheDocument();
  });

  it('runs a whole turn: creates the conversation, streams, asks for approval, then offers undo and a chat link', async () => {
    serveProject();
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    ipc.setChanges('turn-1', { files: [{ path: 'src/main.rs', status: 'modified', added: 2, removed: 1, diff: '@@ -1 +1 @@\n-a\n+b' }] });
    const user = userEvent.setup();
    mount(ipc, '/workspaces/w1');

    // Only the bound project's agents; the version follows the agent.
    await user.click(await screen.findByRole('combobox', { name: 'Agent' }));
    await user.click(await screen.findByRole('option', { name: 'Coder' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));

    await user.type(screen.getByLabelText('What should the agent do?'), 'fix the build');
    await user.click(screen.getByRole('switch', { name: 'Plan mode (no changes)' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
    expect(ipc.calls.started[0]).toEqual({
      workspace_id: 'w1',
      project_id: 42,
      conversation_id: '77',
      application_id: 5,
      version_id: 9,
      prompt: 'fix the build',
      plan_mode: true,
    });

    const at = (seq: number) => ({ turn_id: 'turn-1', seq });
    ipc.emit({ ...at(1), kind: 'status', payload: { phase: 'running' } });
    ipc.emit({ ...at(2), kind: 'text_delta', payload: { text: 'Looking at the build' } });
    ipc.emit({ ...at(3), kind: 'tool_call', payload: { call_id: 'c1', tool: 'run_command', args_summary: 'cargo build', remote: false } });
    expect(await screen.findByText('Looking at the build')).toBeInTheDocument();
    expect(screen.getByText('Local')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();

    ipc.emit({ ...at(4), kind: 'approval_request', payload: { request_id: 'r1', tool: 'run_command', title: 'Run cargo build', detail: 'Build it', command: ['cargo', 'build'], reason: 'unlisted', can_remember: true } });
    const dialog = await screen.findByRole('dialog', { name: 'Run cargo build' });
    await user.click(within(dialog).getByRole('button', { name: 'Allow once' }));
    expect(ipc.calls.approvals).toEqual([{ requestId: 'r1', decision: 'allow_once' }]);
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());

    ipc.emit({ ...at(5), kind: 'tool_result', payload: { call_id: 'c1', ok: true, summary: 'Finished dev profile', truncated: false } });
    ipc.emit({ ...at(6), kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: ['m1'], changed_files: 1 } });
    ipc.emit({ ...at(7), kind: 'status', payload: { phase: 'done' } });

    expect(await screen.findByText('src/main.rs')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Undo turn' })).toBeInTheDocument();
    const chatLink = screen.getByRole('link', { name: 'Open in chat' });
    expect(chatLink).toHaveAttribute('href', '/chat/77');
  });

  it('cancels the running turn from the transcript', async () => {
    serveProject();
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    const user = userEvent.setup();
    mount(ipc, '/workspaces/w1');

    await user.click(await screen.findByRole('combobox', { name: 'Agent' }));
    await user.click(await screen.findByRole('option', { name: 'Coder' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));
    await user.type(screen.getByLabelText('What should the agent do?'), 'go');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(ipc.calls.started).toHaveLength(1));

    ipc.emit({ turn_id: 'turn-1', seq: 1, kind: 'status', payload: { phase: 'running' } });
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(ipc.calls.cancelled).toEqual(['turn-1']);
  });
});
