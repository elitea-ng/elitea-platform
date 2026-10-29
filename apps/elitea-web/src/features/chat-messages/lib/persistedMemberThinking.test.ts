import { describe, expect, it } from 'vitest';

import { groupTraceStepsByMessageGroup } from '@/entities/message/lib/traceSteps';
import type { TraceStepRow } from '@/entities/message/lib/traceSteps';
import type { MessageGroupWire } from '@/entities/message/lib/wire';

import rows from '../__tests__/parallel-member-trace-rows.json';
import { convertMessagesToChatHistory } from './convertMessagesToChatHistory';
import { getSubAgentInstanceKey, getSubAgentName } from './executionHierarchy';
import { partitionActionsIntoBlocks } from './subAgentGrouping';
import type { SubAgentGroupable } from './subAgentGrouping';

/*
 * The light trace rows fanpar stored for one settled "FP inline mix" turn
 * (message group 146): a Parallel of three inline LLM branches (ask_llm asked
 * the user, approve_llm and reject_llm answered without a tool) and a saved
 * pipeline branch "FP llm reverse", plus the coordinator's own final step.
 * The conversation-wide listing reads `tool_call` rows only; the message page
 * reader attaches every kind as `persisted_trace`.
 */
const traceRows = rows as unknown as TraceStepRow[];
const listing = groupTraceStepsByMessageGroup(traceRows.filter((row) => row.kind === 'tool_call'));

function transcript(persistedTrace: unknown): MessageGroupWire[] {
  return [
    { id: '145', uuid: '0f0a1c2d-0000-4000-8000-000000000145', role: 'user', content: 'PM-5 run 1: say hello.', meta: {}, created_at: '2026-10-29 16:04:50' },
    {
      id: '146', uuid: '0f0a1c2d-0000-4000-8000-000000000146', role: 'assistant', content: 'joined', meta: {}, created_at: '2026-10-29 16:06:56',
      ...(persistedTrace === undefined ? {} : { persisted_trace: persistedTrace }),
    },
  ] as unknown as MessageGroupWire[];
}

function answerActions(persistedTrace: unknown): SubAgentGroupable[] {
  const answer = convertMessagesToChatHistory(transcript(persistedTrace), [], undefined, listing)
    .find((message) => message.role === 'assistant');
  return (answer?.toolActions ?? []) as SubAgentGroupable[];
}

function tiers(actions: readonly SubAgentGroupable[]) {
  const blocks = partitionActionsIntoBlocks(actions, {
    deriveName: (action) => getSubAgentName(action as never),
    deriveInstanceKey: (action) => getSubAgentInstanceKey(action as never),
    classifyWrapper: () => null,
  });
  return blocks.flatMap((block) => (block.kind === 'sub' ? [block] : []));
}

const reference = { projectId: '118', conversationId: '28', messageGroupId: 146, steps: traceRows, failed: false };

describe('a reloaded turn keeps its members\' thinking', () => {
  it('restores one tier per Parallel member, inline branches without a tool call included', () => {
    const names = tiers(answerActions(reference)).map((block) => block.name);
    expect(names.sort()).toEqual(['FP llm reverse', 'approve_llm', 'ask_llm', 'reject_llm']);
  });

  it('puts each restored step in its owner\'s tier, next to that owner\'s tool call', () => {
    const byName = new Map(tiers(answerActions(reference)).map((block) => [block.name, block.actions]));
    const ids = (name: string) => (byName.get(name) ?? []).map((action) => (action as { traceStepId?: number }).traceStepId);
    // The saved pipeline's tier holds its wrapper call and its inner thinking.
    expect(ids('FP llm reverse')).toEqual([570, 573]);
    expect(ids('approve_llm')).toEqual([572]);
    expect(ids('reject_llm')).toEqual([571]);
    // ask_llm's question (its ask_user call) and every step of its streamed
    // answer, in the converter's own timestamp order.
    expect([...ids('ask_llm')].sort((a, b) => (a ?? 0) - (b ?? 0))).toEqual([574, 575, 576, 577, 578]);
  });

  it('leaves the coordinator\'s own steps exactly as the tool-call listing alone built them', () => {
    const coordinator = (actions: readonly SubAgentGroupable[]) => actions.filter((action) => getSubAgentName(action as never) === '');
    expect(coordinator(answerActions(reference))).toEqual(coordinator(answerActions(undefined)));
    expect(answerActions(reference).some((action) => (action as { traceStepId?: number }).traceStepId === 579)).toBe(false);
  });

  it('keeps two concurrent instances of one saved agent in their own tiers', () => {
    // The same saved pipeline on two branches: its wrapper call and inner step
    // again, under a second member call id, interleaved with the first.
    const first = 'fo1_be57b086cd1499d9c53730cfdca6c5131ca600f314029f50b4293b7528316bef';
    const second = `fo1_${'9'.repeat(64)}`;
    const again = (row: TraceStepRow, id: number): TraceStepRow => JSON.parse(JSON.stringify(row).replaceAll(first, second).replace(`"id":${String(row.id)}`, `"id":${String(id)}`)) as TraceStepRow;
    const wrapper = traceRows.find((row) => row.id === 570)!;
    const inner = traceRows.find((row) => row.id === 573)!;
    const steps = [wrapper, again(wrapper, 970), inner, again(inner, 973)];
    const twoListing = groupTraceStepsByMessageGroup(steps.filter((row) => row.kind === 'tool_call'));
    const answer = convertMessagesToChatHistory(transcript({ ...reference, steps }), [], undefined, twoListing)
      .find((message) => message.role === 'assistant');
    const blocks = tiers((answer?.toolActions ?? []) as SubAgentGroupable[]);
    expect(blocks.map((block) => block.actions.map((action) => (action as { traceStepId?: number }).traceStepId)))
      .toEqual([[570, 573], [970, 973]]);
  });

  it('changes nothing for a turn whose rows hold no member thinking', () => {
    const own = { ...reference, steps: traceRows.filter((row) => row.kind === 'tool_call' || !row.parent_agent_name) };
    expect(answerActions(own)).toEqual(answerActions(undefined));
  });
});
