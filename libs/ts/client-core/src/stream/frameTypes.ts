/**
 * The chat stream wire vocabulary, shared by the web, desktop and mobile
 * clients.
 *
 * One envelope, two transports. The socket delivered these frames as
 * `chat_predict` receive events. The Go runtime delivers the SAME payload as
 * the `data` of an SSE `execution.node_event`. Each client folds the frames
 * into its own view model (see the package README: the reducers differ on
 * purpose); this module holds only the names and the classifications that
 * every client applies in the same way.
 *
 * `FRAME_TYPE` is the FULL baseline vocabulary
 * (`EliteaUI/src/common/constants.js:157-192`), not the subset a client
 * handles. An unhandled type must be visibly unhandled: a trimmed list turns
 * "not ported yet" into "unknown frame", and output is then dropped silently.
 */

/** The SSE `event:` names of the durable execution replay stream. */
export const EXECUTION_EVENT = {
  /** One chat stream frame (`FRAME_TYPE`) as JSON data. */
  node: 'execution.node_event',
  /** The run failed: `{code, safe_message, retryable}`. */
  failed: 'execution.failed',
  /**
   * The durable log was pruned past the resume cursor: some progress frames
   * between that cursor and this one are lost. Refill from REST.
   */
  replayReset: 'execution.replay_reset',
  /** An index ingest finished. */
  indexIngestCompleted: 'index.ingest.completed',
  /**
   * Client contract 1.27: a steering message is queued, or its pending text
   * was edited. `data` is one `SteeringMessage` (state `pending`).
   */
  steerQueued: 'steer.queued',
  /**
   * The worker injected a steering message at a safe point: the model reads
   * it now. `data` is the `SteeringMessage` (state `injected`, `message_id`).
   */
  steerInjected: 'steer.injected',
  /**
   * A steering message left the queue without injection. `data` is the
   * `SteeringMessage` plus `reason` (`STEERING_DROP_REASON`).
   */
  steerDropped: 'steer.dropped',
} as const;

/** The steering states of client contract 1.27 (`SteeringMessage.state`). */
export const STEERING_STATE = {
  Pending: 'pending',
  Injected: 'injected',
  Removed: 'removed',
  TurnEnded: 'turn_ended',
  /** A `turn_ended` message that a hand-off took as the next message. Final. */
  HandedOff: 'handed_off',
} as const;

export type SteeringState = (typeof STEERING_STATE)[keyof typeof STEERING_STATE];

/**
 * The `reason` of a `steer.dropped` event (client contract 1.27).
 *
 * - `removed`: the author removed the pending message.
 * - `superseded`: the turn ended before injection, and the author sent a
 *   newer message in the conversation first. The server does not send the
 *   message. The event comes on the stream of the author's newer turn, but
 *   its `response_message_id` names the OLD answer. A client hides the item
 *   and tells the user that the text was not sent.
 */
export const STEERING_DROP_REASON = {
  Removed: 'removed',
  Superseded: 'superseded',
} as const;

export type SteeringDropReason = (typeof STEERING_DROP_REASON)[keyof typeof STEERING_DROP_REASON];

/** The steering SSE event names: a client folds these into its queue view. */
export const STEERING_EVENTS: ReadonlySet<string> = new Set<string>([
  EXECUTION_EVENT.steerQueued,
  EXECUTION_EVENT.steerInjected,
  EXECUTION_EVENT.steerDropped,
]);

/** The `type` of each frame the chat stream can carry. */
export const FRAME_TYPE = {
  AgentStart: 'agent_start',
  AgentContextStatus: 'agent_context_status',
  AgentResponse: 'agent_response',
  FullMessage: 'full_message',
  AgentResultChunk: 'agent_result_chunk',
  AgentException: 'agent_exception',
  AgentToolStart: 'agent_tool_start',
  AgentToolEnd: 'agent_tool_end',
  AgentToolError: 'agent_tool_error',
  /**
   * Client contract 1.2 (#1066): the tool call paused for the user (a
   * sensitive-tool approval or a clarifying question) and did NOT fail. The
   * pause itself arrives next as `agent_hitl_interrupt`.
   */
  AgentToolPaused: 'agent_tool_paused',
  /**
   * One slice of a tool result too large for one output frame (#956). The
   * completed call that follows carries `tool_output_chunks` instead of the text.
   */
  AgentToolOutputChunk: 'agent_tool_output_chunk',
  AgentRequiresConfirmation: 'agent_requires_confirmation',
  AgentHitlInterrupt: 'agent_hitl_interrupt',
  AgentNodeRecoveryRequired: 'agent_node_recovery_required',
  McpAuthorizationRequired: 'mcp_authorization_required',
  AgentLlmStart: 'agent_llm_start',
  AgentLlmChunk: 'agent_llm_chunk',
  AgentLlmEnd: 'agent_llm_end',
  AgentOnFunctionToolNode: 'agent_on_function_tool_node',
  AgentOnToolNode: 'agent_on_tool_node',
  AgentOnTransitionalEdge: 'agent_on_transitional_edge',
  AgentOnConditionalEdge: 'agent_on_conditional_edge',
  AgentOnDecisionEdge: 'agent_on_decision_edge',
  References: 'references',
  Chunk: 'chunk',
  AIMessageChunk: 'AIMessageChunk',
  ChatUserMessage: 'chat_user_message',
  StartTask: 'start_task',
  Freeform: 'freeform',
  Error: 'error',
  LlmError: 'llm_error',
  PipelineFinish: 'pipeline_finish',
  AgentThinkingStep: 'agent_thinking_step',
  AgentThinkingStepUpdate: 'agent_thinking_step_update',
  ChatPredictSummaryStarted: 'chat_predict_summary_started',
  ChatPredictSummaryFinished: 'chat_predict_summary_finished',
  SwarmChildMessage: 'swarm_child_message',
  AgentSwarmAgentStart: 'agent_swarm_agent_start',
  AgentSwarmAgentResponse: 'agent_swarm_agent_response',
  AgentSwarmHandoff: 'agent_swarm_handoff',
} as const;

export type FrameType = (typeof FRAME_TYPE)[keyof typeof FRAME_TYPE];

/** The three chunk flavours: each carries a DELTA of the answer text. */
export const ANSWER_DELTA_TYPES: ReadonlySet<string> = new Set<string>([FRAME_TYPE.AgentLlmChunk, FRAME_TYPE.Chunk, FRAME_TYPE.AIMessageChunk]);

/** The frames on which the run FAILED. The worker sends no more node events after one. */
export const TURN_FAILURE_TYPES: ReadonlySet<string> = new Set<string>([FRAME_TYPE.Error, FRAME_TYPE.LlmError, FRAME_TYPE.AgentException]);

/** The tool-call lifecycle frames, keyed by `tool_run_id` (client contract 1.1). */
export const TOOL_CALL_TYPES: ReadonlySet<string> = new Set<string>([
  FRAME_TYPE.AgentToolStart,
  FRAME_TYPE.AgentToolEnd,
  FRAME_TYPE.AgentToolError,
  FRAME_TYPE.AgentToolPaused,
  FRAME_TYPE.AgentToolOutputChunk,
]);

/**
 * The `finish_reason` of a tool call that paused for the user (client
 * contract 1.2: `agent_tool_paused`, and the persisted trace step). A pause
 * is not a failure.
 */
export const PAUSE_FINISH_REASONS: ReadonlySet<string> = new Set<string>(['awaiting_approval', 'awaiting_input', 'interrupted']);

/** A frame body: a loose record. Every consumer reads it defensively. */
export type EventData = Readonly<Record<string, unknown>>;

/**
 * `JSON.parse` of an SSE `data` field at the transport boundary, as a value
 * and never a throw: malformed JSON, or JSON that is not an object, gives
 * `undefined`.
 */
export function parseEventData(data: unknown): EventData | undefined {
  if (typeof data !== 'string' || data === '') return undefined;
  try {
    const parsed: unknown = JSON.parse(data);
    return typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed) ? (parsed as EventData) : undefined;
  } catch {
    return undefined;
  }
}
