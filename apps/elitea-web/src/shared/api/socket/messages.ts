/**
 * The `SocketMessageType` discriminants a streamed socket message can carry,
 * and their payload schemas.
 *
 * The discriminant VALUES are not listed here: they are
 * `@elitea/client-core`'s `FRAME_TYPE`, the one chat stream vocabulary the
 * web, desktop and mobile clients share. This file used to hold its own
 * hand-written copy of the 34 baseline values, and it drifted: the six frame
 * types added after it (context status, full message, result chunk, tool
 * paused, tool output chunk, node recovery) were missing. The schemas below
 * are MAINTAINED BY HAND, for the reason events.ts's header gives: the
 * generator that produced this file required the Go prototype socket.io
 * server as an input, and #126 deleted it. Evidence below is the CLIENT side
 * only — apps/elitea-ui/src/common/constants.js and the call sites that read
 * each discriminant. messages.test.ts holds the union to exactly
 * `FRAME_TYPE`, so a frame type added to the package fails here until it
 * has a schema.
 */
import { FRAME_TYPE, type FrameType } from '@elitea/client-core/stream';
import { z } from 'zod';

/** Shared `response_metadata` shape — see events.ts for the full rationale (permissive by evidence, not by default). */
const responseMetadataSchema = z.record(z.string(), z.unknown());

/** Every SocketMessageType discriminant VALUE, in `FRAME_TYPE` declaration order. Narrow with `(typeof SOCKET_MESSAGE_TYPES)[number]` if a bare type name is ever needed. */
export const SOCKET_MESSAGE_TYPES: readonly FrameType[] = Object.values(FRAME_TYPE);

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:158
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1286-1305
 */
const AgentStartMessageSchema = z.looseObject({
  type: z.literal("agent_start"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFrame.ts
 */
const AgentContextStatusMessageSchema = z.looseObject({
  type: z.literal("agent_context_status"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:159
 *   - apps/elitea-ui/src/components/Chat/hooks.js:492-507
 */
const AgentResponseMessageSchema = z.looseObject({
  type: z.literal("agent_response"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFrame.ts
 */
const FullMessageMessageSchema = z.looseObject({
  type: z.literal("full_message"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFrame.ts
 */
const AgentResultChunkMessageSchema = z.looseObject({
  type: z.literal("agent_result_chunk"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:160
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1231-1266
 */
const AgentExceptionMessageSchema = z.looseObject({
  type: z.literal("agent_exception"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:161
 *   - apps/elitea-ui/src/components/Chat/hooks.js:669-803
 */
const AgentToolStartMessageSchema = z.looseObject({
  type: z.literal("agent_tool_start"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:162
 *   - apps/elitea-ui/src/components/Chat/hooks.js:856-895
 */
const AgentToolEndMessageSchema = z.looseObject({
  type: z.literal("agent_tool_end"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:163
 *   - apps/elitea-ui/src/components/Chat/hooks.js:896-908
 */
const AgentToolErrorMessageSchema = z.looseObject({
  type: z.literal("agent_tool_error"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts (client contract 1.2, #1066)
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFrame.ts
 */
const AgentToolPausedMessageSchema = z.looseObject({
  type: z.literal("agent_tool_paused"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts (#956)
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFrame.ts
 */
const AgentToolOutputChunkMessageSchema = z.looseObject({
  type: z.literal("agent_tool_output_chunk"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:164
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1011-1035
 */
const AgentRequiresConfirmationMessageSchema = z.looseObject({
  type: z.literal("agent_requires_confirmation"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:165
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1036-1212
 */
const AgentHitlInterruptMessageSchema = z.looseObject({
  type: z.literal("agent_hitl_interrupt"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts
 *   - apps/elitea-web/src/features/pipelines/lib/flow-editor/hooks/useRunEvent.ts
 */
const AgentNodeRecoveryRequiredMessageSchema = z.looseObject({
  type: z.literal("agent_node_recovery_required"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:166
 *   - apps/elitea-ui/src/components/Chat/hooks.js:909-1010
 *   - apps/elitea-ui/src/[fsd]/features/mcp/lib/hooks/useMcpAuthCheck.hooks.js:59-63
 */
const McpAuthorizationRequiredMessageSchema = z.looseObject({
  type: z.literal("mcp_authorization_required"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
  stream_id: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:167
 *   - apps/elitea-ui/src/components/Chat/hooks.js:509,669-803
 */
const AgentLlmStartMessageSchema = z.looseObject({
  type: z.literal("agent_llm_start"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:168
 *   - apps/elitea-ui/src/components/Chat/hooks.js:509-600
 */
const AgentLlmChunkMessageSchema = z.looseObject({
  type: z.literal("agent_llm_chunk"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
  thinking: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:169
 *   - apps/elitea-ui/src/components/Chat/hooks.js:601-668
 */
const AgentLlmEndMessageSchema = z.looseObject({
  type: z.literal("agent_llm_end"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:170
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1370-1377
 */
const AgentOnFunctionToolNodeMessageSchema = z.looseObject({
  type: z.literal("agent_on_function_tool_node"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:171
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1370-1377
 */
const AgentOnToolNodeMessageSchema = z.looseObject({
  type: z.literal("agent_on_tool_node"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:172
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1370-1377
 */
const AgentOnTransitionalEdgeMessageSchema = z.looseObject({
  type: z.literal("agent_on_transitional_edge"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:173
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1370-1377
 */
const AgentOnConditionalEdgeMessageSchema = z.looseObject({
  type: z.literal("agent_on_conditional_edge"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:174
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1370-1377
 */
const AgentOnDecisionEdgeMessageSchema = z.looseObject({
  type: z.literal("agent_on_decision_edge"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:175
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1214-1216
 */
const ReferencesMessageSchema = z.looseObject({
  type: z.literal("references"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
  references: z.array(z.unknown()).optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:176
 *   - apps/elitea-ui/src/components/Chat/hooks.js:492-507
 */
const ChunkMessageSchema = z.looseObject({
  type: z.literal("chunk"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:177
 *   - apps/elitea-ui/src/components/Chat/hooks.js:492-507
 */
const AIMessageChunkMessageSchema = z.looseObject({
  type: z.literal("AIMessageChunk"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:178
 *   - apps/elitea-ui/src/components/Chat/hooks.js:466-490
 */
const ChatUserMessageMessageSchema = z.looseObject({
  type: z.literal("chat_user_message"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
  author_participant_id: z.union([z.string(), z.number()]).optional(),
  uuid: z.string().optional(),
  sent_to_id: z.union([z.string(), z.number()]).optional(),
  message_items: z.array(z.unknown()).optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:179
 *   - apps/elitea-ui/src/components/Chat/hooks.js:385-421
 */
const StartTaskMessageSchema = z.looseObject({
  type: z.literal("start_task"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:180
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1284-1285
 */
const FreeformMessageSchema = z.looseObject({
  type: z.literal("freeform"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:181
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1217-1230
 */
const ErrorMessageSchema = z.looseObject({
  type: z.literal("error"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:182
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1268-1282
 */
const LlmErrorMessageSchema = z.looseObject({
  type: z.literal("llm_error"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:183
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1370-1377
 */
const PipelineFinishMessageSchema = z.looseObject({
  type: z.literal("pipeline_finish"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:184
 *   - apps/elitea-ui/src/components/Chat/hooks.js:821-854
 */
const AgentThinkingStepMessageSchema = z.looseObject({
  type: z.literal("agent_thinking_step"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:185
 *   - apps/elitea-ui/src/components/Chat/hooks.js:805-820
 */
const AgentThinkingStepUpdateMessageSchema = z.looseObject({
  type: z.literal("agent_thinking_step_update"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:186
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1378-1431
 */
const ChatPredictSummaryStartedMessageSchema = z.looseObject({
  type: z.literal("chat_predict_summary_started"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
  payload: z.record(z.string(), z.unknown()).optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:187
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1433-1445
 */
const ChatPredictSummaryFinishedMessageSchema = z.looseObject({
  type: z.literal("chat_predict_summary_finished"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:189
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1306-1368
 */
const SwarmChildMessageMessageSchema = z.looseObject({
  type: z.literal("swarm_child_message"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
  parent_message_id: z.string().optional(),
  agent_name: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:190
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1447-1452
 */
const AgentSwarmAgentStartMessageSchema = z.looseObject({
  type: z.literal("agent_swarm_agent_start"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:191
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1447-1452
 */
const AgentSwarmAgentResponseMessageSchema = z.looseObject({
  type: z.literal("agent_swarm_agent_response"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - apps/elitea-ui/src/common/constants.js:192
 *   - apps/elitea-ui/src/components/Chat/hooks.js:1447-1452
 */
const AgentSwarmHandoffMessageSchema = z.looseObject({
  type: z.literal("agent_swarm_handoff"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts (fan-out contract v1.1 §9)
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFanoutFrames.ts
 */
const AgentInterruptPendingMessageSchema = z.looseObject({
  type: z.literal("agent_interrupt_pending"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts (fan-out contract v1.1 §9)
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFanoutFrames.ts
 */
const AgentHitlResolvedMessageSchema = z.looseObject({
  type: z.literal("agent_hitl_resolved"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts (fan-out contract v1.1 §9)
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFanoutFrames.ts
 */
const AgentFanoutMemberMessageSchema = z.looseObject({
  type: z.literal("agent_fanout_member"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/**
 * evidence:
 *   - libs/ts/client-core/src/stream/frameTypes.ts (fan-out contract v1.1 §9)
 *   - apps/elitea-web/src/features/chat-messages/lib/chatStreamFanoutFrames.ts
 */
const AgentFanoutProgressMessageSchema = z.looseObject({
  type: z.literal("agent_fanout_progress"),
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  threadId: z.string().optional(),
});

/** `z.discriminatedUnion('type', [...])` over every `FRAME_TYPE` variant (spec §5.5). */
export const socketMessageSchema = z.discriminatedUnion('type', [
  AgentStartMessageSchema,
  AgentContextStatusMessageSchema,
  AgentResponseMessageSchema,
  FullMessageMessageSchema,
  AgentResultChunkMessageSchema,
  AgentExceptionMessageSchema,
  AgentToolStartMessageSchema,
  AgentToolEndMessageSchema,
  AgentToolErrorMessageSchema,
  AgentToolPausedMessageSchema,
  AgentToolOutputChunkMessageSchema,
  AgentRequiresConfirmationMessageSchema,
  AgentHitlInterruptMessageSchema,
  AgentNodeRecoveryRequiredMessageSchema,
  AgentInterruptPendingMessageSchema,
  AgentHitlResolvedMessageSchema,
  AgentFanoutMemberMessageSchema,
  AgentFanoutProgressMessageSchema,
  McpAuthorizationRequiredMessageSchema,
  AgentLlmStartMessageSchema,
  AgentLlmChunkMessageSchema,
  AgentLlmEndMessageSchema,
  AgentOnFunctionToolNodeMessageSchema,
  AgentOnToolNodeMessageSchema,
  AgentOnTransitionalEdgeMessageSchema,
  AgentOnConditionalEdgeMessageSchema,
  AgentOnDecisionEdgeMessageSchema,
  ReferencesMessageSchema,
  ChunkMessageSchema,
  AIMessageChunkMessageSchema,
  ChatUserMessageMessageSchema,
  StartTaskMessageSchema,
  FreeformMessageSchema,
  ErrorMessageSchema,
  LlmErrorMessageSchema,
  PipelineFinishMessageSchema,
  AgentThinkingStepMessageSchema,
  AgentThinkingStepUpdateMessageSchema,
  ChatPredictSummaryStartedMessageSchema,
  ChatPredictSummaryFinishedMessageSchema,
  SwarmChildMessageMessageSchema,
  AgentSwarmAgentStartMessageSchema,
  AgentSwarmAgentResponseMessageSchema,
  AgentSwarmHandoffMessageSchema,
]);

/** Not exported: no consumer needs the bare type name yet — SocketMessageParseResult (below, exported) already carries it structurally. Export when a Wave-2 consumer needs to name it directly. */
type SocketMessage = z.infer<typeof socketMessageSchema>;

export type SocketMessageParseResult =
  | { readonly ok: true; readonly message: SocketMessage }
  | { readonly ok: false; readonly reason: 'unknown_event'; readonly rawType: string | undefined; readonly raw: unknown };

/**
 * Validate a raw socket payload against the `FRAME_TYPE` union. Spec
 * §5.5: "unknown discriminants route to a logged `unknown_event` branch
 * rather than crashing or silently dropping" — mirrors the old app's own
 * fallback exactly (components/Chat/hooks.js:1453-1460,
 * `console.warn('unknown message type', socketMessageType)`). Never throws.
 */
export function parseSocketMessage(raw: unknown): SocketMessageParseResult {
  const parsed = socketMessageSchema.safeParse(raw);
  if (parsed.success) {
    return { ok: true, message: parsed.data };
  }
  const rawType =
    raw !== null && typeof raw === 'object' && 'type' in raw && typeof (raw as { type: unknown }).type === 'string'
      ? (raw as { type: string }).type
      : undefined;
  // eslint-disable-next-line no-console -- parity: components/Chat/hooks.js:1459's own console.warn fallback
  console.warn('unknown message type', rawType);
  return { ok: false, reason: 'unknown_event', rawType, raw };
}
