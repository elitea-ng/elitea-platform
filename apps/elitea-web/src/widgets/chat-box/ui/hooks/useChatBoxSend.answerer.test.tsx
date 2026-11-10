/**
 * Who answers, as the stream and the regenerate route see it (ADR-0050 D7c,
 * issue 1625). The composer's target is B; the turn in flight may still go to
 * another agent:
 *
 * - a message that addresses A goes to A, so A's streamed answer is
 *   attributed to A, not to the target;
 * - regenerating an answer to a question that went to A asks A again (Main
 *   refuses a regeneration naming another participant).
 *
 * Driven through the real transport; only the network is substituted.
 */
import { useState } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { useChatBoxSend, type UseChatBoxSendResult } from './useChatBoxSend';

const BASE = '/api/v2';
const CONVERSATION = '00000000-0000-4000-8000-000000001625';
const RESPONSE = '00000000-0000-4000-8000-0000000016a0';
const GENERATION = '00000000-0000-4000-8000-0000000016b0';
const AGENT_A = { id: 901, entity_name: 'application', entity_meta: { id: 143 } };
const AGENT_B = { id: 902, entity_name: 'application', entity_meta: { id: 144 } };
const ACCEPTED = { task_id: 'exec-1625', events_url: '/api/v2/executions/7/exec-1625/events', response_message_id: RESPONSE, execution_generation: GENERATION };

let registry: TestEventSourceRegistry;
let bodies: Record<string, unknown>[];
let live: { send: UseChatBoxSendResult | undefined; history: readonly ChatMessage[] };

beforeEach(() => {
  registry = installTestEventSource();
  bodies = [];
  live = { send: undefined, history: [] };
  Object.assign(globalThis, { elitea_ui_config: { vite_server_url: BASE, vite_base_uri: '/', vite_public_project_id: 'public-1' } });
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.post(`${BASE}/elitea_core/messages/prompt_lib/7/${CONVERSATION}`, async ({ request }) => {
      bodies.push(await request.json() as Record<string, unknown>);
      return HttpResponse.json(ACCEPTED);
    }),
    http.post(`${BASE}/elitea_core/regenerate/prompt_lib/7/${RESPONSE}`, async ({ request }) => {
      bodies.push(await request.json() as Record<string, unknown>);
      return HttpResponse.json(ACCEPTED);
    }),
  );
});

afterEach(() => {
  registry.restore();
  Reflect.deleteProperty(globalThis, 'elitea_ui_config');
  resetConfigForTests();
  resetGeneratedClient();
});

function mount(): void {
  function Probe(): null {
    const [history, setHistory] = useState<readonly ChatMessage[]>([]);
    live.history = history;
    live.send = useChatBoxSend({
      deps: { createConversation: () => Promise.resolve(undefined), uploadAttachments: () => Promise.resolve({ success: true, uploaded: [] }) },
      setChatHistory: (update) => setHistory(update),
      conversationUuid: CONVERSATION, projectId: 7, projectIdString: '7',
      target: AGENT_B, participants: [AGENT_A, AGENT_B],
    });
    return null;
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><Probe /></QueryClientProvider>);
}

/** A frame that names no participant: the reducer attributes it from the stream context. */
function agentStart(): void {
  act(() => {
    registry.emit('execution.node_event', JSON.stringify({ type: 'agent_start', stream_id: CONVERSATION, message_id: RESPONSE,
      execution_generation: GENERATION, response_metadata: {}, created_at: '2026-11-09T10:10:00Z' }), '1');
  });
}

it('attributes the answer to the agent a message addressed, not to the composer target', async () => {
  mount();
  await act(async () => {
    await live.send?.startStreamedExecution({ conversationUuid: CONVERSATION, payload: { question: '@A go', question_id: 'q-1', addressedTo: [901] } });
  });
  expect(bodies[0]).toMatchObject({ participant_id: 901 });
  await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
  agentStart();
  await waitFor(() => expect(live.history.find((message) => message.id === RESPONSE)?.participantId).toBe('901'));
});

it('regenerates with the agent the question went to, and attributes the new answer to it', async () => {
  mount();
  await act(async () => {
    await live.send?.regenerateStreamedExecution({ messageId: RESPONSE, questionId: 'q-1', question: 'hi A', participantId: '901' });
  });
  expect(bodies[0]).toMatchObject({ participant_id: 901 });
  await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
  agentStart();
  await waitFor(() => expect(live.history.find((message) => message.id === RESPONSE)?.participantId).toBe('901'));
});

it('regenerates with the composer target when the question names no participant', async () => {
  mount();
  await act(async () => {
    await live.send?.regenerateStreamedExecution({ messageId: RESPONSE, questionId: 'q-1', question: 'hi' });
  });
  expect(bodies[0]).toMatchObject({ participant_id: 902 });
});
