import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it } from 'vitest';

import { grantsTurnedOff, resetGrantsTurnedOff } from '@/features/unattended';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import type { StreamStartOutcome } from './useChatBoxHandlers.helpers';
import { useChatBoxSend, type UseChatBoxSendResult } from './useChatBoxSend';

const BASE = '/api/v2';
const CONVERSATION = '00000000-0000-4000-8000-0000000000ee';
const PARTICIPANT = { id: 42, entity_name: 'application', entity_settings: { agent_type: 'agent' } };
const GRANT = { grant_id: 'g-1', continue_step_limit: true, continue_output_limit: true };
let registry: TestEventSourceRegistry;
let bodies: Record<string, unknown>[];

beforeEach(() => {
  registry = installTestEventSource();
  Object.assign(globalThis, { elitea_ui_config: { vite_server_url: BASE, vite_base_uri: '/', vite_public_project_id: 'public-1' } });
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: BASE });
  bodies = [];
  // Main with approvals.unattended_grants off: a start with a grant is refused.
  server.use(
    http.post(`${BASE}/elitea_core/messages/prompt_lib/7/${CONVERSATION}`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      bodies.push(body);
      if ('unattended' in body) return HttpResponse.json({ error: 'unattended_grant_refused', reason: 'disabled', message: 'The unattended grant was refused: disabled.' }, { status: 422 });
      return HttpResponse.json({ task_id: 'exec-1', events_url: '/api/v2/executions/7/exec-1/events', response_message_id: 'r-1', execution_generation: 'gen-1' });
    }),
  );
});

afterEach(() => {
  registry.restore();
  Reflect.deleteProperty(globalThis, 'elitea_ui_config');
  resetConfigForTests();
  resetGeneratedClient();
  resetGrantsTurnedOff();
});

function mount(grant: Record<string, unknown>): () => UseChatBoxSendResult | undefined {
  let live: UseChatBoxSendResult | undefined;
  function Probe() {
    live = useChatBoxSend({
      deps: { createConversation: () => Promise.resolve(undefined), uploadAttachments: () => Promise.resolve({ success: true, uploaded: [] }) },
      setChatHistory: () => undefined, conversationUuid: CONVERSATION, projectId: 7, projectIdString: '7',
      activeParticipant: PARTICIPANT, participants: [PARTICIPANT],
      unattended: () => grant,
    });
    return null;
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><Probe /></QueryClientProvider>);
  return () => live;
}

it('shows the refusal of a grant that names sensitive tools, and does not send it again', async () => {
  const live = mount({ ...GRANT, sensitive_tools: [{ toolkit_name: 'github', tool_name: 'merge' }] });
  let outcome: StreamStartOutcome | undefined;
  await act(async () => {
    outcome = await live()?.startStreamedExecution({ conversationUuid: CONVERSATION, payload: { question: 'go', question_id: 'q-1', participant_id: 42 } });
  });
  expect(outcome).toMatchObject({ started: false, reason: 'rejected', grantsOff: true, message: expect.stringContaining('An administrator turned off unattended grants.') as string });
  expect(bodies).toHaveLength(1);
  expect(grantsTurnedOff()).toBe(true);
});

it('sends the message again without the grant once when grants are off, and hides the control (ADR-0045)', async () => {
  let live: UseChatBoxSendResult | undefined;
  function Probe() {
    live = useChatBoxSend({
      deps: { createConversation: () => Promise.resolve(undefined), uploadAttachments: () => Promise.resolve({ success: true, uploaded: [] }) },
      setChatHistory: () => undefined, conversationUuid: CONVERSATION, projectId: 7, projectIdString: '7',
      activeParticipant: PARTICIPANT, participants: [PARTICIPANT],
      unattended: () => GRANT,
    });
    return null;
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><Probe /></QueryClientProvider>);
  let outcome: StreamStartOutcome | undefined;
  await act(async () => {
    outcome = await live?.startStreamedExecution({ conversationUuid: CONVERSATION, payload: { question: 'go', question_id: 'q-1', participant_id: 42 } });
  });
  expect(outcome).toEqual({ started: true });
  expect(bodies).toHaveLength(2);
  expect(bodies[0]?.['unattended']).toEqual(GRANT);
  expect(bodies[1]).not.toHaveProperty('unattended');
  expect(bodies[1]?.['user_input']).toEqual(bodies[0]?.['user_input']);
  expect(grantsTurnedOff()).toBe(true);
});
