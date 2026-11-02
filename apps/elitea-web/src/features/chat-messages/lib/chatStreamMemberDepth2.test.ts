/**
 * FU-264: a fixed Parallel whose members run a saved pipeline ("FP child
 * direct") that runs its own Agent node ("FP direct reverse", an effectful MCP
 * call). The frames are Main's replay of a real fanpar run
 * (`execution_replay_events`, chat 47): the first turn parks the two members'
 * cards; the wake runs the first member's approved call. The trace rows are
 * the steps Main stored for that response (message group 298), as a reload
 * reads them.
 */
import { describe, expect, it } from 'vitest';

import { groupTraceStepsByMessageGroup } from '@/entities/message/lib/traceSteps';
import type { TraceStepRow } from '@/entities/message/lib/traceSteps';
import type { MessageGroupWire } from '@/entities/message/lib/wire';

import fixture from '../__tests__/member-depth2-frames.json';
import rows from '../__tests__/member-depth2-trace-rows.json';
import { applyChatStreamFrame, type ChatStreamContext } from './chatStreamReducer';
import type { ChatMessage } from './convertMessagesToChatHistory';
import { convertMessagesToChatHistory } from './convertMessagesToChatHistory';
import { getSubAgentInstanceKey, getSubAgentName } from './executionHierarchy';
import { partitionActionsIntoBlocks } from './subAgentGrouping';
import type { SubAgentGroupable } from './subAgentGrouping';

const MESSAGE_ID = 'f25617a6-209f-5355-a5e7-0f68a14a0aa6';
const QUESTION_ID = 'e11db505-aaee-479b-a017-05827350cdbb';
const CONTEXT: ChatStreamContext = { name: 'FP parallel deep', now: () => '2026-11-02T00:00:00.000Z' };
const MEMBER = 'FP child direct';
const INNER = 'FP direct reverse';

function pendingAssistant(): ChatMessage {
  return {
    id: MESSAGE_ID,
    role: 'assistant',
    name: 'FP parallel deep',
    content: '',
    createdAt: '2026-11-02T00:00:00.000Z',
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

/** Each sub-agent tier's name with the names of the calls it holds. */
function tiers(actions: readonly SubAgentGroupable[]): { name: string; calls: string[] }[] {
  return partitionActionsIntoBlocks(actions, {
    deriveName: (action) => getSubAgentName(action as never),
    deriveInstanceKey: (action) => getSubAgentInstanceKey(action as never),
    classifyWrapper: () => null,
  }).flatMap((block) =>
    block.kind === 'sub'
      ? [{ name: block.name, calls: block.actions.map((action) => callName(action)).filter((name) => name !== '') }]
      : [],
  );
}

function callName(action: SubAgentGroupable): string {
  const record = action as { toolMeta?: { name?: unknown }; name?: unknown; original_name?: unknown };
  const name = record.toolMeta?.name ?? record.original_name ?? record.name;
  return typeof name === 'string' ? name : '';
}

describe('a member whose saved pipeline runs its own Agent node', () => {
  it('shows the inner call in the member\'s tier while it streams', () => {
    // The park raises one card per member; its last frame is the user's later
    // decision on the first one.
    expect(fixture.park[fixture.park.length - 1]?.type).toBe('agent_hitl_resolved');
    const raised = replay([pendingAssistant()], fixture.park.slice(0, -1));
    expect(raised[0]?.fanoutParked).toBe(true);
    expect(raised[0]?.hitlInterrupts).toHaveLength(2);
    const parked = replay([pendingAssistant()], fixture.park);
    expect(parked[0]?.hitlInterrupts).toHaveLength(1);

    // The wake attach (useChatBoxInterruptLedger): same row, parked state cleared.
    const attached = parked.map((message) => ({ ...message, fanoutParked: false, executionGeneration: undefined, isStreaming: true, isLoading: true }));
    const woken = replay(attached, fixture.wake);
    expect(woken[0]?.exception).toBeUndefined();
    const memberTiers = tiers((woken[0]?.toolActions ?? []) as SubAgentGroupable[]).filter((tier) => tier.name === MEMBER);
    expect(memberTiers.some((tier) => tier.calls.includes(INNER))).toBe(true);
  });

  it('shows the same tier after a reload, from the stored steps', () => {
    const traceRows = rows as unknown as TraceStepRow[];
    const listing = groupTraceStepsByMessageGroup(traceRows.filter((row) => row.kind === 'tool_call'));
    const transcript = [
      { id: '297', uuid: QUESTION_ID, role: 'user', content: 'fu264 deep member approve', meta: {}, created_at: '2026-11-02 18:31:39' },
      {
        id: '298', uuid: MESSAGE_ID, role: 'assistant', content: '', meta: {}, created_at: '2026-11-02 18:31:40',
        persisted_trace: { projectId: '118', conversationId: '47', messageGroupId: 298, steps: traceRows, failed: false },
      },
    ] as unknown as MessageGroupWire[];
    const answer = convertMessagesToChatHistory(transcript, [], undefined, listing).find((message) => message.role === 'assistant');
    const memberTiers = tiers((answer?.toolActions ?? []) as SubAgentGroupable[]).filter((tier) => tier.name === MEMBER);
    // One tier per member instance, each holding its own inner call.
    expect(memberTiers).toHaveLength(2);
    for (const tier of memberTiers) expect(tier.calls).toContain(INNER);
  });
});
