/**
 * The turn's read-only sub-agents (#1226; IPC.md "Sub-agents"), folded into
 * the turn's view as one lane per child: `subagent_started` opens it under the
 * `spawn_agent` call that started it, `subagent_event` adds the child's own
 * tool rows and text, `subagent_finished` settles it (again, after a
 * `send_input` continued it).
 */
import type { AgentEvent, SubAgentStatus } from '@/shared/desktop/workspaceIpc';

import type { TranscriptItem } from './transcriptItem';

/** One sub-agent as the transcript shows it, under its `spawn_agent` row. */
export interface SubAgentLane {
  agentId: string;
  /** The `spawn_agent` call that started it. */
  parentCallId: string;
  ordinal: number;
  label?: string;
  status: SubAgentStatus;
  /** Its own tool rows and text, in order. */
  items: TranscriptItem[];
  steps?: number;
  inputTokens?: number;
  outputTokens?: number;
  /** Its final report (or the text it had when it stopped). */
  result?: string;
  /** Why it stopped short: `subagent_failed`, `subagent_budget_exhausted`, `subagent_step_limit`, … */
  error?: string;
}

/** The part of a turn's view these events change. */
interface LaneView {
  subAgents?: SubAgentLane[] | undefined;
}

type EventOf<K extends AgentEvent['kind']> = Extract<AgentEvent, { kind: K }>;

type SubAgentEvent = EventOf<'subagent_started'> | EventOf<'subagent_event'> | EventOf<'subagent_finished'>;

export function isSubAgentEvent(event: AgentEvent): event is SubAgentEvent {
  return event.kind === 'subagent_started' || event.kind === 'subagent_event' || event.kind === 'subagent_finished';
}

function withLane<V extends LaneView>(view: V, agentId: string, change: (lane: SubAgentLane) => SubAgentLane): V {
  const lanes = view.subAgents ?? [];
  const index = lanes.findIndex((lane) => lane.agentId === agentId);
  // An event for a child whose start was lost: nothing to attach it to.
  if (index === -1) return view;
  return { ...view, subAgents: lanes.map((lane, i) => (i === index ? change(lane) : lane)) };
}

function onStarted<V extends LaneView>(view: V, event: EventOf<'subagent_started'>): V {
  const { agent_id: agentId, parent_call_id: parentCallId, ordinal, label } = event.payload;
  const lanes = view.subAgents ?? [];
  if (lanes.some((lane) => lane.agentId === agentId)) return view;
  const lane: SubAgentLane = { agentId, parentCallId, ordinal, status: 'running', items: [], ...(label === undefined ? {} : { label }) };
  return { ...view, subAgents: [...lanes, lane] };
}

/** A child's event after it settled: a `send_input` continued it, so it runs again. */
function withInner(settledLane: SubAgentLane, event: EventOf<'subagent_event'>): SubAgentLane {
  const lane: SubAgentLane = settledLane.status === 'running' ? settledLane : { ...settledLane, status: 'running' };
  const inner = event.payload.inner;
  switch (inner.kind) {
    case 'text_delta':
      return { ...lane, items: [...lane.items, { type: 'text', key: `t${event.seq}`, text: inner.text }] };
    case 'tool_call': {
      const { call_id: callId, tool, args_summary: argsSummary, remote } = inner;
      if (lane.items.some((item) => item.type === 'tool' && item.callId === callId)) return lane;
      return { ...lane, items: [...lane.items, { type: 'tool', key: `c${callId}`, callId, tool, argsSummary, remote }] };
    }
    case 'tool_result': {
      const { call_id: callId, ok, summary, truncated } = inner;
      const result = { ok, summary, truncated };
      return { ...lane, items: lane.items.map((item) => (item.type === 'tool' && item.callId === callId ? { ...item, result } : item)) };
    }
  }
}

function settled(lane: SubAgentLane, event: EventOf<'subagent_finished'>): SubAgentLane {
  const { status, steps, input_tokens: inputTokens, output_tokens: outputTokens, result, error } = event.payload;
  const { result: _before, error: _was, ...rest } = lane;
  return {
    ...rest,
    status,
    steps,
    inputTokens,
    outputTokens,
    ...(result === undefined ? {} : { result }),
    ...(error === undefined ? {} : { error }),
  };
}

export function applySubAgentEvent<V extends LaneView>(view: V, event: SubAgentEvent): V {
  switch (event.kind) {
    case 'subagent_started':
      return onStarted(view, event);
    case 'subagent_event':
      return withLane(view, event.payload.agent_id, (lane) => withInner(lane, event));
    case 'subagent_finished':
      return withLane(view, event.payload.agent_id, (lane) => settled(lane, event));
  }
}

/** The lanes of the children one `spawn_agent` call started, in start order. */
export function lanesOf(lanes: readonly SubAgentLane[] | undefined, callId: string): SubAgentLane[] {
  return (lanes ?? []).filter((lane) => lane.parentCallId === callId);
}
