import { parseSubAgentLanes, settleSubAgentLanes, type CloudSubAgentLane } from '@elitea/client-core/stream';

import type { ToolStatus, ToolStep } from '@/features/chat/toolActivity';

/**
 * A cloud answer's temporary sub-agents (client contract 1.47, ADR-0050
 * decision 4) in the app's tool view: each child's own tool calls, grouped
 * (`groupByAgent`) under "<name> · temporary". Live turns fold the
 * `agent_subagent` frames (`@elitea/client-core/stream` `foldSubAgentEntry`);
 * a reloaded answer reads `meta.subagents_v1`. A settled child's final answer
 * is the group's last row; its in-between text is web and desktop only.
 */

const STATUS_BY_OK = (ok: boolean | undefined): ToolStatus => (ok === undefined ? 'running' : ok ? 'done' : 'error');

/** The group label of one child: its profile name, else "Sub-agent n", marked temporary. */
export function subAgentGroupLabel(lane: Pick<CloudSubAgentLane, 'displayName' | 'ordinal'>): string {
  return `${lane.displayName ?? `Sub-agent ${lane.ordinal}`} · temporary`;
}

const REPORT_STATUS: Record<Exclude<CloudSubAgentLane['status'], 'running'>, ToolStatus> = {
  completed: 'done',
  failed: 'error',
  budget_exhausted: 'error',
  cancelled: 'error',
};

/** A settled child's final answer: the last row of its group, its text in the detail sheet's Output. */
function reportStep(lane: CloudSubAgentLane, label: string): ToolStep {
  const status = lane.status === 'running' ? 'running' : REPORT_STATUS[lane.status];
  return {
    id: `${lane.agentId}:report`,
    name: lane.status === 'completed' ? 'Final answer' : 'Stopped',
    displayName: null,
    toolkit: null,
    parentAgent: label,
    status,
    startedAt: null,
    endedAt: null,
    inputs: null,
    inputsTruncated: false,
    output: lane.result ?? null,
    outputTruncated: false,
    partial: false,
    error: status === 'error' ? (lane.result ?? lane.error ?? 'Stopped') : null,
    chunks: null,
  };
}

/**
 * The children's tool calls as steps, then each settled child's final answer. Once the answer settled (`settled`), a
 * child the worker never ended was stopped with it, and a call of a stopped
 * child that never returned reads as stopped.
 */
export function subAgentSteps(lanes: readonly CloudSubAgentLane[], settled = false): ToolStep[] {
  const steps: ToolStep[] = [];
  for (const lane of settled ? settleSubAgentLanes(lanes) : lanes) {
    const label = subAgentGroupLabel(lane);
    for (const item of lane.items) {
      if (item.type !== 'tool') continue;
      const status = item.ok === undefined && lane.status !== 'running' ? 'error' : STATUS_BY_OK(item.ok);
      steps.push({
        id: `${lane.agentId}:${item.callId}`,
        name: item.tool === '' ? 'tool' : item.tool,
        displayName: null,
        toolkit: null,
        parentAgent: label,
        status,
        startedAt: null,
        endedAt: null,
        inputs: item.argsSummary ?? null,
        inputsTruncated: false,
        output: item.ok === true ? (item.summary ?? null) : null,
        outputTruncated: false,
        partial: true,
        error: item.ok === false ? (item.summary ?? 'Failed') : item.ok === undefined && status === 'error' ? 'Stopped' : null,
        chunks: null,
      });
    }
    if (lane.status !== 'running') steps.push(reportStep(lane, label));
  }
  return steps;
}

/** The lanes an answer keeps (`meta.subagents_v1`); a settled answer stops a child left running. */
export function storedSubAgents(meta: unknown, streaming: boolean): CloudSubAgentLane[] {
  const raw = meta !== null && typeof meta === 'object' && !Array.isArray(meta) ? (meta as Record<string, unknown>).subagents_v1 : undefined;
  const lanes = parseSubAgentLanes(raw);
  return [...(streaming ? lanes : settleSubAgentLanes(lanes))];
}
