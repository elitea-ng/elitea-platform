/**
 * "Run in cloud" for a pipeline (#1239 A3): an ordinary cloud conversation
 * with the pipeline as participant, its turn started on the chat's execution
 * route, then the chat opens. The desktop host is never asked to run it.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { act, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import type { Application } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';

import { server } from '../../test/setup';

import { isCloudOnlyPick, type AgentSelection } from './useAgentSelection';
import { pipelineComposerBinding, useRunInCloud, type RunInCloud } from './useRunInCloud';

const BASE = '/api/v2';
const UUID = 'c0ffee00-0000-4000-8000-000000000088';
const globals = globalThis as unknown as Record<string, unknown>;

function selection(overrides: Partial<AgentSelection> = {}): AgentSelection {
  return {
    agents: [{ id: '6', name: 'Release pipeline' } as Application],
    versions: [{ id: 11, name: 'base', agentType: 'pipeline' }],
    conversations: [],
    agentId: '6',
    versionId: '11',
    conversationId: '',
    loading: false,
    empty: false,
    selectAgent: () => undefined,
    selectVersion: () => undefined,
    selectConversation: () => undefined,
    ...overrides,
  };
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('isCloudOnlyPick', () => {
  it('is a pipeline version, or an agent listed as a pipeline', () => {
    expect(isCloudOnlyPick(selection())).toBe(true);
    expect(isCloudOnlyPick(selection({ versions: [{ id: 11, name: 'base', agentType: 'openai' }] }))).toBe(false);
    expect(isCloudOnlyPick(selection({ versions: [], pipelineIds: new Set(['6']) }))).toBe(true);
    expect(isCloudOnlyPick(selection({ agentId: '' }))).toBe(false);
  });
});

describe('useRunInCloud', () => {
  function serve(executionStatus = 200): { created: unknown[]; participants: unknown[]; executions: { url: string; body: unknown }[] } {
    const calls = { created: [] as unknown[], participants: [] as unknown[], executions: [] as { url: string; body: unknown }[] };
    server.use(
      http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })),
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, async ({ request }) => {
        calls.created.push(await request.json());
        return HttpResponse.json({ id: 88, uuid: UUID, name: 'ship it' });
      }),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/42/88`, async ({ request }) => {
        calls.participants.push(await request.json());
        return HttpResponse.json([
          { id: 30, entity_name: 'user', entity_meta: { id: 3 } },
          { id: 31, entity_name: 'application', entity_meta: { id: 6, project_id: 42 }, entity_settings: { agent_type: 'pipeline', version_id: 11 } },
        ]);
      }),
      http.post(`${BASE}/elitea_core/messages/prompt_lib/42/${UUID}`, async ({ request }) => {
        calls.executions.push({ url: request.url, body: await request.json() });
        return executionStatus === 200
          ? HttpResponse.json({ execution_id: 'e1', events_url: '/api/v2/executions/42/e1/events' })
          : HttpResponse.json({ error: 'refused' }, { status: executionStatus });
      }),
    );
    return calls;
  }

  /** Mounts the hook on a workspace route beside a chat route; answers its latest value. */
  async function mountHook(): Promise<{ current: () => RunInCloud }> {
    let latest: { cloud: RunInCloud; ready: boolean } | undefined;
    function Probe(): null {
      const cloud = useRunInCloud(42, selection());
      latest = { cloud, ready: useGetCurrentAuthor().isSuccess };
      return null;
    }
    const rootRoute = createRootRoute();
    const workspace = createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: Probe });
    const chat = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => <p>chat page</p> });
    const router = createRouter({ routeTree: rootRoute.addChildren([workspace, chat]), history: createMemoryHistory({ initialEntries: ['/workspaces'] }) });
    render(
      <AppProviders>
        <RouterProvider router={router} />
      </AppProviders>,
    );
    // A conversation is created for the signed-in user: wait until it is known.
    await waitFor(() => expect(latest?.ready).toBe(true));
    return { current: () => latest!.cloud };
  }

  async function runOnce(prompt: string): Promise<boolean> {
    const hook = await mountHook();
    let started = false;
    await act(async () => {
      started = await hook.current().run(prompt);
    });
    if (!started) expect(hook.current().error).toMatch(/could not be started in the cloud/);
    return started;
  }

  it('starts the pipeline in a normal cloud conversation and opens its chat', async () => {
    const calls = serve();
    expect(await runOnce('ship it')).toBe(true);
    // A normal conversation: no Local work source, so any Chats window continues it.
    expect(calls.created).toEqual([{ name: 'ship it', is_private: true }]);
    expect(calls.participants).toEqual([
      [
        { entity_name: 'user', entity_meta: { id: 3 } },
        {
          entity_name: 'application',
          entity_meta: { id: 6, name: 'Release pipeline', project_id: 42 },
          entity_settings: { version_id: 11, agent_type: 'pipeline', variables: [], icon_meta: {} },
        },
      ],
    ]);
    expect(calls.executions).toHaveLength(1);
    const [execution] = calls.executions;
    expect(execution?.url).toContain('execution_contract=agent.execute.application.v1');
    expect(execution?.body).toMatchObject({ project_id: 42, conversation_uuid: UUID, participant_id: 31, payload: { user_input: 'ship it' } });
    expect(await screen.findByText('chat page')).toBeInTheDocument();
  });

  it('says so and stays on the workspace when the cloud refuses the turn', async () => {
    serve(422);
    expect(await runOnce('ship it')).toBe(false);
    expect(screen.queryByText('chat page')).not.toBeInTheDocument();
  });
});

describe('pipelineComposerBinding', () => {
  const run = (overrides: Partial<RunInCloud> = {}): RunInCloud => ({ run: () => Promise.resolve(true), running: false, error: null, canRun: true, ...overrides });

  it('gates the send on the cloud run alone, never on the local session', () => {
    // Nothing here knows the local session: a busy or unready local host cannot hold the pipeline back.
    expect(pipelineComposerBinding(run()).canSend).toBe(true);
    expect(pipelineComposerBinding(run({ canRun: false })).canSend).toBe(false);
    expect(pipelineComposerBinding(run({ running: true, canRun: false }))).toMatchObject({ canSend: false, busy: true, showMode: false, acceptImages: false });
  });
});

describe('useRunInCloud canRun', () => {
  it('needs a signed-in user and a project, not the local session', async () => {
    server.use(http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })));
    const seen: { projectId: number; canRun: boolean; ready: boolean }[] = [];
    function Probe({ projectId }: { projectId: number }): null {
      const cloud = useRunInCloud(projectId, selection());
      seen.push({ projectId, canRun: cloud.canRun, ready: useGetCurrentAuthor().isSuccess });
      return null;
    }
    const rootRoute = createRootRoute({
      component: () => (
        <>
          <Probe projectId={42} />
          <Probe projectId={0} />
        </>
      ),
    });
    const router = createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: ['/'] }) });
    render(
      <AppProviders>
        <RouterProvider router={router} />
      </AppProviders>,
    );
    // Before the user is known, nothing can run.
    expect(seen.find((entry) => !entry.ready)?.canRun ?? false).toBe(false);
    await waitFor(() => expect(seen.some((entry) => entry.projectId === 42 && entry.ready && entry.canRun)).toBe(true));
    expect(seen.filter((entry) => entry.projectId === 0).every((entry) => !entry.canRun)).toBe(true);
  });
});
