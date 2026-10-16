/**
 * W6 — every pause card resolves on its own, and the response's pending set
 * resumes as ONE continuation once its last card is decided (today's route
 * consumes the complete set: `decisionsMatchCurrentHITLInterrupts`).
 */
import { renderHook } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';
import { getInterruptIdentity } from '@/features/chat-messages/lib/hitlInterrupts';
import { ROLES } from '@/shared/lib/enums';

import { queueHitlDecision, queuedHitlDecisions } from '../../model/hitlDecisionQueue';
import { useSessionMcpAuthorizationRefs } from './useSessionMcpAuthorizationRefs';
import { useChatBoxHandlers } from './useChatBoxHandlers';
import type { ChatBoxHandlerDeps, StreamStartOutcome } from './useChatBoxHandlers.helpers';
import { restoreQueuedAuthorizations } from './useChatBoxHandlers.authorization';
import { hitlCardIdentity, restoreQueuedHitlDecisions } from './useChatBoxHandlers.hitlCards';

interface Call {
  readonly contract: string;
  readonly body: Record<string, unknown>;
}

const guard = {
  interrupt_id: 'int-1',
  tool_call_id: 'call-1',
  guardrail_type: 'sensitive_tool',
  available_actions: ['approve', 'reject', 'block_with_comment'],
  parent_agent_name: 'Researcher',
  parent_agent_call_id: 'call-research-1',
  parent_agent_path: [{ name: 'Researcher', call_id: 'call-research-1', sibling_ordinal: 1 }],
};
const ask = {
  interrupt_id: 'int-2',
  tool_call_id: 'call-2',
  guardrail_type: 'clarifying_question',
  available_actions: ['answer'],
  parent_agent_name: 'Researcher',
  parent_agent_call_id: 'call-research-2',
  parent_agent_path: [{ name: 'Researcher', call_id: 'call-research-2', sibling_ordinal: 2 }],
};

function paused(): ChatMessage {
  return {
    id: 'answer-1',
    role: ROLES.Assistant,
    name: 'Agent',
    content: '',
    createdAt: '2026-10-16T00:00:00.000Z',
    questionId: 'question-1',
    threadId: 'thread-1',
    isStreaming: false,
    isLoading: false,
    hitlInterrupt: guard,
    hitlInterrupts: [guard, ask],
  };
}

function harness(outcome: StreamStartOutcome = { started: true }, seed: readonly ChatMessage[] = [paused()]) {
  let history: readonly ChatMessage[] = seed;
  const calls: Call[] = [];
  const emitSocket = vi.fn(() => true);
  const setChatHistory: ChatBoxHandlerDeps['setChatHistory'] = (update) => {
    history = typeof update === 'function' ? update(history) : update;
  };
  const useHandlers = (): ReturnType<typeof useChatBoxHandlers> =>
    useChatBoxHandlers({
      emitSocket,
      chatHistory: history,
      setChatHistory,
      setStreamingInfo: () => undefined,
      projectId: 1,
      conversationUuid: 'conv-uuid-1',
      getMcpTokens: () => ({
        'cfg-1:https://login.example.test': { access_token: 'runtime-token-1' },
        'cfg-2:https://login.example.test': { access_token: 'runtime-token-2' },
      }),
      continueStreamedExecution: (params) => {
        calls.push({ contract: params.contract, body: params.body });
        return Promise.resolve(outcome);
      },
    });
  return {
    handlers: useHandlers,
    calls,
    emitSocket,
    read: () => history,
    seed: (next: readonly ChatMessage[]) => {
      history = next;
    },
  };
}

function cards(message: ChatMessage | undefined): readonly Record<string, unknown>[] {
  return (message?.hitlInterrupts ?? []) as readonly Record<string, unknown>[];
}

const SCOPE = { conversation: 'conv-uuid-1', message: 'answer-1', kind: 'hitl' } as const;

beforeEach(() => {
  window.sessionStorage.clear();
});

describe('deciding one card of two', () => {
  it('queues only that card, posts nothing, and leaves the stream state alone', async () => {
    const run = harness();
    await run.handlers().continueHitl({ action: 'approve', toolCallId: 'call-1', interruptId: 'int-1' });

    expect(run.calls).toHaveLength(0);
    expect(run.emitSocket).not.toHaveBeenCalled();
    const message = run.read()[0];
    expect(cards(message)[0]).toMatchObject({ interrupt_id: 'int-1', queued: true, hidden: true });
    expect(cards(message)[1]?.['queued']).toBeUndefined();
    expect(cards(message)[1]?.['hidden']).toBeUndefined();
    expect(message?.hitlInterrupt).toMatchObject({ interrupt_id: 'int-2' });
    expect(message?.isStreaming).toBe(false);
    expect(message?.isLoading).toBe(false);
    expect(queuedHitlDecisions(SCOPE)).toHaveLength(1);
  });

  it('routes by interrupt identity, not by tool_call_id alone', async () => {
    const run = harness();
    run.seed([{ ...paused(), hitlInterrupts: [guard, { ...ask, tool_call_id: 'call-1' }] }]);
    await run.handlers().continueHitl({ action: 'approve', toolCallId: 'call-1', interruptId: 'int-2' });

    expect(cards(run.read()[0]).map((card) => Boolean(card['queued']))).toEqual([false, true]);
  });
});

describe('deciding the last card', () => {
  it('posts exactly one continuation with every decision, in message order', async () => {
    const run = harness();
    await run.handlers().continueHitl({ action: 'answer', value: JSON.stringify({ environment: 'Staging' }), toolCallId: 'call-2', interruptId: 'int-2' });
    await run.handlers().continueHitl({ action: 'block_with_comment', value: 'not this file', toolCallId: 'call-1', interruptId: 'int-1' });

    expect(run.calls).toHaveLength(1);
    expect(run.emitSocket).not.toHaveBeenCalled();
    expect(run.calls[0]?.contract).toBe('agent.continue.hitl.v1');
    expect(run.calls[0]?.body).toEqual({
      project_id: 1,
      conversation_uuid: 'conv-uuid-1',
      message_id: 'answer-1',
      thread_id: 'thread-1',
      hitl_resume: true,
      hitl_decisions: [
        { interrupt_id: 'int-1', tool_call_id: 'call-1', action: 'block_with_comment', value: 'not this file' },
        { interrupt_id: 'int-2', tool_call_id: 'call-2', action: 'answer', value: { environment: 'Staging' } },
      ],
    });
    const message = run.read()[0];
    expect(cards(message).every((card) => card['decided'] === true && card['hidden'] === true && card['queued'] === false)).toBe(true);
    expect(message?.hitlInterrupt).toBeUndefined();
    expect(message?.isStreaming).toBe(true);
    expect(queuedHitlDecisions(SCOPE)).toEqual([]);
  });

  it('keeps the single-card body for a set of one', async () => {
    const run = harness();
    run.seed([{ ...paused(), hitlInterrupts: [guard] }]);
    await run.handlers().continueHitl({ action: 'approve', toolCallId: 'call-1', interruptId: 'int-1' });

    expect(run.calls[0]?.body['hitl_decisions']).toEqual([{ interrupt_id: 'int-1', tool_call_id: 'call-1', action: 'approve', value: '' }]);
  });
});

describe('the route’s answer', () => {
  it('a 409 already-resolved keeps the cards answered, quietly, with no socket emit', async () => {
    const run = harness({ started: false, reason: 'already-resolved', code: 'agent_hitl_already_resolved' });
    await run.handlers().continueHitl({ action: 'approve', toolCallId: 'call-1', interruptId: 'int-1' });
    await run.handlers().continueHitl({ action: 'answer', value: '"Staging"', toolCallId: 'call-2', interruptId: 'int-2' });

    expect(run.calls).toHaveLength(1);
    expect(run.emitSocket).not.toHaveBeenCalled();
    const message = run.read()[0];
    expect(message?.exception).toBeUndefined();
    expect(message?.isStreaming).toBe(false);
    expect(message?.isLoading).toBe(false);
    expect(cards(message).every((card) => card['decided'] === true && card['hidden'] === true)).toBe(true);
    expect(queuedHitlDecisions(SCOPE)).toEqual([]);
  });

  it('any other refusal puts every card of the set back with the route’s reason', async () => {
    const run = harness({ started: false, reason: 'rejected', message: 'The approval set no longer matches.' });
    await run.handlers().continueHitl({ action: 'approve', toolCallId: 'call-1', interruptId: 'int-1' });
    await run.handlers().continueHitl({ action: 'answer', value: '"Staging"', toolCallId: 'call-2', interruptId: 'int-2' });

    expect(run.emitSocket).not.toHaveBeenCalled();
    const message = run.read()[0];
    expect(message?.exception).toBe('The approval set no longer matches.');
    expect(message?.isStreaming).toBe(false);
    expect(cards(message)).toEqual([guard, ask]);
    expect(message?.hitlInterrupt).toEqual(guard);
    expect(queuedHitlDecisions(SCOPE)).toEqual([]);
  });
});

describe('a reload between two decisions', () => {
  it('restores the decided card queued and submits both once the other is decided', async () => {
    const before = harness();
    await before.handlers().continueHitl({ action: 'block_with_comment', value: 'keep the report', toolCallId: 'call-1', interruptId: 'int-1' });

    // Reload: the server hands back the pause with no local flags.
    const hydrated = restoreQueuedHitlDecisions([paused()], 'conv-uuid-1');
    expect(cards(hydrated[0])[0]).toMatchObject({ interrupt_id: 'int-1', queued: true, hidden: true });
    expect(cards(hydrated[0])[1]?.['queued']).toBeUndefined();

    const after = harness();
    after.seed(hydrated);
    await after.handlers().continueHitl({ action: 'answer', value: '"Staging"', toolCallId: 'call-2', interruptId: 'int-2' });

    expect(after.calls).toHaveLength(1);
    expect(after.calls[0]?.body['hitl_decisions']).toEqual([
      { interrupt_id: 'int-1', tool_call_id: 'call-1', action: 'block_with_comment', value: 'keep the report' },
      { interrupt_id: 'int-2', tool_call_id: 'call-2', action: 'answer', value: 'Staging' },
    ]);
  });

  it('drops a persisted decision whose card is no longer pending', async () => {
    const before = harness();
    await before.handlers().continueHitl({ action: 'approve', toolCallId: 'call-1', interruptId: 'int-1' });

    const third = { ...ask, interrupt_id: 'int-3', tool_call_id: 'call-3' };
    const hydrated = restoreQueuedHitlDecisions([{ ...paused(), hitlInterrupts: [ask, third] }], 'conv-uuid-1');

    expect(cards(hydrated[0]).some((card) => card['queued'])).toBe(false);
    expect(queuedHitlDecisions(SCOPE)).toEqual([]);
  });

  it('ignores corrupt storage', () => {
    window.sessionStorage.setItem('el.chat.hitlDecisions', '{"version":1,"responses":[{"bogus":true}]}');
    const history = [paused()];
    expect(restoreQueuedHitlDecisions(history, 'conv-uuid-1')).toBe(history);
  });
});

describe('hitlCardIdentity', () => {
  it('answers exactly what getInterruptIdentity answers', () => {
    const samples = [
      { interrupt_id: 'i-1', tool_call_id: 'c-1' },
      { thread_id: 't', tool_call_id: 'c' },
      { child_thread_id: 'ct', thread_id: 't', tool_call_id: 'c' },
      { message: 'nothing identifies me' },
      { thread_id: '   ' },
    ];
    for (const sample of samples) expect(hitlCardIdentity(sample)).toBe(getInterruptIdentity(sample));
  });
});

function authorizationCard(interruptId: string, toolCallId: string, storageKey: string) {
  return {
    id: interruptId,
    authorizationRequestId: interruptId,
    name: 'SharePoint search',
    status: 'action_required',
    type: 'toolkit',
    toolOutputs: { server_url: storageKey },
    toolMeta: { interrupt_id: interruptId, tool_call_id: toolCallId, server_url: 'https://sharepoint.example.test' },
  };
}

function authorizationPause(): ChatMessage {
  return {
    ...paused(),
    hitlInterrupt: undefined,
    hitlInterrupts: undefined,
    toolActions: [
      authorizationCard('auth-1', 'call-1', 'cfg-1:https://login.example.test'),
      authorizationCard('auth-2', 'call-2', 'cfg-2:https://login.example.test'),
    ],
  };
}

describe('delegated authorization cards', () => {
  it('a 409 already-resolved settles quietly and puts no card back', async () => {
    const run = harness({ started: false, reason: 'already-resolved', code: 'agent_authorization_already_resolved' }, [authorizationPause()]);
    await run.handlers().resumeMcpFlow('answer-1', false, 'auth-1');
    await run.handlers().resumeMcpFlow('answer-1', true, 'auth-2');

    expect(run.calls).toHaveLength(1);
    expect(run.emitSocket).not.toHaveBeenCalled();
    const message = run.read()[0];
    expect(message?.exception).toBeUndefined();
    expect(message?.isStreaming).toBe(false);
    expect(message?.toolActions).toEqual([]);
  });

  it('survives a reload between two decisions, persisting no token', async () => {
    const before = harness({ started: true }, [authorizationPause()]);
    await before.handlers().resumeMcpFlow('answer-1', false, 'auth-1');
    expect(before.calls).toHaveLength(0);
    expect(window.sessionStorage.getItem('el.chat.hitlDecisions')).not.toContain('runtime-token');

    const hydrated = restoreQueuedAuthorizations([authorizationPause()], 'conv-uuid-1');
    expect((hydrated[0]?.toolActions ?? []).map((action) => (action as unknown as { id: string }).id)).toEqual(['auth-2']);

    const after = harness({ started: true }, hydrated);
    await after.handlers().resumeMcpFlow('answer-1', true, 'auth-2');

    expect(after.calls).toHaveLength(1);
    expect(after.calls[0]?.body).toMatchObject({
      hitl_resume: true,
      // Only the authorized card's credential; the skipped one sends none.
      mcp_tokens: { 'cfg-1:https://login.example.test': { access_token: 'runtime-token-1' } },
      hitl_decisions: [
        { interrupt_id: 'auth-1', tool_call_id: 'call-1', guardrail_type: 'mcp_auth', action: 'authorize' },
        { interrupt_id: 'auth-2', tool_call_id: 'call-2', guardrail_type: 'mcp_auth', action: 'skip' },
      ],
    });
  });
});

describe('review findings', () => {
  const node = (id: string) => ({ interrupt_id: id, available_actions: ['approve', 'reject'], node_name: 'review' });

  it('decides cards that carry no tool_call_id (the card sends an empty one)', async () => {
    const run = harness({ started: true }, [{ ...paused(), hitlInterrupt: node('n-1'), hitlInterrupts: [node('n-1'), node('n-2')] }]);
    await run.handlers().continueHitl({ action: 'approve', toolCallId: '', interruptId: 'n-1' });
    expect(run.read()[0]?.exception).toBeUndefined();
    expect(cards(run.read()[0])[0]).toMatchObject({ queued: true });
    await run.handlers().continueHitl({ action: 'reject', toolCallId: '', interruptId: 'n-2' });

    expect(run.calls).toHaveLength(1);
    expect(run.calls[0]?.body['hitl_decisions']).toEqual([
      { interrupt_id: 'n-1', tool_call_id: '', action: 'approve', value: '' },
      { interrupt_id: 'n-2', tool_call_id: '', action: 'reject', value: '' },
    ]);
  });

  it('a legacy fan-out child submit leaves the root set’s queued decisions alone', async () => {
    const child = { interrupt_id: 'int-child', tool_call_id: 'call-c', child_thread_id: 't-child', resume_strategy: 'aggregate_child' };
    const run = harness({ started: true }, [{ ...paused(), hitlInterrupts: [guard, ask, child] }]);
    await run.handlers().continueHitl({ action: 'approve', toolCallId: 'call-1', interruptId: 'int-1' });
    await run.handlers().continueHitl({ action: 'approve', toolCallId: 'call-c', interruptId: 'int-child' });

    expect(run.calls).toHaveLength(1);
    expect(run.calls[0]?.body['thread_id']).toBe('t-child');
    expect(queuedHitlDecisions(SCOPE).map((entry) => entry.interruptId)).toEqual(['int-1']);
    expect(cards(run.read()[0])[0]).toMatchObject({ interrupt_id: 'int-1', queued: true });
  });

  it('a multi-card set the route cannot express is refused readably, not sent over the socket', async () => {
    const run = harness({ started: true }, [{ ...paused(), hitlInterrupts: [guard, { ...ask, interrupt_id: '' }] }]);
    await run.handlers().continueHitl({ action: 'approve', toolCallId: 'call-1', interruptId: 'int-1' });
    await run.handlers().continueHitl({ action: 'answer', value: '"x"', toolCallId: 'call-2' });

    expect(run.calls).toHaveLength(0);
    expect(run.emitSocket).not.toHaveBeenCalled();
    const message = run.read()[0];
    expect(message?.exception).toMatch(/cannot be sent/);
    expect(message?.isStreaming).toBe(false);
    expect(cards(message).some((card) => card['decided'] || card['queued'] || card['hidden'])).toBe(false);
  });

  it('an authorization decision the queue refuses says so', async () => {
    const longId = 'a'.repeat(600);
    const pause = authorizationPause();
    const run = harness({ started: true }, [{ ...pause, toolActions: [authorizationCard(longId, 'call-1', 'cfg-1:https://login.example.test'), ...(pause.toolActions ?? []).slice(1)] }]);
    await run.handlers().resumeMcpFlow('answer-1', false, longId);

    expect(run.calls).toHaveLength(0);
    expect(run.read()[0]?.exception).toMatch(/could not be recorded/);
  });
});

describe('page-life memory follows the conversation', () => {
  it('is dropped when the conversation changes, not on mount', () => {
    queueHitlDecision(SCOPE, { interruptId: 'int-1', action: 'edit', value: 'x'.repeat(9000) });
    const { rerender } = renderHook(({ uuid }: { uuid: string }) => useSessionMcpAuthorizationRefs(uuid), { initialProps: { uuid: 'conv-uuid-1' } });
    expect(queuedHitlDecisions(SCOPE)).toHaveLength(1);
    rerender({ uuid: 'conv-uuid-2' });
    expect(queuedHitlDecisions(SCOPE)).toEqual([]);
  });
});
