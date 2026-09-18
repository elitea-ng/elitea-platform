/**
 * #951 — the reopened conversation's pins. These assert the MAP, not the
 * render: what the light listing can and cannot restore is the whole question,
 * and `buildToolActions` (already covered) does the rest.
 */
import { describe, expect, it } from 'vitest';

import { normaliseAssistantMessage } from './normalise';
import { groupTraceStepsByMessageGroup } from './traceSteps';
import type { PersistedTraceSteps, TraceStepRow } from './traceSteps';
import type { MessageGroupWire } from './wire';

const ROWS: readonly TraceStepRow[] = [
  {
    id: 11,
    message_group_id: 1121,
    kind: 'tool_call',
    tool_name: 'mock_tool_status',
    parent_agent_name: null,
    parent_agent_call_id: null,
    started_at: '2026-09-18T10:00:00Z',
    finished_at: '2026-09-18T10:00:02Z',
    is_error: false,
    attrs: { tool_meta: { display_name: 'Status', metadata: { toolkit_name: 'mock', toolkit_type: 'openapi' } } },
  },
  {
    id: 12,
    message_group_id: 1121,
    kind: 'tool_call',
    tool_name: 'mock_tool_create',
    started_at: '2026-09-18T10:00:03Z',
    finished_at: '2026-09-18T10:00:04Z',
    is_error: true,
    attrs: null,
  },
  // A thinking step. The listing has already dropped the BLANK ones
  // (`has_visible_content`), so a listed one has something to show — just not
  // in this projection, which carries no `text`.
  { id: 13, message_group_id: 1121, kind: 'thinking_step', started_at: '2026-09-18T10:00:01Z', is_error: false },
  { id: 14, message_group_id: 1130, kind: 'tool_call', tool_name: 'other', is_error: false },
];

const EMPTY: PersistedTraceSteps = { toolCalls: [], thinkingSteps: [] };

function stepsFor(groupId: string): PersistedTraceSteps {
  return groupTraceStepsByMessageGroup(ROWS).get(groupId) ?? EMPTY;
}

function group(meta?: MessageGroupWire['meta']): MessageGroupWire {
  return {
    id: 1121,
    uuid: 'group-uuid',
    content: 'answer',
    created_at: '2026-09-18T10:00:00Z',
    ...(meta === undefined ? {} : { meta }),
  };
}

describe('groupTraceStepsByMessageGroup', () => {
  it('keys rows by their message group, as strings, and splits the two kinds', () => {
    const grouped = groupTraceStepsByMessageGroup(ROWS);
    expect([...grouped.keys()].sort()).toEqual(['1121', '1130']);
    expect(stepsFor('1121').toolCalls).toHaveLength(2);
    expect(stepsFor('1121').thinkingSteps).toHaveLength(1);
  });

  it('carries the display sidecar and the error flag a resting pin draws', () => {
    const [status, create] = stepsFor('1121').toolCalls;
    expect(status?.tool_meta?.display_name).toBe('Status');
    expect(status?.timestamp_finish).toBe('2026-09-18T10:00:02Z');
    expect(status?.error).toBeUndefined();
    expect(create?.error).toBe('error');
  });

  it('carries the row identity the modal fetches the body with', () => {
    expect(stepsFor('1121').toolCalls[0]).toMatchObject({ trace_step_id: 11, trace_message_group_id: 1121 });
    expect(stepsFor('1121').thinkingSteps[0]).toMatchObject({ trace_step_id: 13, trace_message_group_id: 1121 });
  });
});

describe('normaliseAssistantMessage with persisted trace steps', () => {
  it('builds the pins a reopened turn has no meta for, in step order', () => {
    const message = normaliseAssistantMessage(group({}), [], undefined, stepsFor('1121'));
    // Sorted by timestamp: status (10:00:00), the thinking step (10:00:01),
    // then create (10:00:03).
    expect((message.toolActions ?? []).map((action) => action.name)).toEqual([
      'mock_tool_status',
      'Thinking step',
      'mock_tool_create',
    ]);
    expect((message.toolActions ?? [])[2]?.isError).toBe(true);
  });

  it('keeps a text-less thinking step only because it was restored from a row', () => {
    const [restored] = normaliseAssistantMessage(group({}), [], undefined, {
      toolCalls: [],
      thinkingSteps: stepsFor('1121').thinkingSteps,
    }).toolActions ?? [];
    expect(restored).toMatchObject({ traceStepId: 13, traceMessageGroupId: 1121 });
    // A LIVE text-less thinking step is still dropped — the source's own rule.
    expect(
      normaliseAssistantMessage(group({ thinking_steps: [{ text: '' }] }), [], undefined).toolActions ?? [],
    ).toEqual([]);
  });

  it('renders nothing extra when there are no persisted steps — the defect it closes', () => {
    expect(normaliseAssistantMessage(group({}), [], undefined).toolActions ?? []).toEqual([]);
  });

  it('leaves a turn that still carries its own meta.tool_calls alone', () => {
    const meta = { tool_calls: { a: { tool_name: 'from_meta', tool_run_id: 'a' } } } as MessageGroupWire['meta'];
    const message = normaliseAssistantMessage(group(meta), [], undefined, stepsFor('1121'));
    expect((message.toolActions ?? []).map((action) => action.name)).toEqual(['from_meta']);
    expect((message.toolActions ?? [])[0]?.traceStepId).toBeUndefined();
  });
});
