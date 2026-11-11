/**
 * The payloads of a workspace turn's sub-agent events (#1226; IPC.md
 * "Sub-agents"): `subagent_started`, `subagent_event`, `subagent_finished`.
 */

/** `subagent_started`'s payload. */
export interface SubAgentStartedPayload {
  agent_id: string;
  /** The `spawn_agent` call that started it. */
  parent_call_id: string;
  /** Its place among the turn's sub-agents, from 1. */
  ordinal: number;
  /** The `label`, else the profile's display name. */
  label?: string;
  /** The spawn's profile and brief (#1827): never the soul, a quote or a fact. */
  display_name?: string;
  avatar?: string;
  profile_digest?: string;
  brief_digest?: string;
  brief_refs?: { message_ids: string[]; attachments: string[]; memory_entries: string[]; facts: number };
}

/** A sub-agent's own event, inside `subagent_event`: the turn event's payload with its `kind` beside it. */
export type SubAgentInnerEvent =
  | { kind: 'tool_call'; call_id: string; tool: string; args_summary: string; remote: boolean }
  | { kind: 'tool_result'; call_id: string; ok: boolean; summary: string; truncated: boolean }
  | { kind: 'text_delta'; text: string };

/** How a sub-agent settled: `running` only on a report read before it did. */
export type SubAgentStatus = 'running' | 'completed' | 'failed' | 'budget_exhausted' | 'cancelled';

/** `subagent_finished`'s payload. */
export interface SubAgentFinishedPayload {
  agent_id: string;
  status: SubAgentStatus;
  steps: number;
  input_tokens: number;
  output_tokens: number;
  result?: string;
  error?: string;
}
