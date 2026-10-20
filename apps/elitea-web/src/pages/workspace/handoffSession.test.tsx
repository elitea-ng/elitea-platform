/**
 * The cloud handoff on the Workspace session page: the Send to cloud button
 * (only when the policy allows), the badge and banner per state, the read-only
 * composer and live server transcript while the thread is away, the take back
 * flow (opened by its button or by a web request), and the badge on the
 * sidebar's thread row. Real hooks over MSW, the host over in-memory fakes.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { WorkspaceIpcProvider } from '@/features/workspace';
import { HandoffIpcProvider, readThreadUuids, useTakeBackRequests } from '@/features/workspace-handoff';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { createFakeHandoffIpc, type FakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';

import { server } from '../../test/setup';

import WorkspaceSessionPage from './WorkspaceSessionPage';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';
const FOLDER: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };

function mount(handoff: FakeHandoffIpc, path = '/workspaces/w1?conversation=77'): void {
  const rootRoute = createRootRoute();
  const session = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces/$workspaceId', component: WorkspaceSessionPage });
  const chat = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => <p>chat page</p> });
  const router = createRouter({ routeTree: rootRoute.addChildren([session, chat]), history: createMemoryHistory({ initialEntries: [path] }) });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={createFakeWorkspaceIpc({ workspaces: [FOLDER] })}>
        <HandoffIpcProvider ipc={handoff}>
          <RouterProvider router={router} />
        </HandoffIpcProvider>
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
}

function serve(messages: { id: string; role: 'user' | 'assistant'; content: string }[] = []): void {
  server.use(
    http.get(`${BASE}/projects/project/default/1`, () => HttpResponse.json([{ id: '1', name: 'Public', suspended: false }, { id: '42', name: 'Marketing', suspended: false }])),
    http.get(`${BASE}/elitea_core/application_skills/prompt_lib/42/9`, () => HttpResponse.json({ items: [], total: 0, page: 1, page_size: 0, total_pages: 1 })),
    http.get(`${BASE}/elitea_core/applications/prompt_lib/42`, () =>
      HttpResponse.json({ rows: [{ id: '5', name: 'Coder', tags: [], created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', owner_id: '1', is_forked: false, meta: null, has_interrupt: false }], total: 1 }),
    ),
    http.get(`${BASE}/elitea_core/application/prompt_lib/42/5`, () =>
      HttpResponse.json({
        id: '5', name: 'Coder', description: '', icon: '', owner_id: '1', created_at: '2026-01-01T00:00:00Z',
        versions: [{ id: '9', name: 'base', status: 'published', agent_type: 'openai', created_at: '2026-01-01T00:00:00Z' }],
      }),
    ),
    http.get(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ rows: [], total: 0 })),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })),
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/42/77`, () => HttpResponse.json({ id: 77, uuid: UUID, name: 'fix the build', source: 'local_work' })),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/42/77`, () =>
      HttpResponse.json({ items: [...messages].reverse().map((m) => ({ ...m, uid: m.id, conversation_id: '77' })), total: messages.length, page: 1, page_size: 100, total_pages: 1 }),
    ),
  );
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  localStorage.clear();
  for (const pending of ['77', UUID, 'another-thread']) useTakeBackRequests.getState().clear(pending);
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

/** Opens the composer footer's run-location menu. */
async function openLocation(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByTestId('executor-badge'));
}

describe('a thread running here', () => {
  it('offers Send to cloud from the run-location menu and shows Local', async () => {
    serve();
    const handoff = createFakeHandoffIpc();
    const user = userEvent.setup();
    mount(handoff);
    const session = await screen.findByTestId('workspace-session');
    await waitFor(() => expect(within(session).getByTestId('executor-badge')).toHaveTextContent('Local'));
    await waitFor(() => expect(handoff.calls.statuses).toContain(UUID));
    await openLocation(user);
    expect(await screen.findByRole('menuitem', { name: 'Send to cloud…' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByTestId('cloud-thread-banner')).toBeNull();
    // The host was asked about this thread by its UUID, with its project.
    expect(handoff.calls.statuses).toContain(UUID);
  });

  it('opens the folder\'s Handoff settings from the run-location menu', async () => {
    serve();
    const user = userEvent.setup();
    const handoff = createFakeHandoffIpc();
    mount(handoff);
    await waitFor(() => expect(handoff.calls.statuses).toContain(UUID));
    await openLocation(user);
    await user.click(await screen.findByRole('menuitem', { name: 'Handoff settings…' }));
    const dialog = await screen.findByRole('dialog', { name: 'Handoff settings' });
    await waitFor(() => expect(handoff.calls.settingsGets).toEqual(['w1']));
    expect(await within(dialog).findByTestId('handoff-settings-exclusions')).toBeInTheDocument();
  });

  it('hides Send to cloud when the policy does not allow it', async () => {
    serve();
    const handoff = createFakeHandoffIpc({ allowed: false });
    mount(handoff);
    const session = await screen.findByTestId('workspace-session');
    // Local, and nothing to do from here: the badge reports, it opens no menu.
    await waitFor(() => expect(within(session).getByTestId('executor-badge')).toBeDisabled());
  });

  it('hides Send to cloud on a new thread that has no conversation yet', async () => {
    serve();
    const handoff = createFakeHandoffIpc();
    mount(handoff, '/workspaces/w1');
    await screen.findByTestId('workspace-session');
    expect(screen.queryByTestId('executor-badge')).toBeNull();
    expect(handoff.calls.statuses).toEqual([]);
  });

  it('sends the thread and then shows it as running in the cloud', async () => {
    serve([{ id: 'm1', role: 'user', content: 'Fix the build' }]);
    const user = userEvent.setup();
    const handoff = createFakeHandoffIpc();
    mount(handoff);
    await waitFor(() => expect(handoff.calls.statuses).toContain(UUID));
    await openLocation(user);
    await user.click(await screen.findByRole('menuitem', { name: 'Send to cloud…' }));

    const dialog = await screen.findByRole('dialog', { name: 'Send this thread to the cloud' });
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Send to cloud' })).toBeEnabled());
    await user.click(within(dialog).getByRole('button', { name: 'Send to cloud' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(handoff.calls.sends).toEqual([{ workspaceId: 'w1', conversationUuid: UUID, leave: [], confirmIncludes: false }]);
    expect(await screen.findByTestId('cloud-thread-banner')).toBeInTheDocument();
    await waitFor(() => expect(screen.getAllByTestId('executor-badge')[0]).toHaveTextContent('In cloud'));
  });
});

describe('a thread running in the cloud', () => {
  const away = { state: 'CLOUD_IDLE', executor: 'cloud', read_only: true, sent_from_here: true, epoch: 2 } as const;

  it('shows the badge and banner, turns the composer off and follows the cloud transcript', async () => {
    serve([
      { id: 'm1', role: 'user', content: 'Fix the build' },
      { id: 'm2', role: 'assistant', content: 'Done in the cloud.' },
    ]);
    const handoff = createFakeHandoffIpc({ status: away });
    mount(handoff);

    const banner = await screen.findByTestId('cloud-thread-banner');
    expect(banner).toHaveTextContent('This thread is running in the cloud executor. Local changes are not seen by it.');
    expect(within(await screen.findByTestId('workspace-session')).getAllByTestId('executor-badge')[0]).toHaveTextContent('In cloud');
    // The transcript is the server's, and it is the only one shown.
    // Looked up again on every try: the session may re-render while the first reads settle.
    await waitFor(() => expect(within(screen.getByTestId('cloud-transcript')).getByText('Done in the cloud.')).toBeInTheDocument());
    // Composer off: no typing, no sending.
    const composer = screen.getByTestId('workspace-composer');
    expect(within(composer).getByRole('textbox')).toBeDisabled();
    expect(screen.getByTestId('workspace-send-button')).toBeDisabled();
    expect(screen.queryByRole('menuitem', { name: 'Send to cloud…' })).toBeNull();
  });

  it('shows a take back that waits for the running cloud turn', async () => {
    serve();
    mount(createFakeHandoffIpc({ status: { ...away, state: 'CLOUD_RUNNING', reclaim_pending: true } }));
    expect(await screen.findByTestId('banner-reclaim-pending')).toBeInTheDocument();
  });

  it('shows Returning for a thread on its way back', async () => {
    serve();
    mount(createFakeHandoffIpc({ status: { ...away, state: 'RECLAIMING', executor: 'returning' } }));
    const banner = await screen.findByTestId('cloud-thread-banner');
    expect(within(banner).getByTestId('executor-badge')).toHaveTextContent('Returning');
  });

  it('takes the thread back: the banner goes, the composer comes back', async () => {
    serve();
    const user = userEvent.setup();
    const handoff = createFakeHandoffIpc({ status: away });
    mount(handoff);
    await user.click(await within(await screen.findByTestId('cloud-thread-banner')).findByRole('button', { name: 'Take back' }));

    const dialog = await screen.findByRole('dialog', { name: 'Take this thread back' });
    expect(handoff.calls.reclaims).toEqual([UUID]);
    handoff.setStatus({ state: 'LOCAL', executor: 'local', read_only: false });
    // The review shows the diffs of what the take back writes, in the changed-files card.
    const diffs = await within(dialog).findByTestId('takeback-diffs');
    expect(within(diffs).getByTestId('changed-file')).toHaveTextContent('src/main.rs');
    await user.click(await within(dialog).findByRole('button', { name: 'Apply to my folder' }));
    await within(dialog).findByTestId('takeback-result');
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));

    await waitFor(() => expect(screen.queryByTestId('cloud-thread-banner')).toBeNull());
    await waitFor(() => expect(within(screen.getByTestId('workspace-composer')).getByRole('textbox')).toBeEnabled());
  });

  it('erases the cloud copy after asking, and the thread is local again', async () => {
    serve();
    const user = userEvent.setup();
    const handoff = createFakeHandoffIpc({ status: away });
    mount(handoff);
    await user.click(await within(await screen.findByTestId('cloud-thread-banner')).findByRole('button', { name: 'Erase cloud copy' }));
    const dialog = await screen.findByRole('dialog', { name: 'Erase the cloud copy?' });
    expect(handoff.calls.erases).toEqual([]);
    await user.click(within(dialog).getByRole('button', { name: 'Erase cloud copy' }));
    await waitFor(() => expect(handoff.calls.erases).toEqual([UUID]));
    await waitFor(() => expect(screen.queryByTestId('cloud-thread-banner')).toBeNull());
  });

  it('opens the take back when a web request asks for this thread', async () => {
    serve();
    const handoff = createFakeHandoffIpc({ status: away });
    useTakeBackRequests.getState().request(UUID.toUpperCase());
    mount(handoff);
    expect(await screen.findByRole('dialog', { name: 'Take this thread back' })).toBeInTheDocument();
    expect(handoff.calls.reclaims).toEqual([UUID]);
    // Picked up once.
    expect(useTakeBackRequests.getState().requested).toBeNull();
  });

  it('leaves a request for another thread alone', async () => {
    serve();
    useTakeBackRequests.getState().request('another-thread');
    mount(createFakeHandoffIpc({ status: away }));
    await screen.findByTestId('cloud-thread-banner');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(useTakeBackRequests.getState().requested).toBe('another-thread');
  });
});

describe('the thread list', () => {
  it('remembers the conversation UUID of a thread it opened, for the sidebar\'s badge', async () => {
    serve();
    mount(createFakeHandoffIpc());
    await waitFor(() => expect(readThreadUuids('w1')).toEqual({ '77': UUID }));
  });
});
