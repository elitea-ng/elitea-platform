/**
 * FU-249: a fixed Parallel inside a saved child pipeline (root pipeline →
 * Agent node → child pipeline). The frames are the Worker's own projection,
 * captured from the real-PostgreSQL test
 * `postgres_a_wake_lost_inside_a_child_pipeline_resumes_the_answered_branch`:
 * the first run parks three cards; the recovered wake re-raises the two
 * unanswered ones and parks again.
 */
import { describe, expect, it } from 'vitest';

import failedFixture from '../__tests__/nested-wake-failed-frames.json';
import fixture from '../__tests__/nested-wake-frames.json';

import { applyChatStreamFrame, type ChatStreamContext } from './chatStreamReducer';
import type { ToolAction } from './chatStreamToolAction';
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

/**
 * The browser run that first reported the open card as lost (fanpar,
 * 2026-10-29): `bell` answered, its resumed model call failed upstream
 * (On Error `continue`), `lights` re-parked. These are that run's frames as
 * Main replays them (`execution_replay_events`): the park, then the wake.
 */
describe('a wake whose answered branch fails', () => {
  it('keeps the other branch\'s open card live and marks the failed member', () => {
    const parked = replay([pendingAssistant()], failedFixture.park);
    expect(parked[0]?.fanoutParked).toBe(true);
    const open = parked[0]?.hitlInterrupts ?? [];
    expect(open).toHaveLength(1);

    const attached = parked.map((message) => ({ ...message, fanoutParked: false, executionGeneration: undefined, isStreaming: true, isLoading: true }));
    // The last frame is the user's later answer to the open card.
    const wake = failedFixture.wake.slice(0, -1);
    expect(failedFixture.wake[failedFixture.wake.length - 1]?.type).toBe('agent_hitl_resolved');
    const woken = replay(attached, wake);
    expect(woken[0]?.fanoutParked).toBe(true);
    expect(woken[0]?.exception).toBeUndefined();
    expect(woken[0]?.hitlInterrupts).toEqual(open);
    const actions = (woken[0]?.toolActions ?? []) as readonly ToolAction[];
    const failed = actions.filter((action) => action.id.startsWith('fanout-member:') && action.status === 'error');
    expect(failed).toHaveLength(1);
  });
});
