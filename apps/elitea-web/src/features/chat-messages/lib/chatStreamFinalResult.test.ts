import { describe, expect, it } from 'vitest';
import { applyChatStreamFrame } from './chatStreamReducer';
import type { ChatStreamFrame } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory';

const answer = '{"total":30}';
const identity = { message_id: 'answer', question_id: 'question', execution_generation: 'generation-1' };
const context = { name: 'Code pipeline', now: () => '2026-10-05T00:00:00Z' };
const pending: ChatMessage = {
  id: identity.message_id, role: 'assistant', name: context.name, content: '',
  questionId: identity.question_id, executionGeneration: identity.execution_generation,
  createdAt: context.now(), isStreaming: true, isLoading: true, isRegenerating: true,
};
const frame = (type: string, fields: Partial<ChatStreamFrame> = {}): ChatStreamFrame => ({ ...identity, type, ...fields });
const full = frame('full_message', {
  content: answer,
  response_metadata: { thread_id: 'thread', should_continue: false, output_limit_reached: false, invoked_skills: ['fixture'] },
});

describe('durable final result reduction', () => {
  it.each([
    { name: 'missing answer', initial: [] },
    { name: 'existing answer', initial: [pending] },
    { name: 'optimistic answer', initial: [{ ...pending, id: 'optimistic-answer' }] },
  ])('renders one exact answer across the Worker final batch: $name', ({ initial }) => {
    let history: readonly ChatMessage[] = initial;
    const batch = [
      frame('pipeline_finish', { content: answer, response_metadata: { finish_reason: 'finished', should_continue: false, next_step: 'END' } }),
      frame('agent_response', { content: answer, response_metadata: { finish_reason: 'stop', thread_id: 'thread' } }),
      full,
    ];
    for (const event of [...batch, ...batch]) history = applyChatStreamFrame(history, event, context);
    expect(history).toHaveLength(1);
    expect(history[0]).toMatchObject({
      id: 'answer', content: answer, questionId: 'question', executionGeneration: 'generation-1', threadId: 'thread',
      isStreaming: false, isLoading: false, isRegenerating: false,
      responseMetadata: full.response_metadata,
    });
  });

  it('renders a result-only replay without earlier progress or an answer row', () => {
    const history = applyChatStreamFrame([], full, context);
    expect(history).toHaveLength(1);
    expect(history[0]).toMatchObject({ content: answer, isStreaming: false, isLoading: false });
  });

  it('replaces intermediate output and preserves later continuation metadata', () => {
    const history = applyChatStreamFrame([{ ...pending, content: 'Intermediate node output.' }], {
      ...full, response_metadata: { ...full.response_metadata, output_limit_reached: true },
    }, context);
    expect(history[0]?.content).toBe(answer);
    expect(history[0]?.requiresConfirmation).toMatchObject({ buttonText: 'Continue' });
    expect(history[0]?.threadId).toBe('thread');
  });

  it('keeps one saved prefix when a continuation result is replayed', () => {
    const initial = { ...pending, content: 'Saved prefix.', continuedResultPrefix: 'Saved prefix.' };
    const event = { ...full, content: ' More.', response_metadata: { should_continue: true, thread_id: 'thread' } };
    const once = applyChatStreamFrame([initial], event, context);
    const twice = applyChatStreamFrame(once, event, context);
    expect(twice[0]?.content).toBe('Saved prefix. More.');
  });

  it('creates a missing chunked answer and settles only its complete referenced snapshot', () => {
    const digest = 'e248940ebb722339ed0b934ecd2b38f0406ffb49ba06080d2344fca05859f75e';
    const metadata = { offset_bytes: 0, total_bytes: answer.length, sha256: digest, final: true };
    const chunk = frame('agent_result_chunk', { content: answer, response_metadata: { result_chunk_v1: metadata } });
    let history = applyChatStreamFrame([], chunk, context);
    history = applyChatStreamFrame(history, chunk, context);
    const terminal = { ...full, content: null, response_metadata: { thread_id: 'thread', result_ref_v1: { total_bytes: answer.length, sha256: digest } } };
    history = applyChatStreamFrame(history, terminal, context);
    expect(history).toHaveLength(1);
    expect(history[0]).toMatchObject({ content: answer, isStreaming: false, isLoading: false, threadId: 'thread' });
  });

  it('refuses a missing or mismatched referenced result', () => {
    const reference = { total_bytes: answer.length, sha256: 'a'.repeat(64) };
    const terminal = { ...full, content: null, response_metadata: { result_ref_v1: reference } };
    const history = [pending];
    expect(applyChatStreamFrame(history, terminal, context)).toBe(history);
    const assembled = [{ ...pending, assembledResult: answer }];
    expect(applyChatStreamFrame(assembled, terminal, context)).toBe(assembled);
  });

  it('rejects stale generation and child results without changing the parent answer', () => {
    const history = [pending];
    expect(applyChatStreamFrame(history, { ...full, execution_generation: 'old-generation' }, context)).toBe(history);
    expect(applyChatStreamFrame(history, { ...full, response_metadata: { ...full.response_metadata, parent_agent_call_id: 'child' } }, context)).toBe(history);
    expect(applyChatStreamFrame(history, { ...full, content: undefined }, context)).toBe(history);
  });
});
