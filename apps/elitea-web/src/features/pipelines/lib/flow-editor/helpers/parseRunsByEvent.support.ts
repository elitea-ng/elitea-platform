/**
 * Types + small pure helpers shared by `parseRunsByEvent.helpers.ts`.
 * Split out purely to keep that file under the §3.5 400-line budget — see
 * that file's doc comment for the actual port's provenance and citations;
 * this file has no baseline counterpart of its own.
 */
import type { SocketMessageType as SocketMessageTypeValue } from '@/entities/message';

import { PipelineNodeTypes } from '../constants/flowEditor.constants';

/** Local duplicate of `common/constants.js`'s `SocketMessageType` object, scoped to `parseRunEvent`'s switch discriminants — see `parseRunsByEvent.helpers.ts`'s doc comment. */
export const PIPELINE_SOCKET_MESSAGE_TYPE = {
  AgentStart: 'agent_start',
  AgentException: 'agent_exception',
  AgentToolStart: 'agent_tool_start',
  AgentToolEnd: 'agent_tool_end',
  AgentToolError: 'agent_tool_error',
  AgentHitlInterrupt: 'agent_hitl_interrupt',
  AgentLlmStart: 'agent_llm_start',
  AgentLlmEnd: 'agent_llm_end',
  AgentOnTransitionalEdge: 'agent_on_transitional_edge',
  AgentOnConditionalEdge: 'agent_on_conditional_edge',
  AgentOnDecisionEdge: 'agent_on_decision_edge',
  StartTask: 'start_task',
  PipelineFinish: 'pipeline_finish',
} as const satisfies Partial<Record<string, SocketMessageTypeValue>>;

export interface RunEventNode {
  readonly id: string;
  readonly toolkit_name?: string;
  readonly tool?: string;
  readonly type?: string;
}

export interface RunTimelineEntry {
  id: string;
  /** Fields below are `T | undefined` explicit, not `field?: T` shorthand — every push site computes them from `event.response_metadata`'s own optional chains and assigns the (possibly-`undefined`) result directly. */
  langgraph_node?: string | undefined;
  /** Absent on the interrupt-marker entry (`parseRunsByEvent.helpers.js:222-231`) — not every timeline entry represents an in-progress/completed step. */
  status?: string | undefined;
  state: Record<string, unknown>;
  created_at: number;
  tool_run_id?: string | undefined;
  source?: string | undefined;
  target?: string | undefined;
  error?: string | undefined;
}

export interface RunPipelineStatus {
  id: string;
  data: {
    label: string;
    timeline: RunTimelineEntry[];
    status: string;
    recoveryPaused?: boolean;
    error?: string;
  };
  type: string;
}

interface RunResponseMetadata {
  readonly node_recovery_required_v1?: unknown;
  readonly [key: string]: unknown;
  readonly metadata?: {
    readonly original_name?: string;
    readonly langgraph_node?: string;
    readonly toolkit_name?: string;
  };
  readonly tool_run_id?: string;
  readonly tool_name?: string;
  readonly toolkit_name?: string;
  readonly node_name?: string;
  readonly next_step?: string;
  readonly state?: Record<string, unknown>;
}

export interface RunSocketEvent {
  readonly message_id?: string;
  readonly execution_generation?: string;
  readonly code?: string;
  readonly type: string;
  readonly response_metadata: RunResponseMetadata;
  readonly content?: unknown;
}

/**
 * Finds the flow-graph node a streamed run event's raw tool/agent name
 * refers to (`parseRunsByEvent.helpers.js`'s inline `findNode`, lines 30-53).
 */
export const findNode = (nodes: readonly RunEventNode[], tool_name: string): RunEventNode | undefined =>
  nodes.find(node => {
    if (node.toolkit_name) {
      if (node.toolkit_name.length > tool_name.length) {
        return node.toolkit_name.startsWith(tool_name);
      }
      return tool_name.startsWith(node.toolkit_name);
    }
    if (node.id.length > tool_name.length) {
      return (
        node.id.startsWith(tool_name) ||
        node.id.replaceAll(' ', '').startsWith(tool_name) ||
        (tool_name === node.tool && node.type === PipelineNodeTypes.Agent)
      );
    }
    return (
      tool_name.startsWith(node.id) ||
      tool_name.startsWith(node.id.replaceAll(' ', '')) ||
      (tool_name === node.tool && node.type === PipelineNodeTypes.Agent)
    );
  });

/** Splits `toolkit___tool` (old format) into its toolkit segment; passes clean tool names through unchanged. */
export const toolkitNameFromRawToolName = (toolNameRaw: string): string =>
  toolNameRaw.includes('___') ? (toolNameRaw.split('___')[0] ?? toolNameRaw) : toolNameRaw;
