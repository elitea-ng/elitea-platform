/**
 * DEV ONLY — the desktop shell without a host or a deployment, for looking at
 * it in a browser (`npx vite --mode desktop`, then `/?harness`; add
 * `&chrome=mac` for the macOS title-bar overlay, `&mode=light|dark`).
 *
 * The workspace and native-shell IPC are the in-memory fakes the tests use;
 * the few API calls the workspace screens make are answered from canned data.
 * `window.__harness.playTurn()` scripts an agent turn against the fake host.
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
import WorkspaceSessionPage from '@/pages/workspace/WorkspaceSessionPage';
import WorkspacesPage from '@/pages/workspace/WorkspacesPage';
import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { BROWSER_PLATFORM } from '@/shared/desktop/appEvents';
import { createFakeAppIpc, MACOS_PLATFORM } from '@/shared/desktop/appEvents.fake';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { PERMISSION_GROUPS } from '@/shared/lib/permissions';
import { AppIpcProvider, DesktopFrame } from '@/widgets/desktop-shell';

const FOLDERS: Workspace[] = [
  { id: 'w1', path: '/Users/me/code/elitea-platform', name: 'elitea-platform', project_id: 42, is_git: true },
  { id: 'w2', path: '/Users/me/notes/docs', name: 'docs', project_id: null, is_git: false },
];

const API: [string, RegExp, unknown][] = [
  ['GET', /\/projects\/project\/default\//, [{ id: '1', name: 'Public', suspended: false }, { id: '42', name: 'Platform team', suspended: false }]],
  ['GET', /\/elitea_core\/applications\/prompt_lib\/42/, { rows: [{ id: '5', name: 'Coder', tags: [] }, { id: '6', name: 'Reviewer', tags: [] }], total: 2 }],
  [
    'GET',
    /\/elitea_core\/application\/prompt_lib\/42\/\d+/,
    { id: '5', name: 'Coder', description: '', versions: [{ id: '9', name: 'base', status: 'published', agent_type: 'openai' }] },
  ],
  ['POST', /\/elitea_core\/conversations\/prompt_lib\/42$/, { id: 77, name: 'quick overview' }],
  ['GET', /\/elitea_core\/conversations\/prompt_lib\/42/, { rows: [], total: 0 }],
  ['POST', /\/elitea_core\/participants\//, []],
  ['GET', /\/social\/author/, { id: 3, name: 'Me' }],
];

function installApiStub(): void {
  const answer = (method: string, url: string): Response => {
    const path = new URL(url, window.location.origin).pathname;
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
  installApiStub();

  if (params.has('threads')) {
    recordThread('w1', { id: '71', title: 'fix CI on the gateway', updatedAt: 1 });
    recordThread('w1', { id: '72', title: 'quick overview', updatedAt: 2 });
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
  const appIpc = createFakeAppIpc(params.get('chrome') === 'mac' ? { ...MACOS_PLATFORM, vibrancy: false } : BROWSER_PLATFORM);
  const permissions = new Set(Object.values(PERMISSION_GROUPS).flat());

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
      page('/chat', 'Chats'),
    ]),
    history: createMemoryHistory({ initialEntries: [params.get('at') ?? '/workspaces'] }),
  });

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
    router,
    playTurn(stage: 'running' | 'approval' | 'done' = 'done') {
      const id = turnId();
      const at = (seq: number) => ({ turn_id: id, seq });
      ipc.emit({ ...at(1), kind: 'status', payload: { phase: 'running', project_instructions: ['AGENTS.md'] } });
      ipc.emit({ ...at(2), kind: 'text_delta', payload: { text: 'Reading the repository layout first.' } });
      ipc.emit({ ...at(3), kind: 'tool_call', payload: { call_id: 'c1', tool: 'read_file', args_summary: 'README.md', remote: false } });
      ipc.emit({ ...at(4), kind: 'tool_result', payload: { call_id: 'c1', ok: true, summary: '# Elitea platform\nGo monorepo…', truncated: false } });
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
            <RouterProvider router={router} />
          </WorkspaceIpcProvider>
        </AppIpcProvider>
      </AppProviders>
    </StrictMode>,
  );
}
