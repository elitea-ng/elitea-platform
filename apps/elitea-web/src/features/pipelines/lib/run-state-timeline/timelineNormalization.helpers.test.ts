import { describe, expect, it } from 'vitest';

import { PipelineStatus } from '../flow-editor/constants/flowEditor.constants';
import { PIPELINE_SOCKET_MESSAGE_TYPE } from '../flow-editor/helpers/parseRunsByEvent.support';
import type { TimelineRunSocketEvent } from './timeline.types';
import { groupConsecutiveSteps } from './timelineGrouping.helpers';
import { extractContinuedFailuresByStep } from './timelineNodeErrors.helpers';
import {
  normalizeRunEventsToGroupedTimeline,
  normalizeTimelineHierarchy,
} from './timelineNormalization.helpers';

describe('normalizeTimelineHierarchy', () => {
  it('extracts root owner and immediate scope from parent_agent_path', () => {
    const hierarchy = normalizeTimelineHierarchy({
      parent_agent_path: [
        { name: 'RootApp', call_id: 'call-root' },
        { name: 'ChildAgent', call_id: 'call-child', sibling_ordinal: 2 },
      ],
    });
    expect(hierarchy.owner).toEqual({ name: 'RootApp', call_id: 'call-root' });
    expect(hierarchy.scope).toEqual({ name: 'ChildAgent', call_id: 'call-child', sibling_ordinal: 2 });
  });

  it('rejects malformed or over-depth parent_agent_path and falls back cleanly', () => {
    const overDepth = [
      { name: 'a', call_id: '1' },
      { name: 'b', call_id: '2' },
      { name: 'c', call_id: '3' },
      { name: 'd', call_id: '4' },
    ];
    expect(normalizeTimelineHierarchy({ parent_agent_path: overDepth }).path).toEqual([]);
    expect(normalizeTimelineHierarchy({ parent_agent_path: 'malformed' }).owner).toBeUndefined();
  });
});

describe('normalizeRunEventsToGroupedTimeline', () => {
  it('groups nested application wrapper + inner LLM/tool steps under one group_id while tracking scope_id', () => {
    let clock = 1_700_000_000_000;
    const nextTick = () => {
      clock += 1000;
      return clock;
    };

    const events: TimelineRunSocketEvent[] = [
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolStart,
        response_metadata: {
          tool_name: 'sub_pipeline',
          tool_run_id: 'run-wrapper',
          metadata: {
            toolkit_type: 'pipeline',
            original_name: 'Sub Pipeline',
            parent_agent_call_id: 'app-call-1',
            langgraph_node: 'sub_pipeline',
          },
        },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentLlmStart,
        response_metadata: {
          tool_run_id: 'run-llm-1',
          metadata: {
            langgraph_node: 'planner',
            langgraph_checkpoint_ns: 'planner:1',
            parent_agent_path: [{ name: 'Sub Pipeline', call_id: 'app-call-1' }],
          },
        },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentLlmEnd,
        response_metadata: { tool_run_id: 'run-llm-1' },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentOnTransitionalEdge,
        response_metadata: {
          state: { answer: 'draft' },
          metadata: {
            langgraph_node: 'planner',
            langgraph_checkpoint_ns: 'planner:1',
            parent_agent_path: [{ name: 'Sub Pipeline', call_id: 'app-call-1' }],
          },
        },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolEnd,
        response_metadata: { tool_run_id: 'run-wrapper' },
      },
    ];

    const steps = normalizeRunEventsToGroupedTimeline(events, nextTick);
    expect(steps).toHaveLength(2);
    expect(steps[0]).toMatchObject({
      id: 'sub_pipeline',
      group_id: 'application:app-call-1',
      group_label: 'Sub Pipeline',
      status: PipelineStatus.Completed,
      is_nested: false,
    });
    expect(steps[1]).toMatchObject({
      id: 'planner',
      group_id: 'application:app-call-1',
      scope_id: 'application:app-call-1',
      status: PipelineStatus.Completed,
      is_nested: true,
      state: { answer: 'draft' },
    });

    const groups = groupConsecutiveSteps(steps);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.stepCount).toBe(2);
  });

  it('groups Map children, Parallel branches, and loop iterations and preserves node_errors', () => {
    const events: TimelineRunSocketEvent[] = [
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolStart,
        response_metadata: {
          tool_name: 'fetch_item',
          tool_run_id: 'm1',
          metadata: { langgraph_node: 'map_worker', map_item_index: 0 },
        },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentOnTransitionalEdge,
        response_metadata: {
          state: {
            node_errors: [{ node: 'map_worker', handled: 'continue', support_ref: 'SUP-77' }],
          },
          metadata: { langgraph_node: 'map_worker', map_item_index: 0 },
        },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentLlmStart,
        response_metadata: {
          tool_run_id: 'p1',
          metadata: { langgraph_node: 'branch_a', parallel_branch_id: 'branch-alpha' },
        },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolStart,
        response_metadata: {
          tool_name: 'search',
          tool_run_id: 'p2',
          metadata: { langgraph_node: 'branch_a', parallel_branch_id: 'branch-alpha' },
        },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.AgentLlmStart,
        response_metadata: {
          tool_run_id: 'iter-1',
          metadata: { langgraph_node: 'refine', iteration_index: 2 },
        },
      },
      {
        type: PIPELINE_SOCKET_MESSAGE_TYPE.PipelineFinish,
        response_metadata: {},
      },
    ];

    const steps = normalizeRunEventsToGroupedTimeline(events, () => 1_700_000_000_000);
    expect(steps.map(s => [s.group_id, s.step_kind])).toEqual([
      ['map:map_worker:0', 'map_child'],
      ['parallel:branch-alpha', 'parallel_child'],
      ['parallel:branch-alpha', 'parallel_child'],
      ['iteration:refine:2', 'llm'],
    ]);

    const continued = extractContinuedFailuresByStep(steps);
    expect(continued.get(0)?.[0]?.supportRef).toBe('SUP-77');
    expect(steps.at(-1)?.status).toBe(PipelineStatus.Completed);
  });
});
