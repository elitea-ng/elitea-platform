/**
 * FU-249: a fixed Parallel inside a saved child pipeline (root pipeline →
 * Agent node → child pipeline). The frames are the Worker's own projection,
 * captured from the real-PostgreSQL test
 * `postgres_a_wake_lost_inside_a_child_pipeline_resumes_the_answered_branch`:
 * the first run parks three cards; the recovered wake re-raises the two
 * unanswered ones and parks again.
 */
import { describe, expect, it } from 'vitest';

import fixture from '../__tests__/nested-wake-frames.json';

import { applyChatStreamFrame, type ChatStreamContext } from './chatStreamReducer';
import type { ChatMessage } from './convertMessagesToChatHistory';

const MESSAGE_ID = '7946ae2d-7226-5316-a395-3bf246433b69';
const QUESTION_ID = '11111111-2222-3333-4444-555555555555';
const CONTEXT: ChatStreamContext = { name: 'Pipeline', now: () => '2026-10-29T00:00:00.000Z' };

function pendingAssistant(): ChatMessage {
  return {
    id: MESSAGE_ID,
    role: 'assistant',
    name: 'Pipeline',
    content: '',
    createdAt: '2026-10-29T00:00:00.000Z',
    questionId: QUESTION_ID,
    isStreaming: true,
    isLoading: true,
  };
}

function replay(history: readonly ChatMessage[], frames: readonly Record<string, unknown>[]): readonly ChatMessage[] {
  return frames.reduce<readonly ChatMessage[]>(
    (current, raw) => applyChatStreamFrame(current, { ...raw, message_id: MESSAGE_ID, question_id: QUESTION_ID, stream_id: 's-1' } as never, CONTEXT),
    history,
  );
}

describe('a wake of a Parallel parked inside a saved child pipeline', () => {
  it('keeps the re-raised cards of the unanswered branches live', () => {
    const parked = replay([pendingAssistant()], fixture.park);
    expect(parked[0]?.fanoutParked).toBe(true);
    expect(parked[0]?.hitlInterrupts).toHaveLength(3);

    // The wake attach (useChatBoxInterruptLedger): same row, parked state cleared.
    const attached = parked.map((message) => ({ ...message, fanoutParked: false, executionGeneration: undefined, isStreaming: true, isLoading: true }));
    const woken = replay(attached, fixture.wake);
    expect(woken[0]?.fanoutParked).toBe(true);
    expect(woken[0]?.hitlInterrupts).toHaveLength(2);
  });
});
