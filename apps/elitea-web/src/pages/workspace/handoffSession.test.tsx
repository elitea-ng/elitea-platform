/**
 * The cloud handoff on the Workspace session page: the Send to cloud button
 * (only when the policy allows), the badge and banner per state, the read-only
 * composer and live server transcript while the thread is away, the take back
 * flow (opened by its button or by a web request), and the badge on the
 * sidebar's thread row. Real hooks over MSW, the host over in-memory fakes.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { WorkspaceIpcProvider } from '@/features/workspace';
import { HandoffIpcProvider, readThreadUuids, useTakeBackRequests } from '@/features/workspace-handoff';
import { useActivityFeed } from '@/widgets/desktop-shell';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
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
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/42/77`, () => HttpResponse.json({ id: 77, uuid: UUID, name: 'fix the build', source: 'local_work', participants: [{ id: 3, entity_name: 'application', name: 'Coder', meta: {} }] })),
    http.get(`${BASE}/elitea_core/messages/prompt_lib/42/77`, () =>
      HttpResponse.json({ items: [...messages].reverse().map((m) => ({ ...m, uid: m.id, conversation_id: '77' })), total: messages.length, page: 1, page_size: 100, total_pages: 1 }),
    ),
  );
}

beforeEach(() => {
  // The chat's message list scrolls its end into view; jsdom has no layout.
  Element.prototype.scrollIntoView = () => undefined;
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

/** Opens the small ⌄ menu beside the composer footer's run-location button. */
async function openLocation(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByTestId('run-location-more'));
}

describe('a thread running here', () => {
  it('offers Send to cloud as a button, the rest in the small menu beside it', async () => {
    serve();
    const handoff = createFakeHandoffIpc();
    const user = userEvent.setup();
    mount(handoff);
    const session = await screen.findByTestId('workspace-session');
    await waitFor(() => expect(within(session).getByTestId('run-location-action')).toHaveTextContent('Send to cloud'));
    await waitFor(() => expect(handoff.calls.statuses).toContain(UUID));
    await openLocation(user);
    expect(await screen.findByRole('menuitem', { name: 'Handoff settings…' })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /Send to cloud/ })).toBeNull();
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
    // Local, and nothing to do from here: no move is offered.
    await waitFor(() => expect(handoff.calls.statuses).toContain(UUID));
    expect(within(session).queryByTestId('run-location-action')).toBeNull();
  });

  it('hides Send to cloud on a new thread that has no conversation yet', async () => {
    serve();
    const handoff = createFakeHandoffIpc();
    mount(handoff, '/workspaces/w1');
    await screen.findByTestId('workspace-session');
    expect(screen.queryByTestId('run-location-action')).toBeNull();
    expect(handoff.calls.statuses).toEqual([]);
  });

  it('sends the thread and then shows it as running in the cloud', async () => {
    serve([{ id: 'm1', role: 'user', content: 'Fix the build' }]);
    const user = userEvent.setup();
    const handoff = createFakeHandoffIpc();
    mount(handoff);
    await waitFor(() => expect(handoff.calls.statuses).toContain(UUID));
    await user.click(await screen.findByTestId('run-location-action'));

    const dialog = await screen.findByRole('dialog', { name: 'Send this thread to the cloud' });
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Send to cloud' })).toBeEnabled());
    await user.click(within(dialog).getByRole('button', { name: 'Send to cloud' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(handoff.calls.sends).toEqual([{ workspaceId: 'w1', conversationUuid: UUID, leave: [], confirmIncludes: false }]);
    expect(await screen.findByTestId('cloud-thread-banner')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId('run-location-action')).toHaveTextContent('Take back'));
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
    expect(within(await screen.findByTestId('workspace-session')).getByTestId('run-location')).toHaveAttribute('data-executor', 'cloud');
    // The transcript is the server's, and it is the only one shown.
    // Looked up again on every try: the session may re-render while the first reads settle.
    await waitFor(() => expect(within(screen.getByTestId('cloud-transcript')).getByText('Done in the cloud.')).toBeInTheDocument());
    // Drawn like the local transcript: your prompt, the agent's text; no avatars, reactions or timestamps.
    const transcript = screen.getByTestId('cloud-transcript');
    expect(within(transcript).getByTestId('your-prompt')).toHaveTextContent('Fix the build');
    expect(within(transcript).getByTestId('turn-transcript')).toBeInTheDocument();
    expect(within(transcript).queryByTestId('chat-message-time')).toBeNull();
    expect(within(transcript).queryByText(/ago$/)).toBeNull();
    expect(within(transcript).queryByRole('img')).toBeNull();
    expect(within(transcript).queryByRole('button', { name: /like|bookmark/i })).toBeNull();
    // The composer stays on, for the cloud copy: no plan mode, the agent picker disabled.
    const composer = screen.getByTestId('workspace-composer');
    expect(within(composer).getByRole('textbox')).toBeEnabled();
    expect(within(composer).getByRole('textbox')).toHaveAttribute('placeholder', 'Message the cloud copy…');
    expect(screen.queryByTestId('mode-picker')).toBeNull();
    expect(screen.getByTestId('agent-chip')).toBeDisabled();
    expect(screen.getByTestId('run-location-action')).toHaveTextContent('Take back');
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
    expect(banner).toHaveAttribute('data-executor', 'returning');
    expect(within(banner).getByTestId('banner-returning')).toBeInTheDocument();
    expect(screen.getByTestId('run-location-action')).toHaveTextContent('Returning');
    expect(screen.getByTestId('run-location-action')).toBeDisabled();
    // Read-only: nothing can be typed or sent to a thread on its way back.
    const composer = screen.getByTestId('workspace-composer');
    expect(within(composer).getByRole('textbox')).toBeDisabled();
    expect(screen.getByTestId('workspace-send-button')).toBeDisabled();
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

describe('chatting with the cloud copy', () => {
  const cloud = { state: 'CLOUD_IDLE', executor: 'cloud', read_only: true, sent_from_here: true, epoch: 2 } as const;
  const EVENTS_URL = '/api/v2/executions/42/exec-1/events';
  const RESPONSE_ID = 'e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44';
  let registry: TestEventSourceRegistry;
  let started: { query: string | null; body: Record<string, unknown> }[];

  beforeEach(() => {
    registry = installTestEventSource();
    started = [];
    useActivityFeed.getState().reset();
  });
  afterEach(() => registry.restore());

  function acceptStart(): void {
    server.use(
      http.post(`${BASE}/elitea_core/messages/prompt_lib/42/${UUID}`, async ({ request }) => {
        started.push({ query: new URL(request.url).searchParams.get('execution_contract'), body: (await request.json()) as Record<string, unknown> });
        return HttpResponse.json({ task_id: 'exec-1', events_url: EVENTS_URL, response_message_id: RESPONSE_ID });
      }),
    );
  }

  const frame = (payload: Record<string, unknown>): string => JSON.stringify({ message_id: RESPONSE_ID, question_id: null, ...payload });

  async function typeAndSend(user: ReturnType<typeof userEvent.setup>, text: string): Promise<void> {
    const composer = await screen.findByTestId('workspace-composer');
    await waitFor(() => expect(within(composer).getByTestId('workspace-send-button')).toBeDefined());
    await user.type(within(composer).getByRole('textbox'), text);
    await waitFor(() => expect(screen.getByTestId('workspace-send-button')).toBeEnabled());
    await user.click(screen.getByTestId('workspace-send-button'));
  }

  it('sends the message to the SERVER conversation (the agent participant), not the local agent loop', async () => {
    serve();
    acceptStart();
    const user = userEvent.setup();
    mount(createFakeHandoffIpc({ status: cloud }));
    await typeAndSend(user, 'run the tests in the cloud');
    await waitFor(() => expect(started).toHaveLength(1));
    expect(started[0]?.query).toBe('agent.execute.application.v1');
    expect(started[0]?.body).toMatchObject({ conversation_uuid: UUID, participant_id: 3, payload: { user_input: 'run the tests in the cloud' } });
    expect(typeof started[0]?.body['question_id']).toBe('string');
  });

  it('streams the reply into the transcript, shows the cloud turn in the activity feed, and clears it when the turn ends', async () => {
    serve([{ id: 'm1', role: 'user', content: 'Fix the build' }]);
    acceptStart();
    const user = userEvent.setup();
    mount(createFakeHandoffIpc({ status: cloud }));
    await typeAndSend(user, 'and run the linter');
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    await waitFor(() => expect(Object.keys(useActivityFeed.getState().cloudRuns)).toEqual([UUID]));

    act(() => {
      registry.emit('execution.node_event', frame({ type: 'agent_start' }));
      registry.emit('execution.node_event', frame({ type: 'agent_llm_start' }));
      registry.emit('execution.node_event', frame({ type: 'agent_llm_chunk', content: 'Linter is clean.' }));
    });
    expect(await within(screen.getByTestId('cloud-transcript')).findByText(/Linter is clean/)).toBeInTheDocument();
    // The composer offers Stop while the cloud turn streams.
    expect(screen.getByRole('button', { name: 'Stop generating' })).toBeInTheDocument();

    act(() => {
      registry.emit('execution.node_event', frame({ type: 'pipeline_finish' }));
    });
    await waitFor(() => expect(useActivityFeed.getState().cloudRuns).toEqual({}));
  });

  it('shows the server\'s refusal as an error, and the turn never shows as running', async () => {
    serve();
    server.use(
      http.post(`${BASE}/elitea_core/messages/prompt_lib/42/${UUID}`, () => HttpResponse.json({ message: 'The cloud executor is not available.' }, { status: 422 })),
    );
    const user = userEvent.setup();
    mount(createFakeHandoffIpc({ status: cloud }));
    await typeAndSend(user, 'hello');
    expect(await screen.findByText('The cloud executor is not available.')).toBeInTheDocument();
    expect(useActivityFeed.getState().cloudRuns).toEqual({});
    // What was typed is kept.
    expect(within(screen.getByTestId('workspace-composer')).getByRole('textbox')).toHaveValue('hello');
  });

  it('offers Take back as a button, Erase cloud copy and Open in Chat in the menu beside it', async () => {
    serve();
    const user = userEvent.setup();
    mount(createFakeHandoffIpc({ status: cloud }));
    expect(await screen.findByTestId('run-location-action')).toHaveTextContent('Take back');
    await user.click(await screen.findByTestId('run-location-more'));
    expect(screen.getByRole('menuitem', { name: 'Erase cloud copy…' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Open in chat' })).toBeInTheDocument();
  });

  describe('a cloud turn that waits on the person', () => {
    const DIGEST = `sha256:${'a'.repeat(64)}`;
    const card = (id: string, call: string): Record<string, unknown> => ({
      type: 'hitl', interrupt_id: id, call_digest: DIGEST, guardrail_type: 'sensitive_tool', node_name: 'sensitive_tool_guard',
      message: 'Deleting a file requires approval.', available_actions: ['approve', 'reject'], routes: {}, tool_call_id: call,
      tool_name: 'delete_file', toolkit_name: 'artifacts', toolkit_type: 'artifact', action_label: 'Delete file', tool_args: { path: 'report.pdf' },
    });
    const pause = (cards: Record<string, unknown>[]): string =>
      frame({
        type: 'agent_hitl_interrupt',
        content: 'Deleting a file requires approval.',
        response_metadata: { thread_id: 'thread-root', chat_project_id: 42, message: 'Deleting a file requires approval.', hitl_interrupt: cards[0], hitl_interrupts: cards, node_name: 'sensitive_tool_guard', available_actions: ['approve', 'reject'], routes: {}, edit_state_key: null },
      });
    let resumed: { query: string | null; body: Record<string, unknown> }[];

    /** Starts a cloud turn that stops on the given approval cards. */
    async function pausedOn(cards: Record<string, unknown>[]): Promise<ReturnType<typeof userEvent.setup>> {
      resumed = [];
      serve([{ id: 'm1', role: 'user', content: 'Fix the build' }]);
      acceptStart();
      server.use(
        http.post(`${BASE}/elitea_core/continue_predict/prompt_lib/42/${UUID}`, async ({ request }) => {
          resumed.push({ query: new URL(request.url).searchParams.get('execution_contract'), body: (await request.json()) as Record<string, unknown> });
          return HttpResponse.json({ events_url: EVENTS_URL, response_message_id: RESPONSE_ID });
        }),
      );
      const user = userEvent.setup();
      mount(createFakeHandoffIpc({ status: cloud }));
      await typeAndSend(user, 'delete the old report');
      await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
      act(() => {
        registry.emit('execution.node_event', frame({ type: 'agent_start' }));
        registry.emit('execution.node_event', pause(cards));
      });
      return user;
    }

    it('shows the approval inline and raises the attention state (island, tray, Dock) for it', async () => {
      await pausedOn([card('int-1', 'call-1')]);
      const approval = await screen.findByTestId('cloud-approval-card');
      // The local approvals' look: the tool, its parameters behind a disclosure, Approve (default) and Deny (destructive).
      expect(within(approval).getByText('Delete file')).toBeInTheDocument();
      expect(within(approval).getByRole('button', { name: 'Parameters' })).toHaveAttribute('aria-expanded', 'false');
      expect(within(approval).getByRole('button', { name: 'Approve' })).toBeInTheDocument();
      expect(within(approval).getByRole('button', { name: 'Deny' })).toBeInTheDocument();
      await waitFor(() => expect(useActivityFeed.getState().cloudRuns[UUID]).toMatchObject({ running: false, waiting: 'Approve: delete_file' }));
    });

    it('answers the approval through the chat\'s resume route, and the turn resumes and finishes', async () => {
      const user = await pausedOn([card('int-1', 'call-1')]);
      const transcript = await screen.findByTestId('cloud-transcript');
      await user.click(await within(transcript).findByRole('button', { name: 'Approve' }));
      await waitFor(() => expect(resumed).toHaveLength(1));
      expect(resumed[0]?.query).toBe('agent.continue.hitl.v1');
      expect(resumed[0]?.body).toEqual({
        project_id: 42, conversation_uuid: UUID, message_id: RESPONSE_ID, thread_id: 'thread-root', hitl_resume: true,
        hitl_decisions: [{ interrupt_id: 'int-1', tool_call_id: 'call-1', action: 'approve', value: '' }],
      });
      // The turn is running again: its stream is open, the waiting state is gone.
      await waitFor(() => expect(useActivityFeed.getState().cloudRuns[UUID]).toMatchObject({ running: true, waiting: null }));
      act(() => {
        registry.emit('execution.node_event', frame({ type: 'agent_llm_chunk', content: 'Deleted report.pdf.' }));
        registry.emit('execution.node_event', frame({ type: 'pipeline_finish' }));
      });
      expect(await within(screen.getByTestId('cloud-transcript')).findByText(/Deleted report\.pdf/)).toBeInTheDocument();
      await waitFor(() => expect(useActivityFeed.getState().cloudRuns).toEqual({}));
    });

    it('shows the server\'s refusal when the answer is not accepted, and keeps the approval answerable', async () => {
      const user = await pausedOn([card('int-1', 'call-1')]);
      server.use(
        http.post(`${BASE}/elitea_core/continue_predict/prompt_lib/42/${UUID}`, () => HttpResponse.json({ message: 'This pause is no longer open.' }, { status: 422 })),
      );
      await user.click(await within(await screen.findByTestId('cloud-transcript')).findByRole('button', { name: 'Approve' }));
      expect(await screen.findByText('This pause is no longer open.')).toBeInTheDocument();
      expect(await within(screen.getByTestId('cloud-transcript')).findByRole('button', { name: 'Approve' })).toBeInTheDocument();
    });

    it('sends the person to the chat in one click for a pause only the chat can answer (several approvals at once)', async () => {
      const user = await pausedOn([card('int-1', 'call-1'), card('int-2', 'call-2')]);
      const inChat = await screen.findByTestId('cloud-answer-in-chat');
      expect(useActivityFeed.getState().cloudRuns[UUID]).toMatchObject({ waiting: 'Answer in chat' });
      await user.click(within(inChat).getByRole('button', { name: 'Answer in chat' }));
      expect(await screen.findByText('chat page')).toBeInTheDocument();
    });
  });
});
