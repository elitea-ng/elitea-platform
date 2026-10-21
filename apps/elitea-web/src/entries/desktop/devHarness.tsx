/**
 * DEV ONLY — the desktop shell without a host or a deployment, for looking at
 * it in a browser (`npx vite --mode desktop`, then `/?harness`; add
 * `&chrome=mac` for the macOS title-bar overlay, `&mode=light|dark`, and
 * `&vibrancy` for the transparent sidebar over a stand-in wallpaper — the
 * worst case for the tint, as no real material softens it).
 *
 * The workspace, index and native-shell IPC are the in-memory fakes the tests use;
 * the few API calls the workspace screens make are answered from canned data.
 * `window.__harness.playTurn()` scripts an agent turn against the fake host;
 * `window.__harness.playActivity()` puts the Local work threads (`&threads`)
 * in the four activity states (running, needs you, unread, read).
 *
 * `main.tsx` imports this behind `import.meta.env.DEV`, so no production build
 * contains it.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import Typography from '@mui/material/Typography';

import { AppProviders } from '@/app/providers/AppProviders';
import { recordThread, WorkspaceIpcProvider } from '@/features/workspace';
import { HandoffIpcProvider } from '@/features/workspace-handoff';
import { IndexIpcProvider } from '@/features/workspace-index';
import WorkspaceSessionPage from '@/pages/workspace/WorkspaceSessionPage';
import WorkspacesPage from '@/pages/workspace/WorkspacesPage';
import ArchivedChatsPage from '@/pages/chat-archived/ArchivedChatsPage';
import { ChatParticipantsPanel } from '@/pages/chat/ChatParticipantsPanel';
import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { BROWSER_PLATFORM } from '@/shared/desktop/appEvents';
import { installTestEventSource } from '@/shared/api/sse/testing';
import { createFakeAppIpc, MACOS_PLATFORM } from '@/shared/desktop/appEvents.fake';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { createFakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';
import { createFakeIndexIpc } from '@/shared/desktop/indexIpc.fake';
import { PERMISSION_GROUPS, PERMISSIONS } from '@/shared/lib/permissions';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { AppIpcProvider, DesktopFrame, useActivityFeed, useDesktopLayout } from '@/widgets/desktop-shell';

const FOLDERS: Workspace[] = [
  { id: 'w1', path: '/Users/me/code/elitea-platform', name: 'elitea-platform', project_id: 42, is_git: true },
  { id: 'w2', path: '/Users/me/notes/docs', name: 'docs', project_id: null, is_git: false },
];

const hoursAgo = (hours: number): string => new Date(Date.now() - hours * 3_600_000).toISOString();
type ChatActivity = { readonly state: 'running' | 'needs_you' | 'idle'; readonly unread?: boolean };
const chat = (id: string, name: string, hours: number, activity?: ChatActivity) => ({
  id,
  name,
  is_private: true,
  author_id: 3,
  updated_at: hoursAgo(hours),
  created_at: hoursAgo(hours),
  // Client contract 1.12: the row's activity status.
  ...(activity === undefined ? {} : { activity: { state: activity.state, since: hoursAgo(hours) }, last_activity_at: hoursAgo(hours), unread: activity.unread === true }),
});

/** The Chats list (`&chats`): the grouped folders listing the sidebar reads. */
const GROUPED_CHATS = {
  pinned: { conversations: [chat('81', 'Release checklist', 30)] },
  folders: [{ id: '5', name: 'Research', meta: { is_pinned: false }, total: 2, offset: 2, conversations: [chat('91', 'Vector stores compared', 50), chat('92', 'Embedding model picks', 70)] }],
  date_groups: [
    {
      name: 'Today',
      total: 12,
      offset: 4,
      needs_you: 1,
      conversations: [
        chat('72', 'quick overview', 0, { state: 'running' }),
        chat('73', 'Fix flaky gateway test', 1, { state: 'needs_you' }),
        chat('74', 'Draft the Q4 roadmap email', 2, { state: 'idle', unread: true }),
        chat('79', 'Rename the config keys', 3, { state: 'idle' }),
      ],
    },
    { name: 'Yesterday', total: 2, offset: 2, conversations: [chat('75', 'Postgres connection math', 26, { state: 'idle', unread: true }), chat('76', 'Explain ADR-0034', 28)] },
    { name: 'This week', total: 1, offset: 1, conversations: [chat('77', 'Helm values for staging', 80)] },
    { name: 'This month', total: 0, offset: 0, conversations: [] },
    { name: 'Older', total: 1, offset: 1, conversations: [chat('78', 'Onboarding notes', 900)] },
  ],
  total_folders: 1,
};

const ARCHIVED_CHATS = {
  total: 2,
  rows: [
    { id: 61, name: 'Old sprint retro', created_at: hoursAgo(400), updated_at: hoursAgo(300), duration: -1, message_groups_count: 4, is_pinned: false, is_archived: true, archived_by: { id: 4, name: 'Ana Ruiz' }, archived_at: hoursAgo(5) },
    { id: 62, name: 'Spike: socket.io removal', created_at: hoursAgo(900), updated_at: hoursAgo(800), duration: -1, message_groups_count: 9, is_pinned: false, is_archived: true, archived_by: { id: 3, name: 'Me' }, archived_at: hoursAgo(50) },
  ],
};

/** The participants the harness chat's inspector shows. */
const HARNESS_CONVERSATION = {
  id: '72',
  name: 'quick overview',
  participants: [
    { id: 3, entity_name: 'application', entity_meta: { id: 5, project_id: 42 }, entity_settings: { agent_type: 'openai' }, meta: { name: 'Coder' } },
    { id: 4, entity_name: 'application', entity_meta: { id: 6, project_id: 42 }, entity_settings: { agent_type: 'openai' }, meta: { name: 'Reviewer' } },
    { id: 7, entity_name: 'user', entity_meta: { id: 3 }, meta: { name: 'Me' } },
  ],
};

function HarnessChat({ title }: { title: string }): React.JSX.Element {
  return (
    <>
      <Placeholder title={title} />
      <ChatParticipantsPanel
        activeConversation={HARNESS_CONVERSATION}
        activeParticipant={undefined}
        projectId="42"
        isMcpVisible={false}
        handlers={{ onSelectParticipant: () => undefined, onDeleteParticipant: () => undefined, onEditParticipant: () => undefined, onAddParticipants: () => undefined }}
      />
    </>
  );
}

const API: [string, RegExp, unknown][] = [
  ['GET', /\/elitea_core\/folder\/prompt_lib\/42$/, GROUPED_CHATS],
  ['PUT', /\/elitea_core\/conversation_archive\/prompt_lib\/42\/\d+$/, { ok: true }],
  ['DELETE', /\/elitea_core\/conversation_archive\/prompt_lib\/42\/\d+$/, { ok: true }],
  ['PUT', /\/elitea_core\/conversation\/prompt_lib\/42\/\d+$/, { id: 72 }],
  ['DELETE', /\/elitea_core\/conversation\/prompt_lib\/42\/\d+$/, {}],
  ['POST', /\/social\/pin\//, { ok: true }],
  ['DELETE', /\/social\/pin\//, { ok: true }],
  ['GET', /\/projects\/project\/default\//, [{ id: '1', name: 'Public', suspended: false }, { id: '42', name: 'Platform team', suspended: false }]],
  ['GET', /\/elitea_core\/applications\/prompt_lib\/42/, { rows: [{ id: '5', name: 'Coder', tags: [] }, { id: '6', name: 'Reviewer', tags: [] }], total: 2 }],
  [
    'GET',
    /\/elitea_core\/application\/prompt_lib\/42\/\d+/,
    { id: '5', name: 'Coder', description: '', versions: [{ id: '9', name: 'base', status: 'published', agent_type: 'openai' }] },
  ],
  ['POST', /\/elitea_core\/conversations\/prompt_lib\/42$/, { id: 77, name: 'quick overview' }],
  ['GET', /\/elitea_core\/conversation\/prompt_lib\/42\/\d+$/, { id: 72, uuid: 'c0ffee00-0000-4000-8000-000000000072', name: 'quick overview', participants: [{ id: 3, entity_name: 'application', name: 'Coder', meta: {} }] }],
  ['POST', /\/elitea_core\/continue_predict\/prompt_lib\/42\/c0ffee00-0000-4000-8000-000000000072$/, { events_url: '/api/v2/executions/42/exec-1/events', response_message_id: 'e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44' }],
  // The cloud copy's messages (`&cloud`), and the start route a message to it goes through.
  ['GET', /\/elitea_core\/messages\/prompt_lib\/42\/72$/, { items: [{ id: 'm1', uid: 'm1', role: 'user', content: 'Give me a quick overview of the repo', conversation_id: '72', created_at: '2026-10-20T08:00:00Z' }, { id: 'm2', uid: 'm2', role: 'assistant', content: 'The repository is a **Go monorepo** with the desktop app in `apps/elitea-desktop`.', conversation_id: '72', created_at: '2026-10-20T08:00:05Z' }], total: 2, page: 1, page_size: 100, total_pages: 1 }],
  ['POST', /\/elitea_core\/messages\/prompt_lib\/42\/c0ffee00-0000-4000-8000-000000000072$/, { task_id: 'exec-1', events_url: '/api/v2/executions/42/exec-1/events', response_message_id: 'e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44' }],
  ['GET', /\/elitea_core\/conversations\/prompt_lib\/42/, { rows: [], total: 0 }],
  ['POST', /\/elitea_core\/participants\//, []],
  ['GET', /\/social\/author/, { id: 3, name: 'Me' }],
  ['GET', /\/auth\/permissions\/prompt_lib\/42$/, ['get', 'create', 'update', 'delete'].map((verb) => ({ name: `models.chat.folders.${verb}`, enabled: true }))],
];

function installApiStub(): void {
  const answer = (method: string, url: string): Response => {
    const parsed = new URL(url, window.location.origin);
    const path = parsed.pathname;
    if (method === 'GET' && parsed.searchParams.get('archived') === 'only') {
      return new Response(JSON.stringify(ARCHIVED_CHATS), { status: 200, headers: { 'content-type': 'application/json' } });
    }
    const hit = API.find(([verb, pattern]) => verb === method && pattern.test(path));
    return new Response(JSON.stringify(hit === undefined ? {} : hit[2]), {
      status: hit === undefined ? 404 : 200,
      headers: { 'content-type': 'application/json' },
    });
  };
  // oxlint-disable-next-line no-restricted-properties -- dev-only harness: stands in for the deployment the generated client would call.
  window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : undefined;
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    return Promise.resolve(answer((init?.method ?? request?.method ?? 'GET').toUpperCase(), url));
  };
}

function Placeholder({ title }: { title: string }): React.JSX.Element {
  return (
    <Typography variant="headingMedium" component="h1" sx={{ padding: 3 }}>
      {title}
    </Typography>
  );
}

export function mountDesktopHarness(container: HTMLElement): void {
  const params = new URLSearchParams(window.location.search);
  const mode = params.get('mode');
  // oxlint-disable-next-line elitea/no-raw-webstorage -- dev-only: the theme's own mode key (`el-mode`), set before it mounts.
  if (mode === 'light' || mode === 'dark') window.localStorage.setItem('el-mode', mode);
  (globalThis as { elitea_ui_config?: unknown }).elitea_ui_config = {
    vite_server_url: window.location.origin,
    vite_base_uri: '/',
    vite_public_project_id: '1',
  };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useSelectedProjectStore.getState().setProject({ id: '42', name: 'Platform team' });
  installApiStub();

  if (params.has('threads')) {
    recordThread('w1', { id: '71', title: 'fix CI on the gateway', updatedAt: 1 });
    recordThread('w1', { id: '72', title: 'quick overview', updatedAt: 2 });
    recordThread('w1', { id: '73', title: 'rename the config keys', updatedAt: 3 });
    recordThread('w1', { id: '74', title: 'bump the chart version', updatedAt: 4 });
  }
  const ipc = createFakeWorkspaceIpc({
    workspaces: params.has('empty') ? [] : FOLDERS,
    nextOpen: { id: 'w3', path: '/Users/me/code/new-thing', name: 'new-thing', project_id: 42, is_git: true },
    files: [
      { path: 'README.md', kind: 'file' },
      { path: 'src', kind: 'dir' },
      { path: 'src/main.rs', kind: 'file' },
    ],
  });
  // `&indexOff`: the policy turns the local index off.
  const indexIpc = createFakeIndexIpc({
    policyAllowed: !params.has('indexOff'),
    statuses: { w1: { state: 'ready', files: 214, entities: 12_400, relations: 30_100, last_run: '2026-10-10T08:00:00Z' } },
  });
  // `&cloud`: threads start in the cloud; `&handoffOff`: the policy turns Send to cloud off.
  const handoffIpc = createFakeHandoffIpc({
    allowed: !params.has('handoffOff'),
    ...(params.has('cloud') ? { status: { state: 'CLOUD_IDLE', executor: 'cloud' as const, read_only: true, sent_from_here: true, epoch: 2 } } : {}),
  });
  // The cloud copy's reply streams over SSE: a scripted one, played by `__harness.playCloudReply()`.
  const streams = installTestEventSource();
  const vibrancy = params.has('vibrancy');
  if (vibrancy) {
    // A busy stand-in wallpaper behind the transparent page (NativeLook clears html/body).
    const wallpaper = document.createElement('style');
    // oxlint-disable-next-line elitea/no-raw-color -- dev-only: a wallpaper is not a brand colour.
    wallpaper.textContent = 'html[data-harness-wallpaper] { background: linear-gradient(135deg, #ff5f6d 0%, #ffc371 35%, #2b5876 70%, #ffffff 100%) fixed; }';
    document.head.append(wallpaper);
    document.documentElement.dataset['harnessWallpaper'] = '';
  }
  const appIpc = createFakeAppIpc(
    params.get('chrome') === 'mac' || vibrancy ? { ...MACOS_PLATFORM, vibrancy } : BROWSER_PLATFORM,
  );
  // The folder rights too: New folder, Move to folder… and the drag in the Chats list.
  const permissions = new Set([...Object.values(PERMISSION_GROUPS).flat(), PERMISSIONS.chat.folders.create, PERMISSIONS.chat.folders.update, PERMISSIONS.chat.archive]);

  const rootRoute = createRootRoute({
    component: () => (
      <DesktopFrame
        permissions={permissions}
        projects={[{ id: 1, name: 'Public', suspended: false }, { id: 42, name: 'Platform team', suspended: false }]}
        selectedProjectId="42"
        onSelectProject={() => undefined}
      >
        <Outlet />
      </DesktopFrame>
    ),
  });
  const page = (path: string, title: string) => createRoute({ getParentRoute: () => rootRoute, path, component: () => <Placeholder title={title} /> });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: WorkspacesPage }),
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: WorkspaceSessionPage }),
      page('/agents', 'Agents'),
      page('/settings', 'Settings'),
      page('/elitea-catalog', 'Catalog'),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: () => <HarnessChat title="New chat" /> }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/archived', component: ArchivedChatsPage }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => <HarnessChat title="quick overview" /> }),
    ]),
    history: createMemoryHistory({ initialEntries: [params.get('at') ?? '/workspaces'] }),
  });

  if (params.has('threads')) {
    // Thread 72 reopens with a turn recorded on "this computer" (thread_history).
    const at = (seq: number) => ({ turn_id: 'recorded-1', seq });
    ipc.setHistory('w1', '72', [
      {
        turn_id: 'recorded-1',
        conversation_id: '72',
        conversation_uuid: null,
        prompt: 'Give me a quick overview of @README.md',
        mentions: ['README.md'],
        started_at: 1,
        finished_at: 2,
        events: [
          { ...at(0), kind: 'status', payload: { phase: 'running' } },
          { ...at(1), kind: 'tool_call', payload: { call_id: 'c1', tool: 'read_file', args_summary: 'README.md', remote: false } },
          { ...at(2), kind: 'tool_result', payload: { call_id: 'c1', ok: true, summary: '# Elitea platform', truncated: false } },
          { ...at(3), kind: 'text_delta', payload: { text: 'The repository is a **Go monorepo** with the desktop app in `apps/elitea-desktop`.' } },
          { ...at(4), kind: 'status', payload: { phase: 'done' } },
          { ...at(5), kind: 'done', payload: { committed: true, conversation_id: '72', message_ids: [], changed_files: 1 } },
        ],
        changes: [{ path: 'docs/notes.md', status: 'added', added: 2, removed: 0, diff: '@@ -0,0 +1,2 @@\n+# Notes\n+Overview.' }],
        events_truncated: false,
        state: 'done',
        live: false,
      },
    ]);
  }
  const turnId = (): string => `turn-${String(ipc.calls.started.length)}`;
  ipc.setChanges('turn-1', {
    files: [
      { path: 'src/main.rs', status: 'modified', added: 3, removed: 1, diff: '@@ -1,3 +1,5 @@\n fn main() {\n-    println!("hi");\n+    let name = "elitea";\n+    println!("hi {name}");\n+    run();\n }' },
      { path: 'docs/overview.md', status: 'added', added: 12, removed: 0, diff: '@@ -0,0 +1,2 @@\n+# Overview\n+The platform in one page.' },
    ],
  });
  (window as unknown as { __harness: unknown }).__harness = {
    ipc,
    appIpc,
    indexIpc,
    handoffIpc,
    feed: useActivityFeed,
    /** The cloud copy stops on an approval (`&cloud`, after a message was sent); `cards: 2` is a pause only the chat can answer. */
    playCloudApproval(cards = 1) {
      const digest = `sha256:${'a'.repeat(64)}`;
      const card = (n: number) => ({ type: 'hitl', interrupt_id: `int-${String(n)}`, call_digest: digest, guardrail_type: 'sensitive_tool', node_name: 'sensitive_tool_guard', message: 'Deleting a file requires approval.', available_actions: ['approve', 'reject'], routes: {}, tool_call_id: `call-${String(n)}`, tool_name: 'delete_file', toolkit_name: 'artifacts', toolkit_type: 'artifact', action_label: 'Delete file', tool_args: { path: 'report.pdf' } });
      const list = Array.from({ length: cards }, (_, i) => card(i + 1));
      const meta = { thread_id: 'thread-root', chat_project_id: 42, message: 'Deleting a file requires approval.', hitl_interrupt: list[0], hitl_interrupts: list, node_name: 'sensitive_tool_guard', available_actions: ['approve', 'reject'], routes: {}, edit_state_key: null };
      const frame = (payload: Record<string, unknown>): string => JSON.stringify({ message_id: 'e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44', question_id: null, ...payload });
      streams.emit('execution.node_event', frame({ type: 'agent_start' }));
      streams.emit('execution.node_event', frame({ type: 'agent_hitl_interrupt', content: 'Deleting a file requires approval.', response_metadata: meta }));
    },
    /** A reply streaming from the cloud copy of a thread (`&cloud`): `stage` 'running' stops mid-answer, 'done' finishes it. */
    playCloudReply(stage: 'running' | 'done' = 'done') {
      const frame = (payload: Record<string, unknown>): string => JSON.stringify({ message_id: 'e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44', question_id: null, ...payload });
      streams.emit('execution.node_event', frame({ type: 'agent_start' }));
      streams.emit('execution.node_event', frame({ type: 'agent_llm_start' }));
      streams.emit('execution.node_event', frame({ type: 'agent_llm_chunk', content: 'Running the linter in the cloud copy… ' }));
      if (stage === 'running') return;
      streams.emit('execution.node_event', frame({ type: 'agent_llm_chunk', content: 'It is clean: **0 warnings**.' }));
      streams.emit('execution.node_event', frame({ type: 'pipeline_finish' }));
    },
    /** A shell notice (the banner under the toolbar). */
    notice: (message: string) => useDesktopLayout.getState().setNotice(message),
    router,
    /** The index builds: `n` of `total` files read (the island and the inspector follow). */
    playIndex(done = 40, total = 120) {
      const building = { state: 'building' as const, files: 0, entities: 0, relations: 0, vectors: 0, chunks: 0, last_run: null, changed_files: 0, error: null, policy_off: false, on_disk: true, watching: false };
      indexIpc.emit({ workspace_id: 'w1', phase: 'listing', message: null, status: building });
      indexIpc.emit({ workspace_id: 'w1', phase: 'parsing', message: `[extract] Parsing ${String(total)} files`, status: building });
      indexIpc.emit({ workspace_id: 'w1', phase: 'building', message: `[progress] Processed ${String(done)} files`, status: building });
    },
    /** A send to the cloud, part way through the upload. */
    playHandoff(done = 3, total = 10) {
      handoffIpc.emit({ conversation_uuid: 'c0ffee00-0000-4000-8000-000000000072', phase: 'upload', done, total, message: null });
    },
    /** The Local work threads (`&threads`) in the four activity states: 71 runs, 72 needs you, 73 is unread, 74 is open and read. */
    async playActivity() {
      const feed = useActivityFeed.getState();
      const owner = (conversationId: string) => ({ workspaceId: 'w1', workspaceName: 'elitea-platform', conversationId });
      const play = (turnId: string, conversationId: string, end: 'running' | 'approval' | 'done') => {
        feed.registerTurn(turnId, owner(conversationId));
        ipc.emit({ turn_id: turnId, seq: 1, kind: 'status', payload: { phase: 'running' } });
        if (end === 'approval') ipc.emit({ turn_id: turnId, seq: 2, kind: 'approval_request', payload: { request_id: `r-${turnId}`, tool: 'run_command', title: 'Run cargo test', detail: '', reason: '', can_remember: false } });
        if (end === 'done') ipc.emit({ turn_id: turnId, seq: 2, kind: 'done', payload: { committed: true, conversation_id: conversationId, message_ids: [], changed_files: 0 } });
      };
      await router.navigate({ to: '/workspaces/$workspaceId', params: { workspaceId: 'w1' }, search: { conversation: '74' } });
      play('act-71', '71', 'running');
      play('act-72', '72', 'approval');
      play('act-73', '73', 'done');
      play('act-74', '74', 'done');
    },
    playTurn(stage: 'running' | 'approval' | 'done' = 'done') {
      const id = turnId();
      const at = (seq: number) => ({ turn_id: id, seq });
      ipc.emit({ ...at(1), kind: 'status', payload: { phase: 'running', project_instructions: ['AGENTS.md'] } });
      ipc.emit({ ...at(2), kind: 'text_delta', payload: { text: 'Reading the repository layout first.' } });
      ipc.emit({ ...at(3), kind: 'tool_call', payload: { call_id: 'c1', tool: 'read_file', args_summary: 'README.md', remote: false } });
      ipc.emit({ ...at(4), kind: 'tool_result', payload: { call_id: 'c1', ok: true, summary: '# Elitea platform\nGo monorepo…', truncated: false } });
      ipc.emit({ ...at(4), kind: 'usage', payload: { prompt_tokens: 12_400, completion_tokens: 1200, total_tokens: 13_600 } });
      ipc.emit({ ...at(5), kind: 'tool_call', payload: { call_id: 'c2', tool: 'run_command', args_summary: 'cargo test -p worker', remote: false } });
      if (stage === 'running') return;
      ipc.emit({
        ...at(6),
        kind: 'approval_request',
        payload: { request_id: 'r1', tool: 'run_command', title: 'Run cargo test', detail: 'Runs the worker tests', command: ['cargo', 'test', '-p', 'worker'], reason: 'not in the allow list', can_remember: true },
      });
      if (stage === 'approval') return;
      ipc.emit({ ...at(7), kind: 'tool_result', payload: { call_id: 'c2', ok: true, summary: 'test result: ok. 42 passed', truncated: false } });
      ipc.emit({ ...at(8), kind: 'text_delta', payload: { text: '\n\nAll 42 worker tests pass. I added docs/overview.md and tidied main.rs.' } });
      ipc.emit({ ...at(9), kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: ['m1'], changed_files: 2 } });
      ipc.emit({ ...at(10), kind: 'status', payload: { phase: 'done' } });
    },
  };

  createRoot(container).render(
    <StrictMode>
      <AppProviders>
        <AppIpcProvider ipc={appIpc}>
          <WorkspaceIpcProvider ipc={ipc}>
            <IndexIpcProvider ipc={indexIpc}>
              <HandoffIpcProvider ipc={handoffIpc}>
                <RouterProvider router={router} />
              </HandoffIpcProvider>
            </IndexIpcProvider>
          </WorkspaceIpcProvider>
        </AppIpcProvider>
      </AppProviders>
    </StrictMode>,
  );
}
