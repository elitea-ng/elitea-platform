/**
 * The Workspace pages, driven through the real hooks (generated API clients
 * over MSW) and the in-memory host fake. The routes here are a thin test
 * router; the composition against the REAL route tree is
 * `routes/__tests__/workspacesRoute.test.tsx`.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { act, configure, fireEvent, getConfig, render, renderHook, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { readThreads, recordThread, WorkspaceIpcProvider, type WorkspaceTurn } from '@/features/workspace';
import { IndexIpcProvider } from '@/features/workspace-index';
import type { Application } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import type { AgentEvent, StoredTurn, Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { createFakeIndexIpc, type FakeIndexIpc } from '@/shared/desktop/indexIpc.fake';
import { createFakeAppIpc, type FakeAppIpc } from '@/shared/desktop/appEvents.fake';
import { AppIpcProvider, InspectorScope } from '@/widgets/desktop-shell';

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
function mount(ipc: FakeWorkspaceIpc, path: string, indexIpc?: FakeIndexIpc, appIpc?: FakeAppIpc): (workspaceId: string) => Promise<void> {
  const rootRoute = createRootRoute();
  const list = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: WorkspacesPage });
  const session = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: WorkspaceSessionPage });
  const chat = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => <p>chat page</p> });
  const createAgent = createRoute({ getParentRoute: () => rootRoute, path: '/agents/create', component: () => <p>agent editor</p> });
  const router = createRouter({
    routeTree: rootRoute.addChildren([list, session, chat, createAgent]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  // The frame's inspector beside the pages (the thread's Changes, Approvals and Index tabs), without the rest of the frame.
  const pages = (
    <InspectorScope>
      <RouterProvider router={router} />
    </InspectorScope>
  );
  const withIndex = indexIpc === undefined ? pages : <IndexIpcProvider ipc={indexIpc}>{pages}</IndexIpcProvider>;
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>{appIpc === undefined ? withIndex : <AppIpcProvider ipc={appIpc}>{withIndex}</AppIpcProvider>}</WorkspaceIpcProvider>
    </AppProviders>,
  );
  return (workspaceId) => router.navigate({ to: '/workspaces/$workspaceId', params: { workspaceId } });
}

/*
 * Deadlines sized for a busy machine. These tests drive whole pages with
 * real timers (MUI selects, typing, dialogs): "runs a whole turn" takes
 * 0.6 s alone, with a cold or a warm transform cache alike (no lazy import
 * is loaded mid-test, and the page's only intervals are 15 s and longer), but
 * 5.4 s while `vitest run src/features` runs beside it: CPU contention,
 * which broke both the 5 s test timeout and testing-library's 1 s
 * `waitFor`/`findBy` deadline. Measured 2026-10-15; see DEVIATIONS
 * "desktop-followup".
 */
vi.setConfig({ testTimeout: 30_000 });
let asyncUtilTimeout = 1000;
beforeAll(() => {
  asyncUtilTimeout = getConfig().asyncUtilTimeout;
  configure({ asyncUtilTimeout: 10_000 });
});
afterAll(() => {
  configure({ asyncUtilTimeout });
});

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
    // What the composer's model chip reads: the agent version's own model, and the project's chat models.
    http.get(`${BASE}/elitea_core/version/prompt_lib/:project/:app/:version`, ({ params }) =>
      HttpResponse.json({ id: Number(params['version']), name: 'base', llm_settings: { model_name: 'gpt-4o', pin_model: pinModel } }),
    ),
    http.get(`${BASE}/configurations/models/:project`, () =>
      HttpResponse.json({
        items: [
          { name: 'gpt-4o', project_id: 42, supports_reasoning: false },
          { name: 'gpt-5', project_id: 42, description: 'Thinks first', supports_reasoning: true },
        ],
        total: 2,
      }),
    ),
  );
});
/** The agent version pins its model (`llm_settings.pin_model`). */
let pinModel = false;

afterEach(() => {
  pinModel = false;
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
  vi.unstubAllEnvs();
});

/** Picks the "Coder" agent in the composer footer's picker. */
async function pickCoder(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByTestId('agent-chip'));
  await user.click(await screen.findByRole('menuitem', { name: 'Coder' }));
  await waitFor(() => expect(screen.getByTestId('agent-chip')).toHaveTextContent('Coder · base'));
}

describe('WorkspacesPage', () => {
  it('lists the opened folders as a table with a git badge and the bound project, and opens a new one', async () => {
    const ipc = createFakeWorkspaceIpc({
      workspaces: [{ ...FOLDER, project_id: 42 }],
      nextOpen: { id: 'w2', path: '/tmp/notes', name: 'notes', project_id: null, is_git: false },
    });
    const user = userEvent.setup();
    mount(ipc, '/workspaces');

    expect(await screen.findByRole('table', { name: 'Local work' })).toBeInTheDocument();
    const rows = await screen.findAllByTestId('workspace-row');
    expect(within(rows[0] as HTMLElement).getByText('app')).toBeInTheDocument();
    expect(within(rows[0] as HTMLElement).getByText('/Users/me/code/app')).toBeInTheDocument();
    expect(within(rows[0] as HTMLElement).getByText('Git')).toBeInTheDocument();
    // No host index client (a plain browser tab): no index chip.
    expect(within(rows[0] as HTMLElement).queryByTestId('index-status-chip')).toBeNull();
    await waitFor(() => expect(within(rows[0] as HTMLElement).getByText('Marketing')).toBeInTheDocument());

    await user.click(screen.getByRole('button', { name: 'Open folder' }));
    const after = await screen.findAllByTestId('workspace-row');
    expect(after).toHaveLength(2);
    expect(within(after[1] as HTMLElement).queryByText('Git')).toBeNull();
    // An unbound folder says what it needs, and does not open.
    expect(within(after[1] as HTMLElement).getByText('Bind a project to work here')).toBeInTheDocument();
    await user.dblClick(after[1] as HTMLElement);
    expect(screen.getByTestId('workspaces-page')).toBeInTheDocument();
  });

  it('opens a folder with a double click or Return, selecting rows with the arrow keys', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }, { ...FOLDER, id: 'w2', name: 'notes', project_id: 42 }] });
    const user = userEvent.setup();
    mount(ipc, '/workspaces');
    const rows = await screen.findAllByTestId('workspace-row');
    await user.click(rows[0] as HTMLElement);
    expect(rows[0]).toHaveAttribute('aria-selected', 'true');
    await user.keyboard('{ArrowDown}');
    expect(rows[1]).toHaveAttribute('aria-selected', 'true');
    expect(rows[1]).toHaveFocus();
    await user.dblClick(rows[1] as HTMLElement);
    await waitFor(() => expect(screen.queryByTestId('workspaces-page')).toBeNull());
  });

  it('shows each folder\'s code index on its row', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER, { ...FOLDER, id: 'w2', name: 'notes' }] });
    mount(ipc, '/workspaces', createFakeIndexIpc({ statuses: { w1: { state: 'ready', entities: 42 } } }));
    const rows = await screen.findAllByTestId('workspace-row');
    await waitFor(() => expect(within(rows[0] as HTMLElement).getByTestId('index-status-chip')).toHaveTextContent('Ready · 42 entities'));
    await waitFor(() => expect(within(rows[1] as HTMLElement).getByTestId('index-status-chip')).toHaveTextContent('Off'));
  });

  it('never opens or refreshes an index from the list (every row reads its status only)', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER, { ...FOLDER, id: 'w2', name: 'notes' }] });
    const indexIpc = createFakeIndexIpc({ statuses: { w1: { state: 'stale', entities: 42 }, w2: { state: 'stale', entities: 7 } } });
    mount(ipc, '/workspaces', indexIpc);
    const rows = await screen.findAllByTestId('workspace-row');
    await waitFor(() => expect(within(rows[1] as HTMLElement).getByTestId('index-status-chip')).toHaveTextContent('Stale'));
    expect(indexIpc.calls.opened).toEqual([]);
    expect(indexIpc.calls.refreshes).toEqual([]);
  });

  it('puts a folder\'s actions in its native context menu', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
    const appIpc = createFakeAppIpc();
    mount(ipc, '/workspaces', undefined, appIpc);
    const row = await screen.findByTestId('workspace-row');
    await waitFor(() => expect(within(row).getByText('Marketing')).toBeInTheDocument());
    fireEvent.contextMenu(row);
    await waitFor(() => expect(appIpc.calls.contextMenus).toHaveLength(1));
    const labels = (appIpc.calls.contextMenus[0] ?? []).map((item) => ('label' in item ? item.label : '-'));
    expect(labels).toEqual(['Open', '-', 'Reveal in Finder', 'Copy Path', '-', 'Project', '-', 'Remove from sidebar']);
    const submenu = (appIpc.calls.contextMenus[0] ?? []).find((item) => 'children' in item);
    expect(submenu !== undefined && 'children' in submenu ? submenu.children.map((c) => c.label) : []).toEqual(['Public', '✓ Marketing']);
  });

  it('binds a project to a folder from its context menu', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    const appIpc = createFakeAppIpc();
    mount(ipc, '/workspaces', undefined, appIpc);

    const row = await screen.findByTestId('workspace-row');
    appIpc.chooseFromContextMenu('project:42');
    fireEvent.contextMenu(row);

    await waitFor(async () => expect((await ipc.list())[0]?.project_id).toBe(42));
    await waitFor(() => expect(within(screen.getByTestId('workspace-row')).getByText('Marketing')).toBeInTheDocument());
  });

  it('says why the project of a folder with a running turn cannot change', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    ipc.failNext('bindProject', 'workspace_busy', 'Wait for the running turn to end.');
    const appIpc = createFakeAppIpc();
    mount(ipc, '/workspaces', undefined, appIpc);

    const row = await screen.findByTestId('workspace-row');
    appIpc.chooseFromContextMenu('project:42');
    fireEvent.contextMenu(row);
    expect(await screen.findByText(/An agent is still working in this folder/)).toBeInTheDocument();
    expect((await ipc.list())[0]?.project_id).toBeNull();
  });

  it('removes a folder from its context menu', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    const appIpc = createFakeAppIpc();
    mount(ipc, '/workspaces', undefined, appIpc);

    appIpc.chooseFromContextMenu('remove');
    fireEvent.contextMenu(await screen.findByTestId('workspace-row'));
    expect(await screen.findByText('Open a folder to start')).toBeInTheDocument();
    expect(await ipc.list()).toEqual([]);
  });

  it('says why a folder with a running turn cannot be removed', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [FOLDER] });
    ipc.failNext('remove', 'workspace_busy', 'A turn is already running in this workspace.');
    const appIpc = createFakeAppIpc();
    mount(ipc, '/workspaces', undefined, appIpc);

    appIpc.chooseFromContextMenu('remove');
    fireEvent.contextMenu(await screen.findByTestId('workspace-row'));
    expect(await screen.findByText(/An agent is still working in this folder/)).toBeInTheDocument();
    expect(await ipc.list()).toHaveLength(1);
  });
});

describe('WorkspaceSessionPage', () => {
  const createdConversations: unknown[] = [];
  function serveProject(skills: { id: string; name: string; description?: string }[] = []): void {
    createdConversations.length = 0;
    server.use(
      http.get(`${BASE}/elitea_core/application_skills/prompt_lib/42/9`, () =>
        HttpResponse.json({ items: skills.map((s) => ({ ...s, project_id: '42', type: 'skill' })), total: skills.length, page: 1, page_size: skills.length, total_pages: 1 }),
      ),
      // The project's own skills (the "/" menu's "Project skills"): none unless a test serves some.
      http.get(`${BASE}/elitea_core/skills/prompt_lib/42`, () => HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100, total_pages: 0 })),
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

  it('shows the folder\'s code index in the inspector, opened from the toolbar menu', async () => {
    serveProject();
    const indexIpc = createFakeIndexIpc({ statuses: { w1: { state: 'stale' } } });
    mount(createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] }), '/workspaces/w1', indexIpc);
    const session = await screen.findByTestId('workspace-session');
    // The session page opens the index, which checks the folder; its settings are in the ⋯ menu.
    await waitFor(() => expect(indexIpc.calls.opened).toEqual(['w1']));
    await userEvent.click(within(session).getByRole('button', { name: 'More' }));
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Index settings…' }));
    // The inspector opens on its Index tab.
    expect(await screen.findByTestId('index-panel')).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Index' })).toHaveAttribute('aria-selected', 'true');
  });

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
    await pickCoder(user);

    await user.type(screen.getByTestId('chat-message-input'), 'fix the build');
    await user.click(screen.getByRole('radio', { name: 'Plan mode (no changes)' }));
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
      skills: [],
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
    // The inspector's Approvals chip counts it.
    expect(screen.getByTestId('inspector-chip-attention-approvals')).toHaveTextContent('1');
    expect(screen.getByRole('tab', { name: 'Approvals, 1 waiting', hidden: true })).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Allow once' }));
    expect(ipc.calls.approvals).toEqual([{ requestId: 'r1', decision: 'allow_once' }]);
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.queryByTestId('inspector-chip-attention-approvals')).toBeNull();

    ipc.emit({ ...at(5), kind: 'tool_result', payload: { call_id: 'c1', ok: true, summary: 'Finished dev profile', truncated: false } });
    ipc.emit({ ...at(6), kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: ['m1'], changed_files: 1 } });
    ipc.emit({ ...at(7), kind: 'status', payload: { phase: 'done' } });

    expect(await screen.findByText('src/main.rs')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Undo turn' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'More' }));
    expect(await screen.findByRole('menuitem', { name: 'Open in chat' })).not.toHaveAttribute('aria-disabled', 'true');
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
    const user = userEvent.setup();
    mount(ipc, '/workspaces/w1?conversation=77');

    const transcript = await screen.findByTestId('session-transcript');
    expect(await within(transcript).findByText('Prompt of t1')).toBeInTheDocument();
    expect(within(transcript).getByText('Answer of t1')).toBeInTheDocument();
    expect(within(transcript).getByText('Prompt of t2')).toBeInTheDocument();
    expect(screen.getAllByTestId('history-turn')).toHaveLength(2);
    expect(screen.getAllByTestId('tool-row')).toHaveLength(1);
    expect(screen.getByText(/did not finish/)).toBeInTheDocument();
    expect(screen.queryByText(/Earlier messages of this thread/)).toBeNull();
    await user.click(screen.getByRole('button', { name: 'More' }));
    expect(await screen.findByRole('menuitem', { name: 'Open in chat' })).not.toHaveAttribute('aria-disabled', 'true');
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());

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

  describe('fork', () => {
    const FORK_UUID = '00000000-0000-4000-8000-000000000088';
    /** Every fork request carries a fresh idempotency key. */
    const ANY_FORK_ID: unknown = expect.any(String);
    const forkBodies: unknown[] = [];
    const forkOfForkBodies: unknown[] = [];
    function serveFork(status = 201): void {
      forkBodies.length = 0;
      forkOfForkBodies.length = 0;
      server.use(
        http.post(`${BASE}/elitea_core/conversation_fork/prompt_lib/42/88`, async ({ request }) => {
          forkOfForkBodies.push(await request.json().catch(() => null));
          return HttpResponse.json({ error: 'stop here' }, { status: 409 });
        }),
        http.post(`${BASE}/elitea_core/conversation_fork/prompt_lib/42/77`, async ({ request }) => {
          forkBodies.push(await request.json().catch(() => null));
          if (status !== 201) return HttpResponse.json({ error: 'fork_point_unsettled' }, { status });
          return HttpResponse.json(
            {
              id: 88,
              uuid: FORK_UUID,
              name: 'Fix the build (fork)',
              source: 'local_work',
              created_at: '2026-10-20T00:00:00',
              updated_at: '2026-10-20T00:00:00',
              duration: -1,
              message_groups_count: 2,
              is_pinned: false,
              is_archived: false,
              forked_from: { conversation_id: 77, message_id: 2, message_uuid: 'a1' },
              forked_messages: { q1: 'q1-copy', a1: 'a1-copy' },
            },
            { status: 201 },
          );
        }),
        http.get(`${BASE}/elitea_core/messages/prompt_lib/42/88`, () => HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100, total_pages: 1 })),
      );
    }
    const answered = (turnId: string, question: string, answer: string): StoredTurn => {
      const base = recorded(turnId);
      return {
        ...base,
        events: base.events.map((event) =>
          event.kind === 'done' ? { ...event, payload: { ...event.payload, message_ids: [question, answer] } } : event,
        ),
      };
    };

    it('forks from a recorded turn: the server at its answer, the host up to the turn, then opens the new thread in the folder', async () => {
      serveProject();
      serveFork();
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      ipc.setHistory('w1', '77', [answered('t1', 'q1', 'a1'), answered('t2', 'q2', 'a2')]);
      const appIpc = createFakeAppIpc();
      mount(ipc, '/workspaces/w1?conversation=77', undefined, appIpc);

      const prompts = await screen.findAllByTestId('your-prompt');
      appIpc.chooseFromContextMenu('fork');
      fireEvent.contextMenu(prompts[0] as HTMLElement);

      await waitFor(() => expect(forkBodies).toEqual([{ message_id: 'a1', fork_id: ANY_FORK_ID }]));
      const labels = (appIpc.calls.contextMenus[0] ?? []).map((item) => ('label' in item ? item.label : '-'));
      expect(labels).toEqual(['Copy', '-', 'Fork from Here', 'Rewind to Here']);
      await waitFor(() =>
        expect(ipc.calls.forks).toEqual([
          {
            workspaceId: 'w1',
            sourceConversationId: '77',
            targetConversationId: '88',
            targetConversationUuid: FORK_UUID,
            upToTurnId: 't1',
            messageIds: { q1: 'q1-copy', a1: 'a1-copy' },
          },
        ]),
      );
      // The new thread opens with the turns up to the fork point, and says the files were not rolled back.
      expect(await screen.findByTestId('fork-notice')).toHaveTextContent('Files stay as they are now.');
      expect(screen.getAllByTestId('history-turn')).toHaveLength(1);
      expect(screen.getByText('Prompt of t1')).toBeInTheDocument();
      expect(screen.queryByText('Prompt of t2')).toBeNull();
      expect(screen.getByTestId('thread-title')).toHaveTextContent('Fix the build (fork)');
      // The source thread is untouched.
      expect(await ipc.threadHistory('w1', '77')).toHaveLength(2);

      // A fork of the fork, at the copied turn, names the copy's answer: a message of the new conversation.
      appIpc.chooseFromContextMenu('fork');
      fireEvent.contextMenu((await screen.findAllByTestId('your-prompt'))[0] as HTMLElement);
      await waitFor(() => expect(forkOfForkBodies).toEqual([{ message_id: 'a1-copy', fork_id: ANY_FORK_ID }]));
    });

    it('rewinds to a recorded turn: a rewind on the server, the source listed under it, and the files restored only when confirmed', async () => {
      serveProject();
      serveFork();
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      ipc.setHistory('w1', '77', [answered('t1', 'q1', 'a1'), answered('t2', 'q2', 'a2')]);
      // The folder as t1 left it is t2's checkpoint.
      ipc.setRestorePoint('t1', 't2');
      ipc.setPreview('t2', { restored: ['src/app.ts'], deleted: ['notes.md'] });
      recordThread('w1', { id: '77', title: 'Fix the build', updatedAt: 1 });
      const appIpc = createFakeAppIpc();
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1?conversation=77', undefined, appIpc);

      const prompts = await screen.findAllByTestId('your-prompt');
      appIpc.chooseFromContextMenu('rewind');
      fireEvent.contextMenu(prompts[0] as HTMLElement);

      await waitFor(() => expect(forkBodies).toEqual([{ message_id: 'a1', mode: 'rewind', fork_id: ANY_FORK_ID }]));
      await waitFor(() => expect(ipc.calls.forks.map((fork) => fork.upToTurnId)).toEqual(['t1']));
      expect(await screen.findByTestId('fork-notice')).toHaveTextContent('the earlier attempt is listed under it');
      // The rewound thread takes the source's place; the source is its earlier attempt.
      expect(readThreads('w1').map((thread) => [thread.id, thread.earlierAttemptOf])).toEqual([
        ['88', undefined],
        ['77', '88'],
      ]);
      expect(await ipc.threadHistory('w1', '77')).toHaveLength(2);

      // Nothing is restored until the person confirms the dry run.
      await user.click(await screen.findByRole('button', { name: 'Restore files' }));
      const preview = await screen.findByTestId('fork-restore-preview');
      expect(preview).toHaveTextContent('src/app.ts is written back');
      expect(preview).toHaveTextContent('notes.md is deleted');
      expect(ipc.calls.restores).toEqual([]);
      await user.click(screen.getByRole('button', { name: 'Restore folder' }));
      await waitFor(() => expect(ipc.calls.restores).toEqual([{ turnId: 't2', confirmOlder: true }]));
      expect(await screen.findByTestId('fork-restored')).toBeInTheDocument();
    });

    it('offers no file restore when no later turn changed the folder, and says why a thread in the cloud is not forked', async () => {
      serveProject();
      serveFork();
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      ipc.setHistory('w1', '77', [answered('t1', 'q1', 'a1')]);
      const appIpc = createFakeAppIpc();
      mount(ipc, '/workspaces/w1?conversation=77', undefined, appIpc);

      appIpc.chooseFromContextMenu('fork');
      fireEvent.contextMenu(await screen.findByTestId('your-prompt'));
      expect(await screen.findByTestId('fork-notice')).toBeInTheDocument();
      await waitFor(() => expect(screen.queryByText('Prompt of t1')).toBeInTheDocument());
      expect(screen.queryByTestId('fork-restore')).toBeNull();

      server.use(
        http.post(`${BASE}/elitea_core/conversation_fork/prompt_lib/42/88`, () => HttpResponse.json({ error: 'thread_executor_is_cloud' }, { status: 409 })),
      );
      appIpc.chooseFromContextMenu('rewind');
      fireEvent.contextMenu((await screen.findAllByTestId('your-prompt'))[0] as HTMLElement);
      expect(await screen.findByTestId('fork-error')).toHaveTextContent('This thread is running in the cloud.');
    });

    it('forks the whole thread from the ⋯ menu, and still opens it when the local copy fails', async () => {
      serveProject();
      serveFork();
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      ipc.setHistory('w1', '77', [answered('t1', 'q1', 'a1')]);
      ipc.failNext('forkThreadHistory', 'storage', 'disk full');
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1?conversation=77');

      await screen.findByText('Prompt of t1');
      await user.click(screen.getByRole('button', { name: 'More' }));
      await user.click(await screen.findByRole('menuitem', { name: 'Fork Thread' }));

      await waitFor(() => expect(forkBodies).toEqual([{}]));
      expect(ipc.calls.forks[0]).toEqual({
        workspaceId: 'w1',
        sourceConversationId: '77',
        targetConversationId: '88',
        targetConversationUuid: FORK_UUID,
        messageIds: { q1: 'q1-copy', a1: 'a1-copy' },
      });
      expect(await screen.findByTestId('fork-notice')).toHaveTextContent('the tool history on this computer was not copied');
      expect(screen.getByTestId('thread-title')).toHaveTextContent('Fix the build (fork)');
    });

    it('says why a fork point that is not settled cannot be forked, and stays in the thread', async () => {
      serveProject();
      serveFork(409);
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      ipc.setHistory('w1', '77', [answered('t1', 'q1', 'a1')]);
      const appIpc = createFakeAppIpc();
      mount(ipc, '/workspaces/w1?conversation=77', undefined, appIpc);

      const prompt = await screen.findByTestId('your-prompt');
      appIpc.chooseFromContextMenu('fork');
      fireEvent.contextMenu(prompt);

      expect(await screen.findByTestId('fork-error')).toHaveTextContent('a turn is still running or waits for an approval');
      expect(ipc.calls.forks).toEqual([]);
      expect(screen.getByText('Prompt of t1')).toBeInTheDocument();
    });

    it('offers no fork point on a turn whose answer is not known', async () => {
      serveProject();
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      ipc.setHistory('w1', '77', [recorded('t1', { state: 'interrupted', events: recorded('t1').events.slice(0, 2) })]);
      const appIpc = createFakeAppIpc();
      mount(ipc, '/workspaces/w1?conversation=77', undefined, appIpc);

      fireEvent.contextMenu(await screen.findByTestId('your-prompt'));
      await waitFor(() => expect(appIpc.calls.contextMenus).toHaveLength(1));
      const fork = (appIpc.calls.contextMenus[0] ?? []).find((item) => 'label' in item && item.label === 'Fork from Here');
      expect(fork !== undefined && 'enabled' in fork ? fork.enabled : undefined).toBe(false);
    });
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

    await pickCoder(user);
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

    await pickCoder(user);
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

    await pickCoder(user);
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

    await pickCoder(user);
    await user.type(screen.getByTestId('chat-message-input'), 'go');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
    ipc.emit({ turn_id: 'turn-1', seq: 1, kind: 'text_delta', payload: { text: 'working in app' } });
    expect(await screen.findByText('working in app')).toBeInTheDocument();
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));

    await act(() => openFolder('w2'));
    expect(await screen.findByRole('heading', { name: 'lib' })).toBeInTheDocument();
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

    await pickCoder(user);
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
        startErrorCode: null,
        start: () => Promise.resolve(started),
        prewarm: () => undefined,
        retryCommit: () => Promise.resolve(),
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
        // What the "Local coding agent" offer checks: may this person create agents here, and with which model.
        http.get(`${BASE}/auth/permissions/prompt_lib/42`, () =>
          HttpResponse.json(mayCreate ? [{ name: 'models.applications.applications.create', enabled: true }] : []),
        ),
        http.get(`${BASE}/configurations/models/42`, () =>
          HttpResponse.json({ items: [{ name: 'gpt-5', project_id: '1', default: true }], default_model_name: 'gpt-5' }),
        ),
      );
    }
    let mayCreate = true;

    afterEach(() => {
      window.localStorage.clear();
      mayCreate = true;
    });

    it('offers a Local coding agent in an empty project, creates it there on a click, and a turn runs on it', async () => {
      const rows: { id: string; name: string }[] = [];
      serveAgents(rows);
      const created: unknown[] = [];
      server.use(
        http.post(`${BASE}/elitea_core/applications/prompt_lib/42`, async ({ request }) => {
          created.push(await request.json());
          rows.push({ id: '5', name: 'Local coding agent' });
          return HttpResponse.json({ id: 5, name: 'Local coding agent' }, { status: 201 });
        }),
        http.get(`${BASE}/elitea_core/application_skills/prompt_lib/42/51`, () =>
          HttpResponse.json({ items: [], total: 0, page: 1, page_size: 0, total_pages: 1 }),
        ),
        http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })),
        http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ id: 77, name: 'fix the build' })),
        http.post(`${BASE}/elitea_core/participants/prompt_lib/42/77`, () => HttpResponse.json([])),
      );
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1');

      const empty = await screen.findByTestId('workspace-no-agents');
      expect(await within(empty).findByText(/^Create a Local coding agent\?/)).toBeInTheDocument();
      // Nothing is created until the person clicks.
      expect(created).toEqual([]);
      await user.click(within(empty).getByRole('button', { name: 'Create a Local coding agent' }));

      await waitFor(() => expect(created).toHaveLength(1));
      const body = created[0] as { name: string; versions: { instructions: string; llm_settings: unknown; meta: unknown; tools?: unknown }[] };
      expect(body.name).toBe('Local coding agent');
      expect(body.versions).toHaveLength(1);
      const [version] = body.versions;
      expect(version?.instructions).toContain('Explore before you edit');
      expect(version?.instructions).toContain('never invent file contents');
      expect(version?.llm_settings).toEqual({ model_name: 'gpt-5', model_project_id: 1 });
      expect(version?.meta).toEqual({ step_limit: 100 });
      expect(version?.tools).toBeUndefined();

      // The list reads again: the new agent is picked and a turn runs on it.
      await waitFor(() => expect(screen.queryByTestId('workspace-no-agents')).toBeNull());
      await waitFor(() => expect(screen.getByTestId('agent-chip')).toHaveTextContent('Local coding agent · base'));
      await user.type(screen.getByTestId('chat-message-input'), 'fix the build');
      await waitFor(() => expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled());
      await user.click(screen.getByRole('button', { name: 'Send' }));
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]).toEqual(expect.objectContaining({ application_id: 5, version_id: 51 }));
    });

    it('explains who can create an agent when the person may not', async () => {
      mayCreate = false;
      serveAgents([]);
      mount(createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] }), '/workspaces/w1');
      const empty = await screen.findByTestId('workspace-no-agents');
      expect(await within(empty).findByText(/You cannot create agents in this project/)).toBeInTheDocument();
      expect(within(empty).queryByRole('button', { name: 'Create a Local coding agent' })).toBeNull();
      expect(within(empty).queryByRole('button', { name: 'Create an agent' })).toBeNull();
      expect(within(empty).getByRole('button', { name: 'Use another project' })).toBeInTheDocument();
    });

    it('explains an empty project and offers to create an agent there or bind another project', async () => {
      serveAgents([]);
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1');

      const empty = await screen.findByTestId('workspace-no-agents');
      expect(within(empty).getByText('This project has no agents')).toBeInTheDocument();
      expect(screen.queryByTestId('agent-chip')).toBeNull();

      await user.click(within(empty).getByRole('button', { name: 'Use another project' }));
      await user.click(await screen.findByRole('menuitemradio', { name: 'Public' }));
      await waitFor(async () => expect((await ipc.list())[0]?.project_id).toBe(1));
    });

    it('creates the agent in the bound project, without creating anything itself', async () => {
      serveAgents([]);
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1');

      const empty = await screen.findByTestId('workspace-no-agents');
      await screen.findByTestId('thread-title');
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

    async function ready(ipc: FakeWorkspaceIpc, skills?: Parameters<typeof serveProject>[0]): Promise<ReturnType<typeof userEvent.setup>> {
      serveProject(skills);
      const user = userEvent.setup();
      mount(ipc, '/workspaces/w1');
      await waitFor(() => expect(screen.getByTestId('agent-chip')).toHaveTextContent('· base'));
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

    it('asks the host to read ahead for the selected agent when the composer gets focus', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc);
      expect(ipc.calls.prewarmed).toEqual([]);
      await user.click(screen.getByTestId('chat-message-input'));
      await waitFor(() => expect(ipc.calls.prewarmed.length).toBeGreaterThan(0));
      // A new thread has no conversation yet: only the agent version is read ahead.
      expect(ipc.calls.prewarmed[0]).toEqual({ project_id: 42, application_id: 5, version_id: 9 });
      expect(ipc.calls.started).toHaveLength(0);
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
    it('runs the "/" commands: plan, help, agent, new, clear, compact and undo', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      ipc.setChanges('turn-1', { files: [{ path: 'a.txt', status: 'modified', added: 1, removed: 1, diff: '' }] });
      const user = await ready(ipc);
      const input = screen.getByTestId('chat-message-input');

      await user.type(input, '/');
      const menu = await screen.findByTestId('workspace-command-menu');
      expect(within(menu).getAllByRole('option').map((o) => o.getAttribute('aria-label'))).toEqual(['/new', '/plan', '/undo', '/agent', '/model', '/clear', '/compact', '/help']);
      await user.type(input, 'pl{Enter}');
      expect(screen.getByRole('radio', { name: 'Plan mode (no changes)' })).toBeChecked();
      await waitFor(() => expect(input).toHaveValue(''));

      await user.type(input, '/help{Enter}');
      expect(within(await screen.findByTestId('workspace-help')).getByText('/undo')).toBeInTheDocument();

      await user.type(input, '/agent{Enter}');
      expect(await screen.findByRole('menu')).toBeInTheDocument();
      await user.keyboard('{Escape}');

      // Nothing to undo yet.
      await user.click(input);
      await user.type(input, '/undo{Enter}');
      expect(await screen.findByText('The last turn here changed no files.')).toBeInTheDocument();

      // /compact: the next send asks the host to compact first.
      await user.type(input, '/compact{Enter}');
      expect(await screen.findByTestId('workspace-compact-next')).toBeInTheDocument();

      // A turn that changed a file: /undo asks the same confirmation as the card's button.
      await user.type(input, 'go{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.plan_mode).toBe(true);
      expect(ipc.calls.started[0]?.compact).toBe(true);
      await waitFor(() => expect(screen.queryByTestId('workspace-compact-next')).toBeNull());
      ipc.emit({ turn_id: 'turn-1', seq: 1, kind: 'status', payload: { phase: 'running' } });
      ipc.emit({ turn_id: 'turn-1', seq: 2, kind: 'text_delta', payload: { text: 'changed a.txt' } });
      ipc.emit({
        turn_id: 'turn-1',
        seq: 3,
        kind: 'usage',
        payload: {
          prompt_tokens: 124_000,
          completion_tokens: 10,
          total_tokens: 124_010,
          context: { used_tokens: 124_000, window_tokens: 200_000, phase: 'compacted', compacted: true, compacted_at: '2026-10-21T10:15:00.000Z', window_fallback: false },
        },
      });
      ipc.emit({ turn_id: 'turn-1', seq: 4, kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: [], changed_files: 1 } });
      expect(await screen.findByText('a.txt')).toBeInTheDocument();
      // The composer's context meter shows the call's occupancy.
      expect(await screen.findByTestId('context-meter')).toHaveTextContent('62% of 200K');
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
      await waitFor(() => expect(screen.getByTestId('agent-chip')).toHaveTextContent('· base'));
      await user.type(screen.getByTestId('chat-message-input'), 'again{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(2));
      expect(created).toBe(1);
      expect(ipc.calls.started[1]?.conversation_id).toBe('78');
      expect(ipc.calls.started[1]?.compact).toBeUndefined();
    }, 30_000);

    it('chooses the turn\'s model and effort from the chip, or with /model, and sends it with the turn', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc);
      const input = screen.getByTestId('chat-message-input');
      const chip = await screen.findByTestId('model-chip');
      await waitFor(() => expect(chip).toHaveTextContent('gpt-4o'));

      await user.click(chip);
      await user.click(await screen.findByRole('menuitem', { name: /gpt-5/ }));
      await waitFor(() => expect(chip).toHaveTextContent('gpt-5'));
      // gpt-5 supports reasoning: its effort is offered too.
      await user.click(chip);
      await user.click(await screen.findByTestId('model-effort-high'));
      await waitFor(() => expect(chip).toHaveTextContent('gpt-5 · High'));

      await user.type(input, 'go{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.model).toEqual({ name: 'gpt-5', reasoning_effort: 'high' });
      ipc.emit({ turn_id: 'turn-1', seq: 1, kind: 'status', payload: { phase: 'running', model: { name: 'gpt-5', reasoning_effort: 'high', chosen: true, pinned: false } } });
      ipc.emit({ turn_id: 'turn-1', seq: 2, kind: 'done', payload: { committed: false, conversation_id: '77', message_ids: [], changed_files: 0 } });
      await waitFor(() => expect(screen.getByTestId('workspace-send-button')).toBeInTheDocument());

      // `/model <name>` as the whole message sets it; nothing goes to the agent.
      await user.type(input, '/model gpt-4o{Enter}');
      await waitFor(() => expect(chip).toHaveTextContent('gpt-4o'));
      expect(chip).not.toHaveTextContent('High');
      await waitFor(() => expect(input).toHaveValue(''));
      expect(ipc.calls.started).toHaveLength(1);

      // `/model` alone opens the chip's menu.
      await user.type(input, '/mod{Enter}');
      expect(await screen.findByRole('menuitem', { name: /gpt-5/ })).toBeInTheDocument();
    });

    it('offers no model choice when the agent pins its model, and /model says why', async () => {
      pinModel = true;
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc);
      const input = screen.getByTestId('chat-message-input');
      await waitFor(() => expect(screen.queryByTestId('model-chip')).toBeNull());
      await user.type(input, '/model{Enter}');
      expect(await screen.findByTestId('workspace-model-fixed')).toBeInTheDocument();
      await user.type(input, '/model gpt-5{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      // Without a chip the text is a message like any other, sent without a model.
      expect(ipc.calls.started[0]?.model).toBeUndefined();
    });

    it('lists the agent\'s skills after the commands, filtered, and sends the one the message starts with', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc, [
        { id: '7', name: 'style', description: 'The house style' },
        { id: '8', name: 'code-review', description: 'Review a change' },
      ]);
      const input = screen.getByTestId('chat-message-input');

      await user.type(input, '/');
      const menu = await screen.findByTestId('workspace-command-menu');
      await waitFor(() => expect(within(menu).getAllByRole('option')).toHaveLength(10));
      expect(within(menu).getAllByRole('option').map((o) => o.getAttribute('aria-label'))).toEqual([
        '/new', '/plan', '/undo', '/agent', '/model', '/clear', '/compact', '/help', '/code-review', '/style',
      ]);
      expect(within(menu).getByText('Skills')).toBeInTheDocument();
      expect(within(menu).getByText('The house style')).toBeInTheDocument();

      // Filtered by what follows the "/": only the skills match "st".
      await user.type(input, 'st');
      await waitFor(() =>
        expect(
          within(screen.getByTestId('workspace-command-menu'))
            .getAllByRole('option')
            .map((o) => o.getAttribute('aria-label')),
        ).toEqual(['/style']),
      );
      await user.keyboard('{Enter}');
      await waitFor(() => expect(input).toHaveValue('/style '));
      expect(screen.queryByTestId('workspace-command-menu')).toBeNull();

      await user.type(input, 'tidy the README{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.prompt).toBe('/style tidy the README');
      expect(ipc.calls.started[0]?.skills).toEqual(['style']);
    });

    it('lists the custom commands by source after the host commands and sends the picked one as a command', async () => {
      const ipc = createFakeWorkspaceIpc({
        workspaces: [{ ...FOLDER, project_id: 42 }],
        commands: [
          { name: 'standup', description: 'My standup', argument_hint: null, source: 'user', path: 'standup.md', ignored: [] },
          { name: 'review', description: 'Review the diff', argument_hint: '[files]', source: 'workspace', path: '.claude/commands/review.md', ignored: ['allowed-tools'] },
        ],
      });
      const user = await ready(ipc, [
        { id: '7', name: 'style', description: 'The house style' },
        { id: '8', name: 'review', description: 'Hidden by the command' },
      ]);
      const input = screen.getByTestId('chat-message-input');

      await user.type(input, '/');
      const menu = await screen.findByTestId('workspace-command-menu');
      await waitFor(() => expect(within(menu).getAllByRole('option')).toHaveLength(11));
      expect(within(menu).getAllByRole('option').map((o) => o.getAttribute('aria-label'))).toEqual([
        '/new', '/plan', '/undo', '/agent', '/model', '/clear', '/compact', '/help', '/review', '/standup', '/style',
      ]);
      expect(within(menu).getByText('Commands in this workspace')).toBeInTheDocument();
      expect(within(menu).getByText('Your commands')).toBeInTheDocument();
      expect(within(menu).getByText('Review the diff · [files] · ignored: allowed-tools')).toBeInTheDocument();

      await user.type(input, 'rev');
      await user.keyboard('{Enter}');
      await waitFor(() => expect(input).toHaveValue('/review '));
      await user.type(input, 'src/a.rs{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.prompt).toBe('/review src/a.rs');
      expect(ipc.calls.started[0]?.command).toEqual({ name: 'review', arguments: 'src/a.rs' });
      expect(ipc.calls.started[0]?.skills).toEqual([]);
    });

    it('lists the custom commands and skills in /help', async () => {
      const ipc = createFakeWorkspaceIpc({
        workspaces: [{ ...FOLDER, project_id: 42 }],
        commands: [{ name: 'standup', description: 'My standup', argument_hint: null, source: 'user', path: 'standup.md', ignored: [] }],
      });
      const user = await ready(ipc, [{ id: '7', name: 'style', description: 'The house style' }]);
      const input = screen.getByTestId('chat-message-input');
      await user.type(input, '/');
      await screen.findByTestId('workspace-command-menu');
      await waitFor(() => expect(within(screen.getByTestId('workspace-command-menu')).getAllByRole('option')).toHaveLength(10));
      await user.type(input, 'help');
      await user.keyboard('{Enter}');
      const help = await screen.findByTestId('workspace-help');
      expect(help).toHaveTextContent('/standup — My standup (yours)');
      expect(help).toHaveTextContent('/style — The house style');
      expect(help).toHaveTextContent('/new — Start a new conversation');
    });

    it('offers the project\'s other skills in their own section and sends a picked one by id', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc, [{ id: '7', name: 'style', description: 'The house style' }]);
      const listed: URL[] = [];
      server.use(
        http.get(`${BASE}/elitea_core/skills/prompt_lib/42`, ({ request }) => {
          listed.push(new URL(request.url));
          return HttpResponse.json({
            items: [
              { id: '21', project_id: '42', name: 'deploy', description: 'Ship it', type: 'skill', is_default: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
              // The version carries it already: offered once, as the version's.
              { id: '22', project_id: '42', name: 'Style', type: 'skill', is_default: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
            ],
            total: 2,
            page: 1,
            page_size: 100,
            total_pages: 1,
          });
        }),
      );
      const input = screen.getByTestId('chat-message-input');
      // Read only once the composer is used.
      expect(listed).toHaveLength(0);

      await user.type(input, '/');
      const menu = await screen.findByTestId('workspace-command-menu');
      await waitFor(() => expect(within(menu).getAllByRole('option')).toHaveLength(10));
      expect(within(menu).getAllByRole('option').map((o) => o.getAttribute('aria-label')).slice(8)).toEqual(['/style', '/deploy']);
      expect(within(menu).getByText('Project skills')).toBeInTheDocument();
      expect(listed[0]?.searchParams.get('page_size')).toBe('100');

      await user.type(input, 'dep');
      await user.keyboard('{Enter}');
      await waitFor(() => expect(input).toHaveValue('/deploy '));
      await user.type(input, 'the release{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.prompt).toBe('/deploy the release');
      expect(ipc.calls.started[0]?.skills).toEqual([{ source: 'project', id: '21' }]);
    });

    it('reads every page of the project\'s skills and labels them in /help', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc, [{ id: '7', name: 'style', description: 'The house style' }]);
      const pages: string[] = [];
      const row = (id: number, name: string) => ({ id: String(id), project_id: '42', name, type: 'skill', is_default: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' });
      server.use(
        http.get(`${BASE}/elitea_core/skills/prompt_lib/42`, ({ request }) => {
          const page = new URL(request.url).searchParams.get('page') ?? '1';
          pages.push(page);
          // A full first page, then a short second one holding the skill asked for.
          // bulk-000 shares its id with the agent's "style": the /help rows are keyed by source too.
          const items = page === '1' ? Array.from({ length: 100 }, (_, i) => row(i === 0 ? 7 : 100 + i, `bulk-${String(i).padStart(3, '0')}`)) : [row(300, 'last-page')];
          return HttpResponse.json({ items, total: 101, page: Number(page), page_size: 100, total_pages: 2 });
        }),
      );
      const input = screen.getByTestId('chat-message-input');
      await user.type(input, '/last');
      const menu = await screen.findByTestId('workspace-command-menu');
      await waitFor(() => expect(within(menu).getAllByRole('option').map((o) => o.getAttribute('aria-label'))).toEqual(['/last-page']));
      expect(pages).toEqual(['1', '2']);
      // /help labels a project skill as the project's, beside the agent's of the same id.
      await user.clear(input);
      await user.type(input, '/help{Enter}');
      const help = await screen.findByTestId('workspace-help');
      expect(help).toHaveTextContent('/bulk-000 — A skill of this project');
      expect(help).not.toHaveTextContent('A skill of this agent');
      expect(help).toHaveTextContent('/style — The house style');
    });

    it('sends no skill when the message does not start with one', async () => {
      const ipc = createFakeWorkspaceIpc({ workspaces: [{ ...FOLDER, project_id: 42 }] });
      const user = await ready(ipc, [{ id: '7', name: 'style' }]);
      await user.type(screen.getByTestId('chat-message-input'), 'use /style here{Enter}');
      await waitFor(() => expect(ipc.calls.started).toHaveLength(1));
      expect(ipc.calls.started[0]?.skills).toEqual([]);
    });

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
