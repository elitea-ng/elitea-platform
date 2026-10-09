import { useState } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';
import { PipelineStatus } from '@/features/pipelines/lib/flow-editor/constants/flowEditor.constants';
import type { RunSocketEvent } from '@/features/pipelines/lib/flow-editor/helpers/parseRunsByEvent.support';
import type { UseRunEventResult } from '@/features/pipelines/lib/flow-editor/hooks/useRunEvent';
import { useRunEvent } from '@/features/pipelines/lib/flow-editor/hooks/useRunEvent';
import type { FlowNode } from '@/features/pipelines/lib/flow-editor/reactFlowTypes';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { useChatBoxSend, type UseChatBoxSendResult } from './useChatBoxSend';

const BASE = '/api/v2';
const CONVERSATION = '00000000-0000-4000-8000-0000000000ff';
const RESPONSE = '11111111-1111-4111-8111-111111111111';
const GENERATION = '00000000-0000-4000-8000-000000000001';
const PARTICIPANT = { id: 42, entity_name: 'application', entity_settings: { agent_type: 'pipeline' } };
const YAML = { nodes: [{ id: 'delay' }] };
let registry: TestEventSourceRegistry;
const stopRequest = vi.fn();

beforeEach(() => {
  registry = installTestEventSource();
  Object.assign(globalThis, { elitea_ui_config: { vite_server_url: BASE, vite_base_uri: '/', vite_public_project_id: 'public-1' } });
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: BASE });
  stopRequest.mockClear();
  server.use(
    http.post(`${BASE}/elitea_core/messages/prompt_lib/7/${CONVERSATION}`, () => HttpResponse.json({
      task_id: 'exec-1', events_url: '/api/v2/executions/7/exec-1/events',
      response_message_id: RESPONSE, execution_generation: GENERATION,
    })),
    http.delete(`${BASE}/elitea_core/task/prompt_lib/7/${RESPONSE}`, () => {
      stopRequest();
      return new HttpResponse(null, { status: 204 });
    }),
  );
});

afterEach(() => {
  registry.restore();
  Reflect.deleteProperty(globalThis, 'elitea_ui_config');
  resetConfigForTests();
  resetGeneratedClient();
});

it.each([
  { code: 'CANCELLED', reason: 'Execution was cancelled.', status: PipelineStatus.Stopped },
  { code: 'PIPELINE_CODE_FAILED', reason: 'The Code node failed.', status: PipelineStatus.Error },
])('settles the owned canvas only after canonical $code SSE', async ({ code, reason, status }) => {
  let live: { send: UseChatBoxSendResult; canvas: UseRunEventResult; nodes: readonly FlowNode[]; history: readonly ChatMessage[] };
  function Probe() {
    const [nodes, setNodes] = useState<FlowNode[]>([{ id: 'delay', type: 'code', position: { x: 0, y: 0 }, data: {} }]);
    const [history, setHistory] = useState<readonly ChatMessage[]>([]);
    const canvas = useRunEvent(setNodes, YAML);
    const send = useChatBoxSend({
      deps: { createConversation: () => Promise.resolve(undefined), uploadAttachments: () => Promise.resolve({ success: true, uploaded: [] }) },
      setChatHistory: setHistory, conversationUuid: CONVERSATION, projectId: 7, projectIdString: '7',
      isAgentsPage: true, activeParticipant: PARTICIPANT, participants: [PARTICIPANT],
      onAgentEvent: frame => canvas.onRcvAgentEvent(frame as unknown as RunSocketEvent),
    });
    live = { send, canvas, nodes, history };
    return null;
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><Probe /></QueryClientProvider>);
  await act(async () => { await live.send.startStreamedExecution({ conversationUuid: CONVERSATION, payload: { question: 'run', question_id: 'q-1', participant_id: 42 } }); });
  await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
  const progress = (payload: Record<string, unknown>) => JSON.stringify({ message_id: RESPONSE, execution_generation: GENERATION, response_metadata: {}, ...payload });
  act(() => { registry.emit('execution.node_event', progress({ type: 'agent_start' })); });
  act(() => { registry.emit('execution.node_event', progress({ type: 'agent_llm_start', response_metadata: { metadata: { langgraph_node: 'delay' } } })); });
  expect(live!.canvas.isRunningPipeline).toBe(true);
  expect(live!.nodes[0]?.data.isPerforming).toBe(true);

  for (const identity of [{ message_id: 'another-response', execution_generation: GENERATION }, { message_id: RESPONSE, execution_generation: 'another-generation' }]) {
    act(() => { live.canvas.onRcvAgentEvent({ type: 'execution.failed', code, response_metadata: {}, ...identity }); });
    expect(live!.canvas.isRunningPipeline).toBe(true);
  }
  if (code === 'CANCELLED') {
    act(() => { live.send.stopStreamedExecution(); });
    await waitFor(() => expect(stopRequest).toHaveBeenCalledOnce());
    expect(live!.canvas.isRunningPipeline).toBe(true);
    expect(live!.canvas.pipelineRunNodes[0]?.data.status).toBe(PipelineStatus.InProgress);
  }
  // Main's canonical terminal body carries no response or generation fields.
  act(() => { registry.emit('execution.failed', JSON.stringify({ code, safe_message: reason, retryable: false })); });
  expect(live!.send.isStreaming).toBe(false);
  expect(live!.canvas.isRunningPipeline).toBe(false);
  expect(live!.canvas.pipelineRunNodes).toHaveLength(1);
  expect(live!.canvas.pipelineRunNodes[0]?.data.status).toBe(status);
  expect(live!.canvas.pipelineRunNodes[0]?.data.timeline[0]?.status).toBe(status);
  expect(live!.nodes.every(node => node.data.isPerforming === undefined)).toBe(true);
  expect(live!.history[0]).toMatchObject({ id: RESPONSE, exception: reason, failureCode: code, isStreaming: false, isLoading: false });
  expect(registry.getOpen()).toHaveLength(0);
});
