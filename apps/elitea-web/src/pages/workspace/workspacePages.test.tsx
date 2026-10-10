/**
 * The Workspace pages, driven through the real hooks (generated API clients
 * over MSW) and the in-memory host fake. The routes here are a thin test
 * router; the composition against the REAL route tree is
 * `routes/__tests__/workspacesRoute.test.tsx`.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { act, render, renderHook, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { WorkspaceIpcProvider, type WorkspaceTurn } from '@/features/workspace';
import type { Application } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import type { AgentEvent, StoredTurn, Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';

import { server } from '../../test/setup';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { useAgentSelection, type AgentSelection } from './useAgentSelection';
import { useSendPrompt } from './useSendPrompt';
import WorkspaceSessionPage from './WorkspaceSessionPage';
import WorkspacesPage from './WorkspacesPage';

const BASE = '/api/v2';
const PUBLIC_PROJECT_ID = '1';
const globals = globalThis as unknown as Record<string, unknown>;

const FOLDER: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: null, is_git: true };

/** Mounts the pages at `path`; answers a function that opens another folder's session. */
function mount(ipc: FakeWorkspaceIpc, path: string): (workspaceId: string) => Promise<void> {
  const rootRoute = createRootRoute();
  const list = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: WorkspacesPage });
  const session = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: WorkspaceSessionPage });
  const chat = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => <p>chat page</p> });
  const createAgent = createRoute({ getParentRoute: () => rootRoute, path: '/agents/create', component: () => <p>agent editor</p> });
  const router = createRouter({
    routeTree: rootRoute.addChildren([list, session, chat, createAgent]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        <RouterProvider router={router} />
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
  return (workspaceId) => router.navigate({ to: '/workspaces/$workspaceId', params: { workspaceId } });
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

  it('says why the project of a folder with a running turn cannot change', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    ipc.failNext('bindProject', 'workspace_busy', 'Wait for the running turn to end.');
    const user = userEvent.setup();
    mount(ipc, '/workspaces');

    const row = await screen.findByTestId('workspace-row');
    await user.click(within(row).getByRole('combobox'));
    await user.click(await screen.findByRole('option', { name: 'Marketing' }));
    expect(await screen.findByText(/An agent is still working in this folder/)).toBeInTheDocument();
    expect((await ipc.list())[0]?.project_id).toBeNull();
  });

  it('removes a folder', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    const user = userEvent.setup();
    mount(ipc, '/workspaces');

    await user.click(await screen.findByRole('button', { name: 'Remove app' }));
    expect(await screen.findByText('Open a folder to start')).toBeInTheDocument();
    expect(await ipc.list()).toEqual([]);
  });

  it('says why a folder with a running turn cannot be removed', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    ipc.failNext('remove', 'workspace_busy', 'A turn is already running in this workspace.');
    const user = userEvent.setup();
    mount(ipc, '/workspaces');

    await user.click(await screen.findByRole('button', { name: 'Remove app' }));
    expect(await screen.findByText(/An agent is still working in this folder/)).toBeInTheDocument();
    expect(await ipc.list()).toHaveLength(1);
  });
});

describe('WorkspaceSessionPage', () => {
  const createdConversations: unknown[] = [];
  function serveProject(): void {
    createdConversations.length = 0;
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
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, async ({ request }) => {
        createdConversations.push(await request.json());
        return HttpResponse.json({ id: 77, name: 'fix the build' });
      }),
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

    await user.type(screen.getByTestId('chat-message-input'), 'fix the build');
    await user.click(screen.getByRole('switch', { name: 'Plan mode (no changes)' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
    // Marked as Local work, with the folder's name (not its path): web Chats keeps it apart and read-only.
    expect(createdConversations).toEqual([
      expect.objectContaining({ source: 'local_work', meta: { local_work: { folder_name: FOLDER.name } }, is_private: true }),
    ]);
    expect(ipc.calls.started[0]).toEqual({
      workspace_id: 'w1',
      project_id: 42,
      conversation_id: '77',
      application_id: 5,
      version_id: 9,
      prompt: 'fix the build',
      plan_mode: true,
      mentions: [],
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

  function recorded(turnId: string, overrides: Partial<StoredTurn> = {}): StoredTurn {
    const at = (seq: number) => ({ turn_id: turnId, seq });
    const events: AgentEvent[] = [
      { ...at(0), kind: 'status', payload: { phase: 'running' } },
      { ...at(2), kind: 'text_delta', payload: { text: `Answer of ${turnId}` } },
      { ...at(3), kind: 'tool_call', payload: { call_id: 'c1', tool: 'write_file', args_summary: 'notes.txt', remote: false } },
      { ...at(4), kind: 'tool_result', payload: { call_id: 'c1', ok: true, summary: 'written', truncated: false } },
      { ...at(5), kind: 'status', payload: { phase: 'done' } },
      { ...at(6), kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: [], changed_files: 1 } },
    ];
    return {
      turn_id: turnId,
      conversation_id: '77',
      conversation_uuid: null,
      prompt: `Prompt of ${turnId}`,
      mentions: [],
      started_at: 1,
      finished_at: 2,
      events,
      changes: [{ path: `${turnId}.txt`, status: 'added', added: 1, removed: 0, diff: '+hi' }],
      events_truncated: false,
      state: 'done',
      live: false,
      ...overrides,
    };
  }

  it('reopens a thread with every turn recorded on this computer, and every turn\'s changes in the panel', async () => {
    serveProject();
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    ipc.setHistory('w1', '77', [recorded('t1'), recorded('t2', { live: true, state: 'interrupted', events: recorded('t2').events.slice(0, 2) })]);
    ipc.setChanges('t2', { files: [{ path: 't2-live.txt', status: 'modified', added: 2, removed: 1, diff: '' }] });
    mount(ipc, '/workspaces/w1?conversation=77');

    const transcript = await screen.findByTestId('session-transcript');
    expect(await within(transcript).findByText('Prompt of t1')).toBeInTheDocument();
    expect(within(transcript).getByText('Answer of t1')).toBeInTheDocument();
    expect(within(transcript).getByText('Prompt of t2')).toBeInTheDocument();
    expect(screen.getAllByTestId('history-turn')).toHaveLength(2);
    expect(screen.getAllByTestId('tool-row')).toHaveLength(1);
    expect(screen.getByText(/did not finish/)).toBeInTheDocument();
    expect(screen.queryByText(/Earlier messages of this thread/)).toBeNull();
    expect(screen.getByRole('link', { name: 'Open in chat' })).toHaveAttribute('href', '/chat/77');

    // The panel: the live turn from the host (with undo), the older one as recorded (without).
    const sets = await screen.findAllByTestId('panel-turn-changes');
    expect(sets).toHaveLength(2);
    expect(await within(sets[0] as HTMLElement).findByText('t2-live.txt')).toBeInTheDocument();
    expect(within(sets[0] as HTMLElement).getByRole('button', { name: 'Undo turn' })).toBeInTheDocument();
    expect(within(sets[0] as HTMLElement).getByText('Prompt of t2')).toBeInTheDocument();
    expect(within(sets[1] as HTMLElement).getByText('t1.txt')).toBeInTheDocument();
    expect(within(sets[1] as HTMLElement).queryByRole('button', { name: 'Undo turn' })).toBeNull();
  });

  it('takes over a turn still running from an earlier visit', async () => {
    serveProject();
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    ipc.setHistory('w1', '77', [recorded('t9', { state: 'running', live: true, finished_at: null, events: recorded('t9').events.slice(0, 2) })]);
    mount(ipc, '/workspaces/w1?conversation=77');

    expect(await screen.findByText('Answer of t9')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
    expect(screen.queryAllByTestId('history-turn')).toHaveLength(0);
    // A live event the record already holds is not shown twice; a new one appends.
    ipc.emit({ turn_id: 't9', seq: 2, kind: 'text_delta', payload: { text: 'Answer of t9' } });
    ipc.emit({ turn_id: 't9', seq: 3, kind: 'text_delta', payload: { text: ', and more' } });
    expect(await screen.findByText('Answer of t9, and more')).toBeInTheDocument();
    ipc.emit({ turn_id: 't9', seq: 4, kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: [], changed_files: 0 } });
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull());
    expect(screen.getByText('Prompt of t9')).toBeInTheDocument();
  });

  it('shows the conversation\'s messages from the server when this computer recorded none', async () => {
    serveProject();
    server.use(
      http.get(`${BASE}/elitea_core/messages/prompt_lib/42/77`, () =>
        HttpResponse.json({
          items: [
            { id: 'm2', uid: 'u2', conversation_id: '77', role: 'assistant', content: 'The **answer** from the web' },
            { id: 'm1', uid: 'u1', conversation_id: '77', role: 'user', content: 'A question asked on the web' },
          ],
          total: 2,
          page: 1,
          page_size: 100,
          total_pages: 1,
        }),
      ),
    );
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    mount(ipc, '/workspaces/w1?conversation=77');

    const question = await screen.findByText('A question asked on the web');
    const answer = await screen.findByTestId('history-answer');
    expect(within(answer).getByText('answer').tagName).toBe('STRONG');
    // Reading order: the question first.
    expect(question.compareDocumentPosition(answer) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByText(/Earlier messages of this thread/)).toBeNull();
  });

  it('points to the chat when neither this computer nor the server can say what came before', async () => {
    serveProject();
    server.use(http.get(`${BASE}/elitea_core/messages/prompt_lib/42/77`, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    ipc.failNext('threadHistory', 'not_signed_in', 'Sign in');
    mount(ipc, '/workspaces/w1?conversation=77');
    expect(await screen.findByText(/Earlier messages of this thread are in the conversation/)).toBeInTheDocument();
  });

  it('keeps the prompt when the host refuses the start', async () => {
    serveProject();
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    ipc.failNext('startTurn', 'local_work_disabled', 'Local work is turned off by your organisation’s policy.');
    const user = userEvent.setup();
    mount(ipc, '/workspaces/w1');

    await user.click(await screen.findByRole('combobox', { name: 'Agent' }));
    await user.click(await screen.findByRole('option', { name: 'Coder' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));
    await user.type(screen.getByTestId('chat-message-input'), 'fix the build');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    expect(await screen.findByText('Local work is turned off by your organisation’s policy.')).toBeInTheDocument();
    expect(ipc.calls.started).toHaveLength(1);
    expect(screen.getByTestId('chat-message-input')).toHaveValue('fix the build');
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled();
  });

  it('retries a refused start in the conversation it already created, not a new one', async () => {
    serveProject();
    let created = 0;
    server.use(
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => {
        created += 1;
        return HttpResponse.json({ id: 77, name: 'fix the build' });
      }),
    );
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    ipc.failNext('startTurn', 'workspace_busy', 'A turn is already running in this workspace.');
    const user = userEvent.setup();
    mount(ipc, '/workspaces/w1');

    await user.click(await screen.findByRole('combobox', { name: 'Agent' }));
    await user.click(await screen.findByRole('option', { name: 'Coder' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));
    await user.type(screen.getByTestId('chat-message-input'), 'fix the build');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByText(/An agent is still working in this folder/)).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(ipc.calls.started).toHaveLength(2));
    expect(ipc.calls.started.map((r) => r.conversation_id)).toEqual(['77', '77']);
    expect(created).toBe(1);
  });

  it('retries a failed participants add in the conversation it already created, adding them again', async () => {
    serveProject();
    let created = 0;
    let adds = 0;
    server.use(
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => {
        created += 1;
        return HttpResponse.json({ id: 77, name: 'fix the build' });
      }),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/42/77`, () => {
        adds += 1;
        return adds === 1 ? HttpResponse.json({ error: 'unavailable' }, { status: 503 }) : HttpResponse.json([]);
      }),
    );
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    const user = userEvent.setup();
    mount(ipc, '/workspaces/w1');

    await user.click(await screen.findByRole('combobox', { name: 'Agent' }));
    await user.click(await screen.findByRole('option', { name: 'Coder' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));
    await user.type(screen.getByTestId('chat-message-input'), 'fix the build');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(adds).toBe(1));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled());
    expect(ipc.calls.started).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
    expect(ipc.calls.started[0]?.conversation_id).toBe('77');
    expect(created).toBe(1);
    expect(adds).toBe(2);
  });

  it('starts afresh in another folder: the first folder\'s running turn does not follow', async () => {
    serveProject();
    const other: Workspace = { id: 'w2', path: '/Users/me/code/lib', name: 'lib', project_id: 42, is_git: false };
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }, other] });
    const user = userEvent.setup();
    const openFolder = mount(ipc, '/workspaces/w1');

    await user.click(await screen.findByRole('combobox', { name: 'Agent' }));
    await user.click(await screen.findByRole('option', { name: 'Coder' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));
    await user.type(screen.getByTestId('chat-message-input'), 'go');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
    ipc.emit({ turn_id: 'turn-1', seq: 1, kind: 'text_delta', payload: { text: 'working in app' } });
    expect(await screen.findByText('working in app')).toBeInTheDocument();
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));

    await act(() => openFolder('w2'));
    expect(await screen.findByText('/Users/me/code/lib')).toBeInTheDocument();
    expect(screen.queryByText('working in app')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
    // One listener: the old session's was removed with it.
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
  });

  it('cancels the running turn from the transcript', async () => {
    serveProject();
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    const user = userEvent.setup();
    mount(ipc, '/workspaces/w1');

    await user.click(await screen.findByRole('combobox', { name: 'Agent' }));
    await user.click(await screen.findByRole('option', { name: 'Coder' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));
    await user.type(screen.getByTestId('chat-message-input'), 'go');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(ipc.calls.started).toHaveLength(1));

    ipc.emit({ turn_id: 'turn-1', seq: 1, kind: 'status', payload: { phase: 'running' } });
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(ipc.calls.cancelled).toEqual(['turn-1']);
  });

  describe('useSendPrompt', () => {
    function selection(picks: string[]): AgentSelection {
      return {
        agents: [{ id: '5', name: 'Coder' } as Application],
        versions: [{ id: 9, name: 'base', agentType: 'openai' }],
        conversations: [{ id: 77, name: 'fix the build' }],
        agentId: '5',
        versionId: '9',
        // An existing conversation: the send reaches the host without creating one.
        conversationId: '77',
        loading: false,
        empty: false,
        selectAgent: () => undefined,
        selectVersion: () => undefined,
        selectConversation: (id) => {
          picks.push(id);
        },
      };
    }

    function turn(started: boolean): WorkspaceTurn {
      return {
        view: { phase: null, items: [], approvals: [] },
        turnId: null,
        earlier: [],
        busy: false,
        startError: null,
        start: () => Promise.resolve(started),
        cancel: () => Promise.resolve(),
        answer: () => Promise.resolve(),
        clear: () => undefined,
        adopt: () => undefined,
      };
    }

    async function send(started: boolean): Promise<{ sent: boolean; picks: string[] }> {
      serveProject();
      const picks: string[] = [];
      const { result } = renderHook(() => useSendPrompt({ ...FOLDER, project_id: 42 }, 42, selection(picks), turn(started)), {
        wrapper: ({ children }) => <AppProviders>{children}</AppProviders>,
      });
      let sent = false;
      await act(async () => {
        sent = await result.current.send('fix the build', false, []);
      });
      return { sent, picks };
    }

    it('reports a refused start as not sent and leaves the conversation choice alone', async () => {
      expect(await send(false)).toEqual({ sent: false, picks: [] });
    });

    it('selects the conversation the turn runs in once the host started it', async () => {
      expect(await send(true)).toEqual({ sent: true, picks: ['77'] });
    });

    it('sends once when Send is pressed twice while the conversation is being created', async () => {
      serveProject();
      let created = 0;
      server.use(
        http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => {
          created += 1;
          return HttpResponse.json({ id: 77, name: 'fix the build' });
        }),
      );
      const starts: string[] = [];
      const fresh: AgentSelection = { ...selection([]), conversationId: '' };
      const running: WorkspaceTurn = {
        ...turn(true),
        start: (request) => {
          starts.push(request.conversation_id);
          return Promise.resolve(true);
        },
      };
      const { result } = renderHook(
        () => ({ prompt: useSendPrompt({ ...FOLDER, project_id: 42 }, 42, fresh, running), author: useGetCurrentAuthor() }),
        { wrapper: ({ children }) => <AppProviders>{children}</AppProviders> },
      );
      // A conversation is created for the signed-in user: wait until it is known.
      await waitFor(() => expect(result.current.author.isSuccess).toBe(true));
      let outcomes: boolean[] = [];
      await act(async () => {
        const { send } = result.current.prompt;
        outcomes = await Promise.all([send('fix the build', false, []), send('fix the build', false, [])]);
      });
      expect(outcomes).toEqual([true, false]);
      expect(created).toBe(1);
      expect(starts).toEqual(['77']);
    });
  });

  it('starts a new conversation when the agent or the version changes', async () => {
    serveProject();
    const { result } = renderHook(() => useAgentSelection('w1', 42), { wrapper: ({ children }) => <AppProviders>{children}</AppProviders> });
    act(() => result.current.selectAgent('5'));
    await waitFor(() => expect(result.current.versionId).toBe('9'));

    act(() => result.current.selectConversation('77'));
    act(() => result.current.selectVersion('9'));
    expect(result.current.conversationId).toBe('');

    act(() => result.current.selectConversation('77'));
    act(() => result.current.selectAgent('5'));
    expect(result.current.conversationId).toBe('');
  });

  describe('start experience', () => {
    function serveAgents(rows: { id: string; name: string }[]): void {
      server.use(
        http.get(`${BASE}/elitea_core/applications/prompt_lib/42`, () =>
          HttpResponse.json({
            rows: rows.map((row) => ({ ...row, tags: [], created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', owner_id: '1', is_forked: false, meta: null, has_interrupt: false })),
            total: rows.length,
          }),
        ),
        http.get(`${BASE}/elitea_core/application/prompt_lib/42/:id`, ({ params }) =>
          HttpResponse.json({
            id: String(params['id']),
            name: rows.find((row) => row.id === params['id'])?.name ?? '',
            description: '',
            icon: '',
            owner_id: '1',
            created_at: '2026-01-01T00:00:00Z',
            versions: [
              { id: `${String(params['id'])}1`, name: 'base', status: 'published', agent_type: 'openai', created_at: '2026-01-01T00:00:00Z' },
              { id: `${String(params['id'])}2`, name: 'v2', status: 'published', agent_type: 'openai', created_at: '2026-01-01T00:00:00Z' },
            ],
          }),
        ),
        http.get(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ rows: [], total: 0 })),
      );
    }

    afterEach(() => {
      window.localStorage.clear();
    });

    it('explains an empty project and offers to create an agent there or bind another project', async () => {
      serveAgents([]);
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1');

      const empty = await screen.findByTestId('workspace-no-agents');
      expect(within(empty).getByText('This project has no agents')).toBeInTheDocument();
      expect(screen.queryByRole('combobox', { name: 'Agent' })).toBeNull();
      await waitFor(() => expect(screen.getByRole('combobox', { name: 'Project' })).toHaveTextContent('Marketing'));

      await user.click(within(empty).getByRole('button', { name: 'Use another project' }));
      await user.click(await screen.findByRole('option', { name: 'Public' }));
      await waitFor(async () => expect((await ipc.list())[0]?.project_id).toBe(1));
    });

    it('creates the agent in the bound project, without creating anything itself', async () => {
      serveAgents([]);
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1');

      const empty = await screen.findByTestId('workspace-no-agents');
      await waitFor(() => expect(screen.getByRole('combobox', { name: 'Project' })).toHaveTextContent('Marketing'));
      await user.click(within(empty).getByRole('button', { name: 'Create an agent' }));
      expect(await screen.findByText('agent editor')).toBeInTheDocument();
      expect(useSelectedProjectStore.getState().project).toEqual({ id: '42', name: 'Marketing' });
      expect(ipc.calls.started).toEqual([]);
    });

    it('preselects the first agent, then the agent and version last used in this workspace', async () => {
      serveAgents([
        { id: '5', name: 'Coder' },
        { id: '6', name: 'Reviewer' },
      ]);
      const first = renderHook(() => useAgentSelection('w1', 42), { wrapper: ({ children }) => <AppProviders>{children}</AppProviders> });
      await waitFor(() => expect(first.result.current.versionId).toBe('51'));
      expect(first.result.current.agentId).toBe('5');
      act(() => first.result.current.selectAgent('6'));
      await waitFor(() => expect(first.result.current.versionId).toBe('61'));
      act(() => first.result.current.selectVersion('62'));
      await waitFor(() => expect(window.localStorage.getItem('el.desktop.workspace.w1.agent')).toContain('"versionId":"62"'));
      first.unmount();

      const again = renderHook(() => useAgentSelection('w1', 42), { wrapper: ({ children }) => <AppProviders>{children}</AppProviders> });
      await waitFor(() => expect(again.result.current.versionId).toBe('62'));
      expect(again.result.current.agentId).toBe('6');
      again.unmount();
      // Another workspace keeps its own default.
      const other = renderHook(() => useAgentSelection('w2', 42), { wrapper: ({ children }) => <AppProviders>{children}</AppProviders> });
      await waitFor(() => expect(other.result.current.versionId).toBe('51'));
    });
  });

  describe('composer', () => {
    const FILES = [
      { path: 'src', kind: 'dir' as const },
      { path: 'src/main.rs', kind: 'file' as const },
      { path: 'README.md', kind: 'file' as const },
    ];

    async function ready(ipc: FakeWorkspaceIpc): Promise<ReturnType<typeof userEvent.setup>> {
      serveProject();
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1');
      await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));
      return user;
    }

    it('looks up "@" on the host, inserts the picked path and sends it as a mention', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }], files: FILES });
      const user = await ready(ipc);
      const input = screen.getByTestId('chat-message-input');

      await user.type(input, 'look at @mai');
      const menu = await screen.findByTestId('workspace-file-menu');
      expect(within(menu).getByRole('option', { name: 'src/main.rs' })).toBeInTheDocument();
      expect(ipc.calls.fileQueries.at(-1)).toEqual({ workspaceId: 'w1', query: 'mai', limit: 50 });
      await user.keyboard('{Enter}');
      await waitFor(() => expect(input).toHaveValue('look at @src/main.rs '));
      expect(screen.queryByTestId('workspace-file-menu')).toBeNull();

      // A folder, picked with the mouse, keeps its "/".
      await user.type(input, 'and @sr');
      await user.click(await screen.findByRole('option', { name: 'src/' }));
      await waitFor(() => expect(input).toHaveValue('look at @src/main.rs and @src/ '));

      await user.keyboard('{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.mentions).toEqual(['src/main.rs', 'src/']);
      expect(ipc.calls.started[0]?.prompt).toBe('look at @src/main.rs and @src/ ');
      await waitFor(() => expect(input).toHaveValue(''));
    });

    it('moves through the "@" menu with the arrows, closes on Esc, and drops a deleted reference', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }], files: FILES });
      const user = await ready(ipc);
      const input = screen.getByTestId('chat-message-input');

      await user.type(input, '@');
      const menu = await screen.findByTestId('workspace-file-menu');
      expect(within(menu).getAllByRole('option')).toHaveLength(3);
      await user.keyboard('{ArrowDown}{ArrowDown}');
      expect(within(menu).getByRole('option', { name: 'README.md' })).toHaveAttribute('aria-selected', 'true');
      await user.keyboard('{ArrowUp}{Enter}');
      await waitFor(() => expect(input).toHaveValue('@src/main.rs '));

      await user.type(input, '@READ');
      await screen.findByTestId('workspace-file-menu');
      await user.keyboard('{Escape}');
      await waitFor(() => expect(screen.queryByTestId('workspace-file-menu')).toBeNull());

      // The picked reference is edited away before sending: no mention goes out.
      await user.clear(input);
      await user.type(input, 'just text');
      await user.keyboard('{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.mentions).toEqual([]);
    });

    it('Shift+Enter breaks the line and Enter sends', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc);
      const input = screen.getByTestId('chat-message-input');
      await user.type(input, 'one{Shift>}{Enter}{/Shift}two');
      expect(input).toHaveValue('one\ntwo');
      expect(ipc.calls.started).toHaveLength(0);
      await user.keyboard('{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.prompt).toBe('one\ntwo');
    });

    // Six commands, a turn and a new thread in one test: the default 5 s is not
    // enough under the coverage shard's instrumentation (30 s below).
    it('runs the "/" commands: plan, help, agent, new, clear and undo', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      ipc.setChanges('turn-1', { files: [{ path: 'a.txt', status: 'modified', added: 1, removed: 1, diff: '' }] });
      const user = await ready(ipc);
      const input = screen.getByTestId('chat-message-input');

      await user.type(input, '/');
      const menu = await screen.findByTestId('workspace-command-menu');
      expect(within(menu).getAllByRole('option').map((o) => o.getAttribute('aria-label'))).toEqual(['/new', '/plan', '/undo', '/agent', '/clear', '/help']);
      await user.type(input, 'pl{Enter}');
      expect(screen.getByRole('switch', { name: 'Plan mode (no changes)' })).toBeChecked();
      await waitFor(() => expect(input).toHaveValue(''));

      await user.type(input, '/help{Enter}');
      expect(within(await screen.findByTestId('workspace-help')).getByText('/undo')).toBeInTheDocument();

      await user.type(input, '/agent{Enter}');
      await waitFor(() => expect(screen.getByRole('combobox', { name: 'Agent' })).toHaveFocus());

      // Nothing to undo yet.
      await user.click(input);
      await user.type(input, '/undo{Enter}');
      expect(await screen.findByText('The last turn here changed no files.')).toBeInTheDocument();

      // A turn that changed a file: /undo asks the same confirmation as the card's button.
      await user.type(input, 'go{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.plan_mode).toBe(true);
      ipc.emit({ turn_id: 'turn-1', seq: 1, kind: 'status', payload: { phase: 'running' } });
      ipc.emit({ turn_id: 'turn-1', seq: 2, kind: 'text_delta', payload: { text: 'changed a.txt' } });
      ipc.emit({ turn_id: 'turn-1', seq: 3, kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: [], changed_files: 1 } });
      expect(await screen.findByText('a.txt')).toBeInTheDocument();
      await user.type(input, '/undo{Enter}');
      const dialog = await screen.findByRole('dialog', { name: 'Undo this turn?' });
      await user.click(within(dialog).getByRole('button', { name: 'Undo turn' }));
      await waitFor(() => expect(ipc.calls.restores).toEqual([{ turnId: 'turn-1' }]));

      await user.type(input, '/clear{Enter}');
      await waitFor(() => expect(screen.queryByText('changed a.txt')).toBeNull());
      expect(screen.queryByText('a.txt')).toBeNull();

      // /new: the next send starts a new conversation again.
      let created = 0;
      server.use(
        http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => {
          created += 1;
          return HttpResponse.json({ id: 78, name: 'again' });
        }),
        http.post(`${BASE}/elitea_core/participants/prompt_lib/42/78`, () => HttpResponse.json([])),
      );
      await user.type(input, '/new{Enter}');
      // A new thread is a fresh session (its own composer), with the agent last used here.
      await waitFor(() => expect(screen.getByTestId('chat-message-input')).not.toBe(input));
      await waitFor(() => expect(screen.getByRole('combobox', { name: 'Version' })).toHaveTextContent('base'));
      await user.type(screen.getByTestId('chat-message-input'), 'again{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(2));
      expect(created).toBe(1);
      expect(ipc.calls.started[1]?.conversation_id).toBe('78');
    }, 30_000);

    it('does not open the command menu for a "/" inside the text', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc);
      await user.type(screen.getByTestId('chat-message-input'), 'fix src/');
      expect(screen.queryByTestId('workspace-command-menu')).toBeNull();
    });

    it('shows when the turn applied AGENTS.md', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc);
      await user.type(screen.getByTestId('chat-message-input'), 'go{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      ipc.emit({ turn_id: 'turn-1', seq: 1, kind: 'status', payload: { phase: 'running', project_instructions: ['AGENTS.md', 'apps/web/AGENTS.md'] } });
      const chip = await screen.findByTestId('agents-md-applied');
      expect(chip).toHaveTextContent('AGENTS.md applied');
      expect(chip).toHaveAttribute('aria-description', 'AGENTS.md, apps/web/AGENTS.md');
    });
  });
});
