/**
 * lib/chatStreamFrame.ts — the chat streaming wire vocabulary (issue #93).
 *
 * One envelope, two transports. The socket delivered these frames as
 * `chat_predict` receive events; the Go runtime delivers the SAME payload as
 * the `data` of an SSE `execution.node_event` (verified against a live stack:
 * the frame even carries `sio_event: "chat_predict"`, the emit it answers).
 * `shared/api/sse/executionEvents.ts` documents why the SSE event NAME is
 * `execution.node_event` rather than the `chat.stream.chunk` / `chat.stream.done`
 * that issue #93's table predicted — those names exist in no backend.
 *
 * Because the payload is identical, the reducer that consumes these frames is
 * transport-agnostic: `features/toolkits` already proves the pattern by casting
 * an `ExecutionEventData` straight into its socket-shaped handler
 * (`useToolkitChatSocket.hooks.ts:65-67`).
 *
 * `SocketMessageType` is the FULL baseline vocabulary
 * (`EliteaUI/src/common/constants.js:157-192`), not the subset the current
 * reducer slice handles. Listing every member is deliberate: an unhandled type
 * must be visibly unhandled — a trimmed enum turns "not ported yet" into
 * "unknown frame", which is how a half-migrated surface drops output silently.
 */

import type { MessageItemWire } from '@/entities/message/lib/wire';

/** Every `type` the chat stream can carry. Ported verbatim from the baseline. */
export const SocketMessageType = {
  AgentStart: 'agent_start',
  AgentResponse: 'agent_response',
  AgentException: 'agent_exception',
  AgentToolStart: 'agent_tool_start',
  AgentToolEnd: 'agent_tool_end',
  AgentToolError: 'agent_tool_error',
  AgentRequiresConfirmation: 'agent_requires_confirmation',
  AgentHitlInterrupt: 'agent_hitl_interrupt',
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

/**
 * The types that CHANGE MESSAGE STATE. Everything else in `SocketMessageType`
 * is state-inert by design — see the reducer's module doc.
 *
 * The `agent_on_*` graph frames are absent for the second reason, not the
 * first: they carry the flow editor's node highlighting and touch no message
 * (`agentGraphEvents.ts`). Listing them here would claim a state change the
 * baseline never made.
 *
 * Exported so a caller can tell "we chose not to render this yet" from "the
 * backend sent something nobody has ever seen", and so a test can assert the
 * boundary rather than infer it.
 */
export const HANDLED_STREAM_TYPES: ReadonlySet<string> = new Set<string>([
  SocketMessageType.StartTask,
  SocketMessageType.AgentStart,
  SocketMessageType.AgentLlmStart,
  SocketMessageType.AgentLlmChunk,
  SocketMessageType.Chunk,
  SocketMessageType.AIMessageChunk,
  SocketMessageType.AgentLlmEnd,
  SocketMessageType.AgentResponse,
  SocketMessageType.References,
  SocketMessageType.PipelineFinish,
  SocketMessageType.Error,
  SocketMessageType.LlmError,
  SocketMessageType.AgentException,
  SocketMessageType.AgentToolStart,
  SocketMessageType.AgentToolEnd,
  SocketMessageType.AgentToolError,
  SocketMessageType.AgentThinkingStep,
  SocketMessageType.AgentThinkingStepUpdate,
  SocketMessageType.AgentHitlInterrupt,
  SocketMessageType.AgentRequiresConfirmation,
  SocketMessageType.McpAuthorizationRequired,
  SocketMessageType.SwarmChildMessage,
  SocketMessageType.ChatPredictSummaryStarted,
  SocketMessageType.ChatPredictSummaryFinished,
  SocketMessageType.ChatUserMessage,
]);

/**
 * The response metadata the reducer reads. The baseline treats this blob as
 * open — it carries provider, langgraph and pipeline detail this slice does not
 * interpret — so unknown members are preserved rather than typed away.
 */
interface StreamResponseMetadata {
  readonly finish_reason?: string | undefined;
  readonly tool_run_id?: string | undefined;
  readonly tool_name?: string | undefined;
  readonly toolkit_name?: string | undefined;
  readonly toolkit_type?: string | undefined;
  readonly tool_inputs?: unknown;
  readonly tool_outputs?: unknown;
  readonly tool_output?: unknown;
  readonly tool_output_chunk_v1?: unknown;
  readonly tool_meta?:
    | {
        readonly name?: string | undefined;
        readonly description?: string | undefined;
        readonly metadata?: Record<string, unknown> | undefined;
        readonly [key: string]: unknown;
      }
    | undefined;
  readonly message?: unknown;
  readonly markdown?: boolean | undefined;
  readonly render_html?: boolean | undefined;
  readonly thinking_steps?: readonly ThinkingStep[] | undefined;
  readonly timestamp_start?: string | number | undefined;
  readonly timestamp_finish?: string | number | undefined;
  readonly thread_id?: string | undefined;
  readonly metadata?: { readonly thread_id?: string | undefined; readonly [key: string]: unknown } | undefined;

  // --- interrupts -----------------------------------------------------------
  /** N paused sub-agents in one frame; its mere presence selects parallel resume. */
  readonly hitl_interrupts?: readonly Record<string, unknown>[] | undefined;
  /** The nested single-pause detail, alongside the legacy top-level fields. */
  readonly hitl_interrupt?: Record<string, unknown> | undefined;
  readonly node_name?: string | undefined;
  readonly available_actions?: readonly string[] | undefined;
  readonly routes?: Record<string, unknown> | undefined;
  readonly edit_state_key?: string | undefined;
  readonly interrupt_id?: string | undefined;
  readonly resume_strategy?: string | undefined;

  // --- MCP authorization ----------------------------------------------------
  readonly resource_metadata_url?: string | undefined;
  readonly resource_metadata?:
    | {
        readonly authorization_servers?: readonly string[] | undefined;
        readonly resource_name?: string | undefined;
        readonly configuration_uuid?: string | undefined;
      }
    | undefined;
  readonly authorization_servers?: readonly string[] | undefined;
  readonly server_url?: string | undefined;
  readonly status?: number | undefined;

  readonly [key: string]: unknown;
}

/**
 * One entry of `agent_llm_end`'s `thinking_steps` fan-out: the model's account
 * of a step it already reported live, arriving in one batch at the end.
 */
export interface ThinkingStep {
  readonly tool_run_id?: string | undefined;
  readonly text?: unknown;
  readonly thinking?: string | undefined;
  readonly timestamp_finish?: string | number | undefined;
  readonly metadata?: Record<string, unknown> | undefined;
  readonly message?:
    | {
        readonly id?: string | undefined;
        readonly response_metadata?:
          | { readonly model_name?: string | undefined; readonly tool_name?: string | undefined; readonly metadata?: Record<string, unknown> | undefined }
          | undefined;
      }
    | undefined;
  readonly [key: string]: unknown;
}

/**
 * One streaming frame.
 *
 * `message_id` identifies the ASSISTANT message being streamed and
 * `question_id` the user turn it replies to; the baseline resolves a frame to
 * an existing message by either, because the two arrive in different frames
 * (`hooks.js:391-404`). Both are optional on the wire, which is why the reducer
 * has to tolerate a frame that identifies nothing.
 */
export interface ChatStreamFrame {
  readonly type?: string | undefined;
  readonly message_id?: string | undefined;
  readonly question_id?: string | undefined;
  readonly stream_id?: string | undefined;
  readonly content?: unknown;
  readonly thinking?: string | undefined;
  readonly response_metadata?: StreamResponseMetadata | undefined;
  readonly references?: readonly unknown[] | undefined;
  readonly created_at?: string | number | undefined;
  readonly sio_event?: string | undefined;
  /**
   * Swarm child fields, carried at the TOP level rather than in
   * `response_metadata` — both are first-class members of the node-event codec
   * (`elitea-main/internal/transport/runtimegrpc/nodeevent/codec.go:41`).
   *
   * On a `swarm_child_message` frame `message_id` is the CHILD's id, not the
   * assistant message being streamed, and `parent_message_id` names the message
   * the child's output belongs to.
   */
  readonly parent_message_id?: string | undefined;
  readonly agent_name?: string | undefined;
  readonly task_id?: string | undefined;
  /**
   * The user-echo fields, matching the declared socket schema
   * (`shared/api/socket/messages.ts:354-366`). `uuid` — not `message_id` — is
   * the id of the user message this frame describes.
   */
  readonly uuid?: string | undefined;
  readonly author_participant_id?: string | number | undefined;
  readonly sent_to_id?: string | number | undefined;
  readonly message_items?: readonly MessageItemWire[] | undefined;
  /**
   * The summary frames' own envelope. `chat_predict_summary_started` carries
   * its run id and model under `payload`, NOT under `response_metadata` like
   * every other frame — reading the usual place yields nothing.
   */
  readonly payload?:
    | {
        readonly response_metadata?: { readonly tool_run_id?: string | undefined } | undefined;
        readonly llm_settings?: { readonly model_name?: string | undefined } | undefined;
      }
    | undefined;
  readonly [key: string]: unknown;
}

/** Narrows an untyped SSE/socket payload to a frame worth reducing. */
export function isChatStreamFrame(value: unknown): value is ChatStreamFrame {
  return typeof value === 'object' && value !== null && typeof (value as { type?: unknown }).type === 'string';
}
