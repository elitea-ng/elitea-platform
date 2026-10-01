/**
 * The RUN-EVENT FEED: `ChatBox`'s `extensions.onAgentEvent` -> `useChatBoxSend`
 * -> `useChatStreamTransport` -> the pipeline editor's flow canvas.
 *
 * WHY THIS TEST EXISTS AT ALL. The receive side of the pipeline editor's run
 * visualisation was complete and tested before this change —
 * `EditorPanel.onRcvAgentEvent` -> `FlowEditor` -> `useRunEvent` ->
 * `parseRunsByEvent` -> the node's `isPerforming` highlight and the "Run N
 * details" node — and NOTHING fed it. `useChatStreamTransport` even accepted
 * an `onAgentEvent` parameter, which no caller passed: dead wiring on both
 * ends of the same wire, invisible to every unit suite because each half was
 * correct on its own. This file pins the JOIN.
 *
 * IT IS DRIVEN THROUGH THE REAL TRANSPORT, not a stub of it. That is the
 * point: the FILTER that decides which frames the canvas may see lives in
 * `useChatStreamTransport` (`shouldForwardAgentEvent`), and a test that
 * mocked the transport would prove only that a callback was passed along —
 * exactly the kind of green that let the dead wiring above survive. The
 * `EventSource` double is the sanctioned substitution for a browser global
 * jsdom does not implement (`shared/api/sse/testing.ts`'s own header).
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useConversationLifecycle } from '@/entities/conversation';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { server } from '@/test/setup';

import { useChatBoxSend, type UseChatBoxSendResult, type UseChatBoxSendParams } from './useChatBoxSend';

const BASE = '/api/v2';
const EVENTS_URL = '/api/v2/executions/7/exec-1/events';
const CONVERSATION_UUID = '00000000-0000-4000-8000-0000000000ff';
/** A pipeline participant: `entity_name: 'application'` is what routes the turn to the application contract. */
const PIPELINE_PARTICIPANT = { id: 42, entity_name: 'application', entity_settings: { agent_type: 'pipeline' } };

const globals = globalThis as unknown as Record<string, unknown>;
let registry: TestEventSourceRegistry;

interface Harness {
  readonly api: { current: UseChatBoxSendResult | undefined };
  readonly agentEvents: { readonly type?: string }[];
  readonly Probe: () => null;
}

function harness(overrides: Partial<UseChatBoxSendParams> = {}, createWithLifecycle = false): Harness {
  const api: { current: UseChatBoxSendResult | undefined } = { current: undefined };
  const agentEvents: { readonly type?: string }[] = [];

  function Probe(): null {
    const lifecycle = useConversationLifecycle(7);
    api.current = useChatBoxSend({
      deps: {
        createConversation: createWithLifecycle ? lifecycle.createConversation : () => Promise.resolve(undefined),
        uploadAttachments: () => Promise.resolve({ success: true, uploaded: [] }),
      },
      setChatHistory: () => undefined,
      conversationUuid: CONVERSATION_UUID,
      projectId: 7,
      projectIdString: '7',
      isAgentsPage: true,
      activeParticipant: PIPELINE_PARTICIPANT,
      participants: [PIPELINE_PARTICIPANT],
      onAgentEvent: (frame) => agentEvents.push(frame),
      ...overrides,
    });
    return null;
  }

  return { api, agentEvents, Probe };
}

/** The real transport requires TanStack mutations. Disable retries so a failed request surfaces as itself. */
function withQueryClient(children: ReactNode): ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

/** One durable progress frame, in the envelope `executions/events.go` writes. */
function nodeEvent(payload: Record<string, unknown>): string {
  return JSON.stringify({ message_id: '11111111-1111-4111-8111-111111111111', question_id: null, ...payload });
}

beforeEach(() => {
  registry = installTestEventSource();
  globals['elitea_ui_config'] = { vite_server_url: BASE, vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.post(`${BASE}/elitea_core/messages/prompt_lib/7/${CONVERSATION_UUID}`, () =>
      HttpResponse.json({ task_id: 'exec-1', events_url: EVENTS_URL, response_message_id: 'resp-1' }),
    ),
  );
});

afterEach(() => {
  window.sessionStorage.clear();
  registry.restore();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
});

describe('useChatBoxSend — the flow editor’s run-event feed', () => {
  it('retains a picked model in conversation participants, start, and regeneration', async () => {
    const selectedSettings = { model_name: 'eu.anthropic.claude-haiku', model_project_id: 1, temperature: 0.6 };
    let participantsBody: unknown;
    let startBody: Record<string, unknown> | undefined;
    let regenerateBody: Record<string, unknown> | undefined;
    let redundantParticipantWrites = 0;
    server.use(
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/7`, async ({ request }) => {
        const body = await request.json() as Record<string, unknown>;
        participantsBody = body['participants'];
        return HttpResponse.json({ id: 776, uuid: CONVERSATION_UUID, name: 'hello' });
      }),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/7/776`, () => {
        redundantParticipantWrites += 1;
        return HttpResponse.json([]);
      }),
      http.post(`${BASE}/elitea_core/messages/prompt_lib/7/${CONVERSATION_UUID}`, async ({ request }) => {
        startBody = await request.json() as Record<string, unknown>;
        return HttpResponse.json({ task_id: 'exec-1', events_url: EVENTS_URL, response_message_id: 'resp-1' });
      }),
      http.post(`${BASE}/elitea_core/regenerate/prompt_lib/7/resp-1`, async ({ request }) => {
        regenerateBody = await request.json() as Record<string, unknown>;
        return HttpResponse.json({ task_id: 'exec-2', events_url: '/api/v2/executions/7/exec-2/events', response_message_id: 'resp-1' });
      }),
    );
    const { api, Probe } = harness({
      isAgentsPage: false,
      activeParticipant: { id: 2, entity_name: 'dummy' },
      participants: [{ id: 2, entity_name: 'dummy' }],
      userId: '5',
      llmSettings: selectedSettings,
      model: { name: 'vllm/CONTINUATION-REPAIR-FIXTURE' },
    }, true);
    render(withQueryClient(<Probe />));
    await act(async () => {
      await api.current?.createConversationForSend('hello');
      await api.current?.startStreamedExecution({
        conversationUuid: CONVERSATION_UUID, payload: { question: 'hello', question_id: 'q-1' },
      });
      await api.current?.regenerateStreamedExecution({ messageId: 'resp-1', questionId: 'q-1', question: 'hello' });
    });
    const expectedSettings = { ...selectedSettings, stream: true };
    expect(participantsBody).toEqual([
      { entity_name: 'user', entity_meta: { id: 5 } },
      { entity_name: 'dummy', entity_meta: {}, entity_settings: { llm_settings: expectedSettings } },
    ]);
    expect(redundantParticipantWrites).toBe(0);
    expect(startBody?.['llm_settings']).toEqual(expectedSettings);
    expect((regenerateBody?.['payload'] as Record<string, unknown> | undefined)?.['llm_settings']).toEqual(expectedSettings);
  });

  it.each([
    { label: 'pipeline host', isAgentsPage: true, model: { name: 'pipeline-model' } },
    { label: 'chat without a model', isAgentsPage: false, model: null },
  ])('preserves creation defaults for $label', async ({ isAgentsPage, model }) => {
    let participantsBody: unknown;
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    server.use(http.post(`${BASE}/elitea_core/conversations/prompt_lib/7`, async ({ request }) => {
      participantsBody = (await request.json() as Record<string, unknown>)['participants'];
      return HttpResponse.json({ id: 776, uuid: CONVERSATION_UUID, name: 'hello' });
    }));
    const { api, Probe } = harness({ isAgentsPage, model }, true);
    render(withQueryClient(<Probe />));
    await act(async () => {
      expect(await api.current?.createConversationForSend('hello')).toEqual({ id: 776, uuid: CONVERSATION_UUID });
    });
    expect(participantsBody).toEqual([]);
    warning.mockRestore();
  });

  it('awaits session refresh and carries access tokens on start and regeneration', async () => {
    const tokenKey = 'credential:https://issuer.example';
    window.sessionStorage.setItem('el.mcp.tokens', JSON.stringify({
      [tokenKey]: {
        access_token: 'old-token', refresh_token: 'refresh-token', issued_at: Date.now() - 10000,
        expires_at: Date.now() - 1, project_id: '7', toolkit_id: '27', client_id: 'client',
        token_endpoint: 'https://issuer.example/token', session_id: 'session',
      },
    }));
    const order: string[] = [];
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post(`${BASE}/elitea_core/mcp_oauth_proxy/7`, () => {
        order.push('refresh');
        return HttpResponse.json({ access_token: 'fresh-token', refresh_token: 'rotated-token', expires_in: 3600 });
      }),
      http.post(`${BASE}/elitea_core/messages/prompt_lib/7/${CONVERSATION_UUID}`, async ({ request }) => {
        order.push('start');
        bodies.push(await request.json() as Record<string, unknown>);
        return HttpResponse.json({ task_id: 'exec-1', events_url: EVENTS_URL, response_message_id: 'resp-1' });
      }),
      http.post(`${BASE}/elitea_core/regenerate/prompt_lib/7/resp-1`, async ({ request }) => {
        order.push('regenerate');
        bodies.push(await request.json() as Record<string, unknown>);
        return HttpResponse.json({ task_id: 'exec-2', events_url: '/api/v2/executions/7/exec-2/events', response_message_id: 'resp-1' });
      }),
    );
    const { api, Probe } = harness();
    render(withQueryClient(<Probe />));
    await act(async () => {
      await api.current?.startStreamedExecution({
        conversationUuid: CONVERSATION_UUID, payload: { question: 'echo marker', question_id: 'q-1', participant_id: 42 },
      });
    });
    await act(async () => {
      await api.current?.regenerateStreamedExecution({ messageId: 'resp-1', questionId: 'q-1', question: 'echo marker' });
    });
    expect(order).toEqual(['refresh', 'start', 'regenerate']);
    const expected = { [tokenKey]: { access_token: 'fresh-token', session_id: 'session' } };
    expect(bodies[0]?.['mcp_tokens']).toEqual(expected);
    const regenerationPayload = bodies[1]?.['payload'] as Record<string, unknown> | undefined;
    expect(regenerationPayload?.['mcp_tokens']).toEqual(expected);
    expect(JSON.stringify(bodies)).not.toContain('refresh-token');
    expect(JSON.stringify(bodies)).not.toContain('rotated-token');
  });

  it('forwards a graph frame to onAgentEvent and withholds a per-token chunk', async () => {
    const { api, agentEvents, Probe } = harness();
    render(withQueryClient(<Probe />));

    await act(async () => {
      await api.current?.startStreamedExecution({
        conversationUuid: CONVERSATION_UUID,
        payload: { question: 'run the graph', question_id: 'q-1', participant_id: 42 },
      });
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    act(() => {
      // A graph frame. `agent_on_*` is a PREFIX test in the forwarding
      // contract, not a closed list, because the worker emits more of them
      // than the baseline enumerated.
      registry.emit(
        'execution.node_event',
        nodeEvent({ type: 'agent_on_tool_node', response_metadata: { tool_name: 'autotest_llm_node' } }),
      );
      // A per-token chunk. It reaches the reducer (it is how the answer is
      // rendered) and must NOT reach the run timeline, which has no entry for
      // it — forwarding it would push one canvas update per token.
      registry.emit('execution.node_event', nodeEvent({ type: 'agent_llm_chunk', content: 'MOCK: ' }));
    });

    await waitFor(() => expect(agentEvents).toHaveLength(1));
    expect(agentEvents[0]?.type).toBe('agent_on_tool_node');
    expect(agentEvents.map((frame) => frame.type)).not.toContain('agent_llm_chunk');
  });

  it('forwards the node lifecycle frames the run timeline is built out of', async () => {
    const { api, agentEvents, Probe } = harness();
    render(withQueryClient(<Probe />));

    await act(async () => {
      await api.current?.startStreamedExecution({
        conversationUuid: CONVERSATION_UUID,
        payload: { question: 'run the graph', question_id: 'q-1', participant_id: 42 },
      });
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    act(() => {
      // Exactly the types `parseRunsByEvent`'s own handler table keys off.
      for (const type of ['agent_start', 'agent_llm_start', 'agent_llm_end', 'pipeline_finish']) {
        registry.emit('execution.node_event', nodeEvent({ type, response_metadata: {} }));
      }
    });

    await waitFor(() => expect(agentEvents.length).toBeGreaterThanOrEqual(4));
    expect(agentEvents.map((frame) => frame.type)).toEqual(
      expect.arrayContaining(['agent_start', 'agent_llm_start', 'agent_llm_end', 'pipeline_finish']),
    );
  });
});


describe('internal tools persistence before chat execution', () => {
  it('awaits selection persistence before posting and never sends an internal-tools override', async () => {
    let release = () => undefined as void;
    const saved = new Promise<void>((resolve) => { release = resolve; });
    const posted = vi.fn();
    server.use(http.post(`${BASE}/elitea_core/messages/prompt_lib/7/${CONVERSATION_UUID}`, async ({ request }) => {
      posted(await request.json());
      return HttpResponse.json({ task_id: 'exec-1', events_url: EVENTS_URL });
    }));
    const { api, Probe } = harness({ getInternalToolsForSend: async () => { await saved; return ['elitea']; } });
    render(withQueryClient(<Probe />));
    const started = api.current?.startStreamedExecution({ conversationUuid: CONVERSATION_UUID, payload: { question: 'save skill', question_id: 'q-1', participant_id: 42 } });
    await act(async () => { await Promise.resolve(); });
    expect(posted).not.toHaveBeenCalled();
    await act(async () => { release(); await started; });
    expect(posted).toHaveBeenCalledOnce();
    expect(posted.mock.calls[0]?.[0]).not.toHaveProperty('internal_tools');
  });

  it('fails explicitly without execution or socket fallback when selection persistence fails', async () => {
    const { api, Probe } = harness({ getInternalToolsForSend: () => Promise.reject(new Error('save failed')) });
    render(withQueryClient(<Probe />));
    await expect(api.current?.startStreamedExecution({ conversationUuid: CONVERSATION_UUID, payload: { question: 'save skill' } })).resolves.toMatchObject({ started: false, reason: 'rejected' });
    expect(registry.getOpen()).toHaveLength(0);
  });

  it('seeds draft tools together with step limit in the conversation creation request', async () => {
    const createConversation = vi.fn().mockResolvedValue({ id: 17, uuid: CONVERSATION_UUID });
    const { api, Probe } = harness({
      getInternalToolsForSend: () => Promise.resolve(['elitea']), llmSettings: { steps_limit: 35 },
      deps: { createConversation, uploadAttachments: () => Promise.resolve({ success: true, uploaded: [] }) },
    });
    render(withQueryClient(<Probe />));
    await act(async () => { await api.current?.createConversationForSend('save a skill'); });
    expect(createConversation).toHaveBeenCalledWith({ name: 'save a skill', isPrivate: true, meta: { steps_limit: 35, internal_tools: ['elitea'] } });
  });
});
