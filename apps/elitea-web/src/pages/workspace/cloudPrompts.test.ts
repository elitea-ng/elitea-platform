import { describe, expect, it } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';

import { cloudPrompt, continueBody, hitlBody, markDecided } from './cloudPrompts';

const labels = { approve: (tool: string) => `Approve: ${tool}`, continuePrompt: 'Continue?', chat: 'Answer in chat' };
const message = (over: Partial<ChatMessage> = {}): ChatMessage => ({ id: 'm1', role: 'assistant', name: 'Agent', content: '', createdAt: '2026-10-20T00:00:00Z', ...over });
const identity = (m: ChatMessage) => ({ projectId: 42, conversationUuid: 'u1', message: m });

describe('cloudPrompt', () => {
  it('is null for a finished or streaming turn, or a question of the user', () => {
    expect(cloudPrompt([], labels)).toBeNull();
    expect(cloudPrompt([message()], labels)).toBeNull();
    expect(cloudPrompt([message({ isStreaming: true, hitlInterrupt: { tool_name: 'x' } })], labels)).toBeNull();
    expect(cloudPrompt([message({ role: 'user' })], labels)).toBeNull();
  });

  it('names a single pending approval, and the token-limit continue', () => {
    expect(cloudPrompt([message({ hitlInterrupt: { tool_name: 'delete_file' } })], labels)).toMatchObject({ kind: 'approval', label: 'Approve: delete_file' });
    expect(cloudPrompt([message({ requiresConfirmation: { message: 'm', buttonText: 'Continue' } })], labels)).toMatchObject({ kind: 'continue', label: 'Continue?' });
    // A decided card is no longer owed.
    expect(cloudPrompt([message({ hitlInterrupts: [{ tool_name: 'x', decided: true }] })], labels)).toBeNull();
  });

  it('sends what only the chat can answer there: several cards, a static pause, node recovery', () => {
    expect(cloudPrompt([message({ hitlInterrupts: [{ tool_name: 'a' }, { tool_name: 'b' }] })], labels)).toMatchObject({ kind: 'chat', label: 'Answer in chat' });
    expect(cloudPrompt([message({ staticPause: {} as never })], labels)?.kind).toBe('chat');
    expect(cloudPrompt([message({ nodeRecoveryRequired: {} as never })], labels)?.kind).toBe('chat');
  });
});

describe('the answer bodies', () => {
  const m = message({ threadId: 'thread-1' });

  it('answers a root approval with the action (and the value only for the valued actions)', () => {
    expect(hitlBody(identity(m), { action: 'approve' }, undefined)).toEqual({ project_id: 42, conversation_uuid: 'u1', message_id: 'm1', thread_id: 'thread-1', hitl_resume: true, hitl_action: 'approve' });
    expect(hitlBody(identity(m), { action: 'edit', value: 'new text' }, undefined)).toMatchObject({ hitl_action: 'edit', hitl_value: 'new text' });
    expect(hitlBody(identity(m), { action: 'answer', value: '{"env":"staging"}' }, undefined)).toMatchObject({ hitl_value: { env: 'staging' } });
  });

  it('answers a tool card as a decision routed by its interrupt id, never without one', () => {
    expect(hitlBody(identity(m), { action: 'reject', toolCallId: 'c1', interruptId: 'i1' }, undefined)).toMatchObject({
      hitl_decisions: [{ interrupt_id: 'i1', tool_call_id: 'c1', action: 'reject', value: '' }],
    });
    expect(hitlBody(identity(m), { action: 'approve', toolCallId: 'c1' }, undefined)).toBeUndefined();
  });

  it('has no body for an invalid project or an unknown action, and never carries mcp tokens', () => {
    expect(hitlBody({ projectId: undefined, conversationUuid: 'u1', message: m }, { action: 'approve' }, undefined)).toBeUndefined();
    expect(hitlBody(identity(m), { action: 'nope' as never }, undefined)).toBeUndefined();
    expect(JSON.stringify(hitlBody(identity(m), { action: 'approve' }, undefined))).not.toContain('mcp_tokens');
  });

  it('continues a token-limit turn with just the identity', () => {
    expect(continueBody(identity(m))).toEqual({ project_id: 42, conversation_uuid: 'u1', message_id: 'm1' });
  });

  it('marks the cards decided (streaming) and restores them', () => {
    const paused = message({ hitlInterrupts: [{ tool_name: 'x', interrupt_id: 'i1' }], hitlInterrupt: { tool_name: 'x', interrupt_id: 'i1' } });
    const decided = markDecided(paused, true);
    expect(decided).toMatchObject({ isStreaming: true, hitlInterrupt: undefined, hitlInterrupts: [{ decided: true }] });
    expect(markDecided(decided, false)).toMatchObject({ isStreaming: false, hitlInterrupt: { decided: false } });
  });
});
