/**
 * A message for people alone (mentions, no addressed agent) is stored and
 * notifies them; Main starts no turn for it. The send path must therefore open
 * no stream, name no participant and carry no model settings: the sticky or
 * last-addressed agent and the conversation's default model are not addressees
 * (the model that answered "can you see this?" on a colleague's behalf was the
 * defect). A message that addresses an agent still goes to it.
 */
import { useState } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { useChatBoxSend, type UseChatBoxSendResult } from './useChatBoxSend';
import { isPeopleOnlyPayload, resolvedTarget } from './useChatBoxSend.floor';

const BASE = '/api/v2';
const CONVERSATION = '00000000-0000-4000-8000-000000001970';
const QUESTION = '00000000-0000-4000-8000-0000000019a0';
const AGENT = { id: 901, entity_name: 'application', entity_meta: { id: 143 } };
const OTHER_AGENT = { id: 902, entity_name: 'application', entity_meta: { id: 144 } };
const POSTED = { message_id: QUESTION, created: true, turn_started: false };
const ACCEPTED = { task_id: 'exec-1970', execution_id: 'exec-1970', events_url: '/api/v2/executions/7/exec-1970/events', response_message_id: '00000000-0000-4000-8000-0000000019b0' };

let registry: TestEventSourceRegistry;
let requests: { readonly contract: string | null; readonly body: Record<string, unknown> }[];
let live: { send: UseChatBoxSendResult | undefined; history: readonly ChatMessage[] };
let answer: unknown;

beforeEach(() => {
  registry = installTestEventSource();
  requests = [];
  answer = POSTED;
  live = { send: undefined, history: [] };
  Object.assign(globalThis, { elitea_ui_config: { vite_server_url: BASE, vite_base_uri: '/', vite_public_project_id: 'public-1' } });
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.post(`${BASE}/elitea_core/messages/prompt_lib/7/${CONVERSATION}`, async ({ request }) => {
      requests.push({ contract: new URL(request.url).searchParams.get('execution_contract'), body: await request.json() as Record<string, unknown> });
      return HttpResponse.json(answer as Record<string, unknown>, { status: answer === POSTED ? 201 : 200 });
    }),
  );
});

afterEach(() => {
  registry.restore();
  Reflect.deleteProperty(globalThis, 'elitea_ui_config');
  resetConfigForTests();
  resetGeneratedClient();
});

function mount(target: unknown): void {
  function Probe(): null {
    const [history, setHistory] = useState<readonly ChatMessage[]>([]);
    live.history = history;
    live.send = useChatBoxSend({
      deps: { createConversation: () => Promise.resolve(undefined), uploadAttachments: () => Promise.resolve({ success: true, uploaded: [] }) },
      setChatHistory: (update) => setHistory(update),
      conversationUuid: CONVERSATION, projectId: 7, projectIdString: '7',
      model: { name: 'gpt-default', projectId: '7' },
      target: resolvedTarget(target), participants: [AGENT, OTHER_AGENT],
    });
    return null;
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><Probe /></QueryClientProvider>);
}

const send = async (payload: Record<string, unknown>) => {
  let outcome: Awaited<ReturnType<UseChatBoxSendResult['startStreamedExecution']>> | undefined;
  await act(async () => {
    outcome = await live.send?.startStreamedExecution({ conversationUuid: CONVERSATION, payload: { question: '@Aksana can you see this?', question_id: QUESTION, ...payload } });
  });
  return outcome;
};

describe('a message for people alone', () => {
  it('posts the message with the mentions and opens no stream, whatever the sticky target is', async () => {
    mount(AGENT);
    const outcome = await send({ isSendingToUser: true, userIds: ['11'] });

    expect(outcome).toEqual({ started: false, reason: 'posted' });
    expect(requests).toHaveLength(1);
    const { body } = requests[0] ?? { body: {} };
    expect(body).toMatchObject({ user_ids: [11], question_id: QUESTION, payload: { user_input: '@Aksana can you see this?' } });
    // Not the sticky agent, not the default model, and no participant_id-0 fallback.
    expect(body).not.toHaveProperty('participant_id');
    expect(body).not.toHaveProperty('llm_settings');
    expect(body).not.toHaveProperty('addressed_to');
    expect(registry.getOpen()).toHaveLength(0);
  });

  it('does the same with no target at all (the default model would have answered)', async () => {
    mount(undefined);
    const outcome = await send({ isMentioningEveryone: true });

    expect(outcome).toEqual({ started: false, reason: 'posted' });
    expect(requests[0]?.body).toMatchObject({ is_mentioning_everyone: true });
    expect(requests[0]?.body).not.toHaveProperty('participant_id');
    expect(requests[0]?.body).not.toHaveProperty('llm_settings');
    expect(registry.getOpen()).toHaveLength(0);
  });
});

describe('a message that addresses an agent', () => {
  it('still goes to that agent, with its people in the body, and streams', async () => {
    answer = ACCEPTED;
    mount(OTHER_AGENT);
    const outcome = await send({ userIds: ['11'], addressedTo: [901] });

    expect(outcome).toEqual({ started: true });
    expect(requests[0]?.contract).toBe('agent.execute.application.v1');
    expect(requests[0]?.body).toMatchObject({ participant_id: 901, addressed_to: [901], user_ids: [11] });
    expect(registry.getOpen()).toHaveLength(1);
  });

  it('a message with no mention is unchanged: the target answers and streams', async () => {
    answer = ACCEPTED;
    mount(AGENT);
    const outcome = await send({});

    expect(outcome).toEqual({ started: true });
    expect(requests[0]?.body).toMatchObject({ participant_id: 901 });
    expect(requests[0]?.body).not.toHaveProperty('user_ids');
    expect(registry.getOpen()).toHaveLength(1);
  });
});

describe('isPeopleOnlyPayload', () => {
  it('reads the same fields Main decides on', () => {
    expect(isPeopleOnlyPayload({ userIds: ['11'] })).toBe(true);
    expect(isPeopleOnlyPayload({ userIds: [11], addressedTo: [] })).toBe(true);
    expect(isPeopleOnlyPayload({ isMentioningEveryone: true })).toBe(true);
    expect(isPeopleOnlyPayload({ userIds: ['11'], addressedTo: [901] })).toBe(false);
    expect(isPeopleOnlyPayload({ userIds: ['not-an-id', 0] })).toBe(false);
    expect(isPeopleOnlyPayload({ question: 'hi' })).toBe(false);
  });
});
