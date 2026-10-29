/**
 * Per-interrupt decisions through the public decision API (fan-out contract
 * v1.1 §6, §9.3): each card POSTs on its own, a 409 is quiet, other refusals
 * are readable, and nothing waits for a set or touches the continuation route.
 * The route is answered by msw through the real generated client.
 */
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';
import { fanoutFixture } from '@/features/chat-messages/__tests__/fanoutFixtures';
import { applyChatStreamFrame } from '@/features/chat-messages/lib/chatStreamReducer';
import type { NormalizedHitlInterrupt } from '@/features/chat-messages/lib/hitlInterrupts';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import type { ChatBoxHandlerDeps } from './useChatBoxHandlers.helpers';
import { decideHitlCard } from './useChatBoxHandlers.hitl';

const RESPONSE = '10000000-0000-4000-8000-000000000043';
const DECISION = '*/elitea_core/task/prompt_lib/:projectId/:responseMessageId/interrupts/:interruptKey/decision';
const LIST = '*/elitea_core/task/prompt_lib/:projectId/:responseMessageId/interrupts';
const base = fanoutFixture<Record<string, unknown>>('fanout-interrupt-card-v1');

function memberCard(ordinal: number): Record<string, unknown> {
  const callId = `fo1_${String(ordinal).repeat(64)}`;
  return {
    ...base,
    interrupt_id: `ask:${String(ordinal)}`,
    interrupt_key: String(ordinal).repeat(64),
    kind: 'ask_user',
    available_actions: ['answer'],
    display: { guardrail_type: 'clarifying_question', message: `Question ${String(ordinal)}` },
    fanout_v1: { ...(base['fanout_v1'] as object), member: `b${String(ordinal)}`, ordinal },
    parent_agent_call_id: callId,
    parent_agent_path: [{ call_id: callId, name: 'Researcher', sibling_ordinal: ordinal }],
  };
}

function frame(type: string, responseMetadata: Record<string, unknown>): Record<string, unknown> {
  return { type, message_id: RESPONSE, content: null, response_metadata: responseMetadata };
}

/** One browser tab: its own history, built from the same three card frames. */
function tab() {
  let history: readonly ChatMessage[] = [{ id: RESPONSE, role: 'assistant', name: 'Pipeline', content: '', createdAt: '2026-10-19T00:00:00Z', isStreaming: true }];
  for (const ordinal of [1, 2, 3]) {
    history = applyChatStreamFrame(history, frame('agent_interrupt_pending', { pause_progress: true, interrupt_card_v1: memberCard(ordinal) }));
  }
  const continueStreamedExecution = vi.fn();
  const emitSocket = vi.fn(() => true);
  const deps = (): ChatBoxHandlerDeps => ({
    emitSocket,
    chatHistory: history,
    setChatHistory: (update) => { history = typeof update === 'function' ? update(history) : update; },
    setStreamingInfo: () => undefined,
    projectId: 1,
    conversationUuid: 'conv-uuid-1',
    continueStreamedExecution,
  } as ChatBoxHandlerDeps);
  const cards = () => (history[0]?.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[];
  const visible = () => cards().filter((card) => !card.hidden).map((card) => card.interrupt_id);
  const answer = (ordinal: number, value = `answer ${String(ordinal)}`) =>
    decideHitlCard(deps(), { action: 'answer', value, interruptId: `ask:${String(ordinal)}` });
  const receive = (next: Record<string, unknown>) => { history = applyChatStreamFrame(history, next); };
  return { answer, receive, cards, visible, read: () => history, continueStreamedExecution, emitSocket };
}

interface Posted { readonly key: string; readonly body: Record<string, unknown> }

/** The ledger: the first decision per card wins (200), any other is 409. */
function installLedger(refuse?: number): { readonly posted: Posted[]; readonly lists: string[] } {
  const posted: Posted[] = [];
  const lists: string[] = [];
  const decided = new Map<string, string>();
  server.use(
    http.post(DECISION, async ({ params, request }) => {
      const key = String(params['interruptKey']);
      const body = (await request.json()) as Record<string, unknown>;
      posted.push({ key, body });
      if (refuse) return HttpResponse.json({ error: 'agent_interrupt_not_found', message: 'No such request.', retryable: false }, { status: refuse });
      const known = decided.get(key);
      if (known !== undefined && known !== body['request_id']) {
        return HttpResponse.json({ error: 'agent_interrupt_already_resolved', message: 'This request was already answered.', retryable: false }, { status: 409 });
      }
      decided.set(key, String(body['request_id']));
      return HttpResponse.json({ interrupt_key: key, replay: known !== undefined, request_id: body['request_id'], revision: 2, schema: 'elitea.pipeline.fanout-interrupt-decision-result.v1', state: 'DECIDED' });
    }),
    http.get(LIST, ({ params }) => {
      lists.push(String(params['responseMessageId']));
      return HttpResponse.json({ decision_revision: decided.size, interrupts: [], response_message_id: RESPONSE, schema: 'elitea.pipeline.fanout-interrupt-list.v1' });
    }),
  );
  return { posted, lists };
}

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

describe('decideHitlCard for per-interrupt ledger cards', () => {
  it('POSTs one card at a time, in any order, and never touches the continuation route or the socket', async () => {
    const { posted } = installLedger();
    const a = tab();
    await a.answer(3);
    expect(a.visible()).toEqual(['ask:1', 'ask:2']);
    await a.answer(1);
    await a.answer(2);
    expect(a.visible()).toEqual([]);
    expect(posted.map((entry) => entry.key[0])).toEqual(['3', '1', '2']);
    expect(posted[0]?.body).toEqual({ request_id: expect.stringMatching(/^[0-9a-f]{64}$/u) as unknown, expected_revision: 1, action: 'answer', value: 'answer 3' });
    expect(a.continueStreamedExecution).not.toHaveBeenCalled();
    expect(a.emitSocket).not.toHaveBeenCalled();
    expect(a.read()[0]?.isStreaming).toBe(true);
    expect(a.read()[0]?.exception).toBeUndefined();
  });

  it('two tabs answer in reverse order: the other tab drops each card on its resolution, and a lost race is a quiet 409', async () => {
    const { lists } = installLedger();
    const a = tab();
    const b = tab();
    await a.answer(3);
    b.receive(frame('agent_hitl_resolved', { interrupt_resolved_v1: { action: 'answer', interrupt_id: 'ask:3', interrupt_key: '3'.repeat(64), revision: 2, schema: 'elitea.pipeline.fanout-interrupt-resolved.v1', state: 'DECIDED' } }));
    expect(b.visible()).toEqual(['ask:1', 'ask:2']);
    await b.answer(1);
    // Tab A has not seen tab B's resolution yet and answers the same card.
    await a.answer(1, 'a different answer');
    expect(a.cards().map((card) => card.interrupt_id)).not.toContain('ask:1');
    expect(a.read()[0]?.exception).toBeUndefined();
    expect(lists).toEqual([RESPONSE]);
    expect(JSON.stringify(b.read())).not.toContain('answer 3');
  });

  it.each([
    [403, 'You cannot answer this request.'],
    [404, 'This request is no longer waiting for an answer. Reload the conversation to see its current state.'],
    [400, 'This answer could not be accepted. Check it and try again.'],
  ])('puts the card back with a readable reason on %i', async (status, text) => {
    installLedger(status);
    const a = tab();
    await a.answer(2);
    expect(a.visible()).toEqual(['ask:1', 'ask:2', 'ask:3']);
    expect(a.read()[0]?.exception).toBe(text);
  });

  it('retries the same body under the same request id after a lost answer', async () => {
    const ids: unknown[] = [];
    let first = true;
    server.use(http.post(DECISION, async ({ request }) => {
      ids.push(((await request.json()) as Record<string, unknown>)['request_id']);
      if (first) { first = false; return HttpResponse.error(); }
      return HttpResponse.json({ state: 'DECIDED', replay: true });
    }));
    const a = tab();
    await a.answer(2);
    expect(a.read()[0]?.exception).toBe('The decision could not be sent. Try again.');
    await a.answer(2);
    expect(ids).toHaveLength(2);
    expect(ids[0]).toBe(ids[1]);
    expect(a.visible()).toEqual(['ask:1', 'ask:3']);
  });

  it('refuses locally what the route would refuse: an authorize without a credential reference', async () => {
    const { posted } = installLedger();
    const a = tab();
    a.receive(frame('agent_interrupt_pending', { pause_progress: true, interrupt_card_v1: fanoutFixture('fanout-interrupt-card-v1.delegated-auth') }));
    await decideHitlCard({ chatHistory: a.read(), setChatHistory: () => undefined } as unknown as ChatBoxHandlerDeps, { action: 'authorize', interruptId: 'hitl:sensitive:0001' });
    expect(posted).toHaveLength(0);
  });
});
