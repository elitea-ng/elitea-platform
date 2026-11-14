/**
 * `indexChat.helpers.ts`'s (unit A4a) socket-message -> chat-history reducer
 * — split into this sibling file purely to keep `indexChat.helpers.ts`
 * under the repo's 400-line budget (R-eslint(max-lines)), after the
 * `applyStartTask` extraction and `switch`-to-`if`-chain rewrite already
 * done in-file to fix its two R-eslint(complexity) violations (`
 * applyAgentToolEnd` at 13, `generateChatMessageBasedOnResponse` at 14 —
 * both > 12). Zero behavior change — every branch below is unchanged from
 * the single file this used to be one contiguous part of. See
 * `indexChat.helpers.ts`'s own doc comment for the full port rationale
 * (baseline file, `ChatParticipantType`/`ToolActionStatus`/`SocketMessageType`
 * provenance, `crypto.randomUUID()` vs `uuid` decision) — all of it applies
 * unchanged to this file too.
 *
 * DELIBERATELY imports NOTHING from `indexChat.helpers.ts` (not even a
 * type): that file re-exports `generateChatMessageBasedOnResponse` FROM
 * this one (so `features/toolkits/lib/hooks/useToolkitChat*.hooks.ts`, unit
 * A4b, keeps its existing import path), and a reverse import here would
 * make that a circular module dependency — confirmed by `depcruise`'s
 * `no-cycle` rule on the first attempt, which fires on the module-graph
 * edge regardless of type-only vs value. `IndexChatMessage` therefore comes
 * from `indexChatMessage.types.ts` (a third file neither of these two
 * depends on the other for), and `generateMockMessageTemplate` — otherwise
 * a one-line import from `indexChat.helpers.ts` — is duplicated locally
 * below (non-exported, 7 lines, byte-identical) rather than imported.
 */
import { ToolActionStatus } from '@/shared/lib/chat';
import { ROLES } from '@/shared/lib/enums';
import { convertJsonToString } from '@/shared/lib/json';

import { IndexStatuses, SocketMessageType } from '../constants/indexDetails.constants';
import type { IndexChatMessage } from './indexChatMessage.types';
import { notifyTaskComplete, notifyTaskError } from './soundNotification.local';

/** Local duplicate of `indexChat.helpers.ts`'s exported `generateMockMessageTemplate` — see this file's own header doc comment for why. */
function generateMockMessageTemplateLocal(content: string, participantId: string): IndexChatMessage {
  return {
    id: crypto.randomUUID(),
    role: ROLES.Assistant,
    content,
    created_at: new Date().getTime(),
    participant_id: participantId,
  };
}

interface ResponseMetadataLike {
  tool_run_id?: string;
  tool_name?: string;
  tool_meta?: { description?: string; metadata?: Record<string, unknown>; [key: string]: unknown };
  metadata?: Record<string, unknown>;
  tool_inputs?: unknown;
  tool_output?: unknown;
  timestamp_start?: unknown;
  timestamp_finish?: unknown;
  finish_reason?: string;
  content_type?: string;
  message?: string;
}

interface SocketMessageLike {
  message_id: string;
  type: string;
  response_metadata?: ResponseMetadataLike;
  content?: unknown;
  created_at?: unknown;
}

export interface GenerateChatMessageBasedOnResponseParams {
  message: SocketMessageLike;
  chatHistory: readonly IndexChatMessage[];
  onFinish: (status: string) => void;
  onStartTask?: (taskId: string | undefined) => void;
}

/**
 * Extracts `toolkit_name` from a "Toolkit: name" line in the tool-meta
 * description when it is not already present in the merged metadata
 * (`indexChat.helpers.js:156-163`).
 */
function extractToolkitNameFromDescription(description: string): string | undefined {
  const descMatch = /(?:^|\n)Toolkit:\s*([^\n]+)/.exec(description);
  return descMatch?.[1]?.trim();
}

function buildToolActionMeta(responseMetadata: ResponseMetadataLike | undefined): Record<string, unknown> {
  const mergedMetadata: Record<string, unknown> = {
    ...responseMetadata?.tool_meta,
    ...responseMetadata?.metadata,
    ...responseMetadata?.tool_meta?.metadata,
  };

  if (!mergedMetadata['toolkit_name'] && responseMetadata?.tool_meta?.description) {
    const extracted = extractToolkitNameFromDescription(responseMetadata.tool_meta.description);
    if (extracted) mergedMetadata['toolkit_name'] = extracted;
  }

  return mergedMetadata;
}

function applyAgentToolStart(updatedHistory: IndexChatMessage[], message: SocketMessageLike): IndexChatMessage[] {
  const msgIndex = updatedHistory.findIndex((msg) => msg.id === message.message_id);
  if (msgIndex < 0) return updatedHistory;

  const msg = updatedHistory[msgIndex]!;
  msg.toolActions ??= [];

  const existingAction = msg.toolActions.find((t) => t.id === message.response_metadata?.tool_run_id);
  if (existingAction) {
    // A repeated start of a running step carries its newest name (an index run
    // reports "Indexing documents: 10 of 30" this way, #2041).
    if (existingAction.status === ToolActionStatus.processing && message.response_metadata?.tool_name) {
      existingAction.name = message.response_metadata.tool_name;
    }
    return updatedHistory;
  }

  msg.toolActions.push({
    name: message.response_metadata?.tool_name,
    id: message.response_metadata?.tool_run_id,
    status: ToolActionStatus.processing,
    toolInputs: message.response_metadata?.tool_inputs,
    toolMeta: buildToolActionMeta(message.response_metadata),
    created_at: message.response_metadata?.timestamp_start ?? message.created_at,
    type: 'tool',
  });

  return updatedHistory;
}

/** `applyAgentToolEnd`'s execution-time computation, extracted to keep that function's cyclomatic complexity under the repo's max-12 gate (R-eslint(complexity)). Returns `undefined` (never sets the field) unless both timestamps are present — byte-identical to the baseline's `if (toolAction.created_at && toolAction.ended_at) {...}` guard. */
function computeExecutionTimeSeconds(createdAt: unknown, endedAt: unknown): number | undefined {
  if (!createdAt || !endedAt) return undefined;
  const startTime = new Date(createdAt as string | number).getTime();
  const endTime = new Date(endedAt as string | number).getTime();
  return (endTime - startTime) / 1000;
}

function applyAgentToolEnd(updatedHistory: IndexChatMessage[], message: SocketMessageLike): IndexChatMessage[] {
  const toolMsgIndex = updatedHistory.findIndex((msg) => msg.id === message.message_id);
  if (toolMsgIndex < 0) return updatedHistory;

  const msg = updatedHistory[toolMsgIndex]!;
  const toolAction = msg.toolActions?.find((t) => t.id === message.response_metadata?.tool_run_id);

  if (toolAction) {
    toolAction.status = ToolActionStatus.complete;
    toolAction.toolOutputs = message.response_metadata?.tool_output;
    toolAction.content = convertJsonToString(message.content ?? '');
    toolAction.ended_at = message.response_metadata?.timestamp_finish ?? message.created_at;

    const executionTimeSeconds = computeExecutionTimeSeconds(toolAction.created_at, toolAction.ended_at);
    // Only assigned when both timestamps are present — matches the
    // baseline's `if (created_at && ended_at) {...}` guard exactly (an
    // unconditional assignment here would add `execution_time_seconds:
    // undefined` as an own property even when the baseline never touches
    // the field at all).
    if (executionTimeSeconds !== undefined) toolAction.execution_time_seconds = executionTimeSeconds;
  }

  if (message.response_metadata?.finish_reason === 'error') {
    msg.content = convertJsonToString(message.content ?? '');
  }

  return updatedHistory;
}

function buildToolExecutionSummary(msg: IndexChatMessage): string {
  return (
    msg.toolActions
      ?.slice()
      .sort((a, b) => new Date(a.created_at as string | number).getTime() - new Date(b.created_at as string | number).getTime())
      .map((action) => {
        const status = action.status === ToolActionStatus.cancelled || action.status === ToolActionStatus.error ? '❌' : '✅';
        const execTime = action.execution_time_seconds ? ` (${action.execution_time_seconds.toFixed(3)}s)` : '';
        return `${status} \`${action.name}\`${execTime}`;
      })
      .join('  \n') || ''
  );
}

function applyStreamingUpdate(
  updatedHistory: IndexChatMessage[],
  message: SocketMessageLike,
  onFinish: (status: string) => void,
): IndexChatMessage[] {
  const responseMsgIndex = updatedHistory.findIndex((msg) => msg.id === message.message_id);
  if (responseMsgIndex < 0) return updatedHistory;

  const msg = updatedHistory[responseMsgIndex]!;
  const contentType = message.response_metadata?.content_type;
  const shouldWrapInBlock = contentType === 'json';
  // `||`, not `??` — matches the baseline's `message.content ||
  // response_metadata.message` (`indexChat.helpers.js:222`) exactly: ANY
  // falsy `message.content` (including a legitimate empty-string streaming
  // chunk) falls back to `response_metadata.message`, not just
  // null/undefined. See this file's own header doc comment for the
  // zero-behavior-change porting convention this branch is part of.
  msg.content = convertJsonToString(message.content || message.response_metadata?.message, shouldWrapInBlock);
  msg.isLoading = false;

  if (message.response_metadata?.finish_reason) {
    msg.isStreaming = false;
    onFinish(IndexStatuses.success);
    notifyTaskComplete();

    const toolExecutionSummary = buildToolExecutionSummary(msg);
    if (toolExecutionSummary) {
      msg.content = `${toolExecutionSummary}\n\n\n${msg.content}`;
    }
  }

  return updatedHistory;
}

function applyAgentToolError(
  updatedHistory: IndexChatMessage[],
  message: SocketMessageLike,
  onFinish: (status: string) => void,
): IndexChatMessage[] {
  const errorMsgIndex = updatedHistory.findIndex((msg) => msg.id === message.message_id);
  if (errorMsgIndex < 0) return updatedHistory;

  const msg = updatedHistory[errorMsgIndex]!;
  const toolAction = msg.toolActions?.find((t) => t.id === message.response_metadata?.tool_run_id);
  if (toolAction) {
    toolAction.status = ToolActionStatus.error;
    toolAction.content = convertJsonToString(message.content ?? '');
    toolAction.ended_at = message.created_at;
  }
  msg.isLoading = false;
  msg.isStreaming = false;

  onFinish(IndexStatuses.fail);
  notifyTaskError();

  return updatedHistory;
}

function applyGeneralError(
  updatedHistory: IndexChatMessage[],
  message: SocketMessageLike,
  onFinish: (status: string) => void,
): IndexChatMessage[] {
  notifyTaskError();
  const finalMsgIndex = updatedHistory.findIndex((msg) => msg.id === message.message_id);

  if (finalMsgIndex >= 0) {
    const msg = updatedHistory[finalMsgIndex]!;
    msg.isLoading = false;
    msg.isStreaming = false;
    msg.exception = message.content;
    onFinish(IndexStatuses.fail);
    return updatedHistory;
  }

  const errorMessage = generateMockMessageTemplateLocal(
    `❌ Error occurred during tool testing:\n\n**Error:** ${convertJsonToString(message.content)}`,
    'toolkit',
  );
  return [...updatedHistory, errorMessage];
}

/** `SocketMessageType.StartTask`'s branch, extracted to its own named `apply*` function — same shape and reason as `applyAgentToolStart`/`applyAgentToolEnd`/etc above. Port of `indexChat.helpers.js:115-131`. */
function applyStartTask(
  updatedHistory: IndexChatMessage[],
  message: SocketMessageLike,
  onStartTask: ((taskId: string | undefined) => void) | undefined,
): IndexChatMessage[] {
  const taskId = message.content instanceof Object ? (message.content as { task_id?: string }).task_id : undefined;

  const loadingMessage: IndexChatMessage = {
    id: message.message_id,
    role: ROLES.Assistant,
    content: '🔄 Testing tool...',
    isLoading: true,
    isStreaming: true,
    task_id: taskId,
    toolActions: [],
    created_at: new Date().getTime(),
    participant_id: 'system',
  };

  onStartTask?.(taskId);

  return [...updatedHistory, loadingMessage];
}

/** The several `SocketMessageType` members that all resolve to `applyStreamingUpdate` (baseline: one `switch` case list, `indexChat.helpers.js:209-215`). */
const STREAMING_UPDATE_TYPES: ReadonlySet<string> = new Set([
  SocketMessageType.AgentThinkingStepUpdate,
  SocketMessageType.AgentThinkingStep,
  SocketMessageType.AgentResponse,
  SocketMessageType.Chunk,
  SocketMessageType.AIMessageChunk,
]);

/** The two `SocketMessageType` members that resolve to `applyGeneralError` (baseline: `indexChat.helpers.js:278-279`). */
const GENERAL_ERROR_TYPES: ReadonlySet<string> = new Set([SocketMessageType.Error, SocketMessageType.AgentException]);

/**
 * Reduces one socket message onto the in-progress index-test chat history.
 * Port of `indexChat.helpers.js`'s `generateChatMessageBasedOnResponse`
 * (108-307) — same case-by-case dispatch, but as a chain of `if`s over a
 * named `apply*` function per branch (grouped branches via the two
 * `ReadonlySet`s above) instead of a `switch`, to stay under the repo's
 * cyclomatic-complexity budget: oxlint's `complexity` check counts every
 * `case` label as its own branch regardless of fallthrough grouping, which
 * put the original `switch`-shaped version at 14 (max 12) purely from
 * case-label count, not from any real branching complexity. Same dispatch
 * table, same outcome per message type — zero behavior change.
 */
export function generateChatMessageBasedOnResponse(
  params: GenerateChatMessageBasedOnResponseParams,
): IndexChatMessage[] {
  const { message, chatHistory, onFinish, onStartTask } = params;
  const { type: socketMessageType } = message;

  const updatedHistory = [...chatHistory];

  if (socketMessageType === SocketMessageType.StartTask) return applyStartTask(updatedHistory, message, onStartTask);
  if (socketMessageType === SocketMessageType.AgentToolStart) return applyAgentToolStart(updatedHistory, message);
  if (socketMessageType === SocketMessageType.AgentToolEnd) return applyAgentToolEnd(updatedHistory, message);
  if (STREAMING_UPDATE_TYPES.has(socketMessageType)) return applyStreamingUpdate(updatedHistory, message, onFinish);
  if (socketMessageType === SocketMessageType.AgentToolError) return applyAgentToolError(updatedHistory, message, onFinish);
  if (GENERAL_ERROR_TYPES.has(socketMessageType)) return applyGeneralError(updatedHistory, message, onFinish);

  return updatedHistory;
}
