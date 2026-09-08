/**
 * Split out of `useChatBoxHandlers.ts` to stay under the file-length budget
 * (§3.5) — its dependency-injection types plus every pure (no closure-over-
 * `deps`-state) helper function the hook's action closures call. Nothing
 * here owns React state; `useChatBoxHandlers.ts` re-exports these types from
 * its own barrel-facing surface so `ui/index.ts`'s public export paths don't
 * change.
 */
import type { Dispatch, SetStateAction } from 'react';

import type { conversationApi } from '@/entities/conversation';
import type { ChatMessage } from '@/features/chat-messages';
import type { SocketClient } from '@/shared/api/socket/client';
import { ToolActionStatus } from '@/shared/lib/chat';
import { ROLES } from '@/shared/lib/enums';

import type { McpAuthorizationBatch } from './useChatBoxHandlers.authorization';

/** A resolved HITL interrupt action from the user ('approve'/'reject'/'edit'/'block_with_comment'). `value` carries the rewritten prompt/comment text for 'edit'/'block_with_comment'. `childThreadId` (Track-2 independent fan-out child) is present only when this decision resumes ONE still-running child independently of its siblings. */
export interface HitlInterruptAction {
  readonly action: string;
  readonly value?: string;
  readonly toolCallId?: string;
  readonly childThreadId?: string;
}
/** A single edited message item (baseline: `updatedItems`; matches `UserMessage.tsx`'s `UserMessageUpdatedItem` shape structurally). */
export interface UpdatedMessageItem { readonly uuid?: string | undefined; readonly content: string; readonly item_type: string; }
/**
 * Result returned by the send handler.
 *
 * `success` is about the TURN. `createdConversation` is about the ROW, and the
 * two are independent: `resolveConversationForSend` commits the conversation on
 * the server BEFORE any transport is tried, so a turn that is then refused
 * still leaves a real conversation behind. It is therefore present on a failed
 * result too, and the caller announces it either way — see
 * `useChatBoxActions`'s `handleSend`.
 */
export interface SendResult { readonly success: boolean; readonly createdConversation?: { readonly id?: string | number; readonly uuid?: string } }
export interface SendQuestionParams {
  readonly question: string;
  readonly attachments?: readonly File[];
  /** baseline: `getPayload`'s `isSendingToUser` (`ChatBox.jsx:488`) — truthy when this question targets specific user(s) or everyone rather than the active participant. */
  readonly isSendingToUser?: boolean;
  /** baseline: `getPayload`'s `userIds` (`ChatBox.jsx:489-498`). */
  readonly userIds?: readonly string[];
}
/** One uploaded attachment's outcome (structurally matches `entities/conversation`'s `UploadedAttachment`). */
interface UploadedAttachmentOutcome { readonly filepath?: string | undefined; readonly sanitizedName: string; }
/** Runtime dependencies injected by the composition root — plain values + function signatures, no React state/effects owned here. */
export interface ChatBoxHandlerDeps {
  /** Typed exactly as `SocketClient['emit']` so an invalid event name is a compile error. */
  readonly emitSocket: SocketClient['emit'];
  /** The current, live chat history — read access for looking up questions/answers/HITL interrupts by id. */
  readonly chatHistory: readonly ChatMessage[];
  /** Setter for the SAME state `chatHistory` reads (baseline: `useChatBoxData`'s `setChatHistory`). */
  readonly setChatHistory: Dispatch<SetStateAction<readonly ChatMessage[]>>;
  readonly isStreamingNow?: boolean;
  readonly setStreamingInfo: (questionId: string) => void;
  /** Builds the base `chat_predict` payload; falls back to a minimal built-in payload when not supplied (see module doc). */
  readonly generateMessagePayload?: (data: { question: string; questionId: string; participant: unknown; conversationUuid?: string | undefined; attachmentList?: readonly unknown[] | undefined; isSendingToUser?: boolean | undefined; userIds?: readonly string[] | undefined }) => Record<string, unknown>;
  /** Creates a new conversation for a fresh chat's first message — called BEFORE `chat_predict` is emitted. */
  readonly createConversation?: (question: string) => Promise<{ readonly id?: string | number; readonly uuid?: string } | undefined>;
  /** Uploads pending attachments once the target conversation is known, before `chat_predict` is emitted. */
  readonly uploadAttachments?: (conversationId: string | number, attachments: readonly File[]) => Promise<{ readonly success: boolean; readonly uploaded: readonly UploadedAttachmentOutcome[] }>;
  readonly triggerRegenerate?: (params: Parameters<typeof conversationApi.regenerate>[0]) => Promise<unknown>;
  /**
   * Returns what the server actually deleted, not `unknown`: one delete can
   * remove the answer AND the question it replies to, and `deleteAnswer` prunes
   * that set. Typed `unknown` — as it was — the ids are silently unreachable
   * and the paired question stays on screen with nothing failing to compile.
   */
  readonly triggerDeleteMessage?: (
    params: Parameters<typeof conversationApi.deleteMessage>[0],
  ) => Promise<Awaited<ReturnType<typeof conversationApi.deleteMessage>> | undefined>;
  readonly triggerDeleteAllMessages?: (params: Parameters<typeof conversationApi.deleteAllMessages>[0]) => Promise<unknown>;
  /** Available for a future stop-from-handlers call site; `streaming.stopStreaming` already covers the live NewChatInput stop button. */
  readonly triggerStopChatTask?: (params: Parameters<typeof conversationApi.stopTask>[0]) => Promise<unknown>;
  readonly getUserParticipant?: () => { id?: string; name?: string; avatar?: string };
  readonly getActiveParticipant?: () => unknown;
  readonly participants?: readonly unknown[] | undefined;
  /** REST-layer conversation identity — distinct from `conversationUuid`, the socket-layer identity. */
  readonly conversationId?: string | number | undefined;
  readonly conversationUuid?: string | undefined;
  readonly projectId?: string | number | undefined;
  /** Baseline: `socket?.id` — threaded into `regenerate`'s `sid` so the REST trigger links back to the caller's streaming connection. */
  readonly socketId?: string | undefined;
  /** Session-scoped bookkeeping of MCP servers declined/authenticated this conversation (never persisted). Lifetime owned by the caller. */
  readonly sessionDeclinedMcpServersRef?: { current: Map<string, Record<string, unknown>> };
  /** Pending exact decisions for a parallel delegated-authorization pause. */
  readonly sessionMcpAuthorizationBatchesRef?: { current: Map<string, McpAuthorizationBatch> };
  /** Returns the credential-native token map used only by the continuation request. */
  readonly getMcpTokens?: () => Record<string, unknown>;
  /**
   * Start the run over REST and subscribe to its SSE replay stream
   * (`features/chat-messages`'s `useChatStreamTransport`).
   *
   * Reports a `StreamStartOutcome`, not a boolean: a boolean cannot say WHY
   * the start did not happen, and the two reasons need opposite handling.
   *
   * Optional so a caller that has not wired the transport keeps the socket
   * behaviour unchanged.
   */
  readonly startStreamedExecution?: (params: {
    readonly conversationUuid: string;
    readonly payload: Record<string, unknown>;
  }) => Promise<StreamStartOutcome>;
  /**
   * Resume a PAUSED run over REST
   * (`POST /elitea_core/continue_predict/prompt_lib/{projectID}/{conversationID}`).
   *
   * Optional for the same reason as `startStreamedExecution`: a caller that
   * has not wired the transport keeps the socket behaviour unchanged.
   */
  readonly continueStreamedExecution?: (params: {
    readonly conversationUuid: string;
    readonly contract: string;
    readonly body: Record<string, unknown>;
  }) => Promise<StreamStartOutcome>;
  /** Regenerate one answer through Main and subscribe to its SSE replay. */
  readonly regenerateStreamedExecution?: (params: {
    readonly messageId: string;
    readonly questionId: string;
    readonly question: string;
    readonly updatedItems?: readonly UpdatedMessageItem[] | undefined;
  }) => Promise<StreamStartOutcome>;
}
/**
 * What one attempt to start a run over REST reports back.
 *
 * `started` — the transport owns the run. `chat_predict` must NOT also be
 * emitted: the two are separate starts, and emitting both runs the agent
 * twice.
 *
 * `no-transport` — this deployment serves no replay stream, or the body could
 * not satisfy the route's contract. The socket emit is the correct fallback.
 *
 * `rejected` — the turn cannot succeed and the reason is already known. The
 * socket fallback must NOT run, because it only hides the reason.
 *
 * `retry-later` — the server refused this attempt and said so is temporary
 * (regeneration only: HTTP 409 `agent_regeneration_pending`, `retryable: true`,
 * `Retry-After: 1`). Neither a fallback nor a failure: repeat the SAME request
 * after a short wait. Mirrors `AgentStreamStartAttempt` in
 * `features/chat-messages/model/useChatStreamRunStarters.ts`, which produces it
 * — the two types are structurally identical on purpose, so a transport result
 * is a `StreamStartOutcome` without a mapping step.
 */
export type StreamStartOutcome =
  | { readonly started: true }
  | { readonly started: false; readonly reason: 'no-transport' }
  | { readonly started: false; readonly reason: 'retry-later' }
  | { readonly started: false; readonly reason: 'rejected'; readonly message: string };
/** The run is live server-side; the SSE transport owns it. */
export const STREAM_STARTED: StreamStartOutcome = { started: true };
/** No REST start happened; the socket emit is the fallback. */
export const NO_STREAM_TRANSPORT: StreamStartOutcome = { started: false, reason: 'no-transport' };
/**
 * Emits one socket event and reports whether ANY transport took the payload.
 *
 * `SocketClient.emit` answers `false` for the no-op stub the app injects when
 * `vite_socket_server` is empty, and throws when socket.io itself refuses.
 * Both mean the payload went nowhere. The old call sites ignored the return
 * value, so on a socket-less deployment the message was dropped in silence.
 */
export function tryEmit(emit: () => boolean, label: string): boolean {
  try {
    return emit();
  } catch (error) {
    console.warn(`[useChatBoxHandlers] ${label} emit failed:`, error);
    return false;
  }
}
/**
 * An assistant-role message that carries the reason a turn never ran.
 *
 * It has to be a NEW message rather than a patch of the optimistic user
 * bubble. The user bubble carries no in-flight flag. The stream reducer's
 * settle pass therefore does not rewrite it. `ApplicationAnswer` renders
 * `ErrorTrace` only for an assistant answer that has an `exception`.
 */
export function buildFailedTurnMessage(questionId: string, message: string): ChatMessage {
  return {
    id: `${questionId}-error`,
    role: ROLES.Assistant,
    name: '',
    content: '',
    createdAt: new Date().toISOString(),
    questionId,
    exception: message,
  };
}
/**
 * Puts a message back the way it was before an optimistic continuation patch,
 * and shows why the resume did not happen.
 *
 * The patch clears `hitlInterrupt`/`hitlInterrupts`/`toolActions` and sets
 * `isLoading`/`isStreaming`. Without this revert a continuation that reached
 * no transport left the approval card gone and the bubble spinning for the
 * rest of the session.
 */
export function revertContinuation(setChatHistory: ChatBoxHandlerDeps['setChatHistory'], original: ChatMessage, message: string): void {
  setChatHistory((prev) =>
    prev.map((item) => (item.id !== original.id ? item : { ...original, isLoading: false, isStreaming: false, exception: message })),
  );
}
/** Result of the handlers hook. `regenerateAnswer`: baseline never emits a separate socket event — the REST call's `sid` links it to the live stream. */
export interface UseChatBoxHandlersResult {
  readonly sendQuestion: (params: SendQuestionParams) => Promise<SendResult>;
  readonly copyToClipboard: (message: ChatMessage) => Promise<boolean>;
  readonly regenerateAnswer: (messageId: string, updatedItems?: readonly UpdatedMessageItem[]) => Promise<void>;
  readonly deleteAnswer: (messageId: string) => Promise<void>;
  readonly clearChat: () => Promise<void>;
  readonly continueHitl: (action: HitlInterruptAction) => Promise<void>;
  /** Resume the exact authorization request. Skip applies only to this run. */
  readonly resumeMcpFlow: (messageId: string, addToIgnoreList?: boolean, authorizationRequestId?: string) => Promise<void>;
  readonly continueTokenLimit: (messageId: string) => Promise<void>;
}
export interface ToolActionLike {
  readonly id?: string;
  readonly authorizationRequestId?: string;
  readonly status?: string;
  readonly name?: string;
  readonly toolOutputs?: unknown;
  readonly toolMeta?: Record<string, unknown>;
}
function normalizeToolAction(action: ToolActionLike | undefined): { outputs: Record<string, unknown>; meta: Record<string, unknown>; name: string } {
  const outputs = action?.toolOutputs;
  return {
    outputs: typeof outputs === 'object' && outputs !== null ? (outputs as Record<string, unknown>) : {},
    meta: action?.toolMeta ?? {},
    name: action?.name ?? '',
  };
}
const firstTruthy = (...values: readonly unknown[]): unknown => values.find(Boolean);
export const toProjectIdString = (projectId: string | number | undefined): string => String(projectId ?? '');
export const resolveParticipantId = (participant: unknown): string | number | undefined => (participant as Record<string, unknown> | null)?.id as string | number | undefined;
/** The last `action_required` tool action on a message, if any (baseline: `toolActions.find(status === actionRequired)`). */
export function findActionRequiredToolAction(message: ChatMessage | undefined): ToolActionLike | undefined {
  const actions = (message?.toolActions ?? []) as readonly ToolActionLike[];
  return actions.find((action) => action.status === ToolActionStatus.actionRequired);
}
export function readServerUrl(action: ToolActionLike | undefined): string | undefined {
  const url = normalizeToolAction(action).outputs.server_url;
  return typeof url === 'string' && url !== '' ? url : undefined;
}
/** baseline: `ChatBox.jsx:804-811`/`1150-1159`'s declined-server bookkeeping entry. */
function buildDeclinedServerEntry(action: ToolActionLike | undefined): Record<string, unknown> {
  const { outputs, meta, name } = normalizeToolAction(action);
  return {
    actual_server_url: firstTruthy(meta.server_url) ?? null,
    tool_name: firstTruthy(outputs.tool_name, meta.tool_name, name) ?? '',
    resource_metadata_url: firstTruthy(outputs.resource_metadata_url) ?? null,
    www_authenticate: firstTruthy(outputs.www_authenticate) ?? null,
    resource_metadata: firstTruthy(outputs.resource_metadata) ?? null,
    toolkit_type: firstTruthy(outputs.toolkit_type, meta.toolkit_type) ?? null,
  };
}
/** Marks/unmarks an `action_required` MCP tool's server as session-declined (baseline: `ChatBox.jsx:793-812`/`1133-1166`). */
export function trackMcpAuthDecision(ref: ChatBoxHandlerDeps['sessionDeclinedMcpServersRef'], action: ToolActionLike | undefined, serverUrl: string | undefined, decline: boolean): void {
  if (!serverUrl || !ref) return;
  if (decline) ref.current.set(serverUrl, buildDeclinedServerEntry(action));
  else ref.current.delete(serverUrl);
}
/** `ChatMessage.content` already carries the flattened question text. */
export function findQuestionText(chatHistory: readonly ChatMessage[], message: ChatMessage): string | undefined {
  if (message.questionId === undefined) return undefined;
  return chatHistory.find((item) => item.id === message.questionId)?.content || undefined;
}
/** Minimal built-in `chat_predict` payload builder, used when the caller doesn't inject a fuller `generateMessagePayload` (see module doc). */
export function buildDefaultMessagePayload(data: { readonly question: string; readonly questionId: string; readonly participant: unknown; readonly conversationUuid?: string | undefined; readonly attachmentList?: readonly unknown[] | undefined; readonly isSendingToUser?: boolean | undefined; readonly userIds?: readonly string[] | undefined }): Record<string, unknown> {
  const participantId = resolveParticipantId(data.participant);
  return {
    question: data.question,
    question_id: data.questionId,
    conversation_uuid: data.conversationUuid,
    ...(participantId !== undefined ? { participant_id: participantId } : {}),
    ...(data.attachmentList && data.attachmentList.length > 0 ? { attachments: data.attachmentList } : {}),
    ...(data.isSendingToUser ? { isSendingToUser: data.isSendingToUser, userIds: data.userIds ?? [] } : {}),
  };
}
/** Shared `chat_continue_predict` base fields every continuation path emits. */
export function buildChatContinuePayload(deps: ChatBoxHandlerDeps, params: { readonly messageId: string; readonly threadId?: string | undefined; readonly question: string }): Record<string, unknown> {
  return {
    conversation_uuid: deps.conversationUuid,
    project_id: toProjectIdString(deps.projectId),
    message_id: params.messageId,
    thread_id: params.threadId,
    user_input: params.question,
  };
}
/** baseline: `[...sessionDeclinedMcpServersRef.current.entries()].map(...)`. */
export function buildDeclinedServersList(ref: ChatBoxHandlerDeps['sessionDeclinedMcpServersRef']): readonly Record<string, unknown>[] {
  if (!ref) return [];
  return [...ref.current.entries()].map(([url, rest]) => ({ server_url: (rest.actual_server_url as string | undefined) || url, ...rest }));
}
function extractMessageItemText(rawItem: unknown): string {
  const raw = rawItem as Record<string, unknown>;
  const details = raw.item_details as Record<string, unknown> | undefined;
  if (raw.item_type === 'canvas_message') return ((details?.latest_version as Record<string, unknown> | undefined)?.canvas_content as string | undefined) ?? '';
  if (raw.item_type === 'attachment_message') return `[${(details?.name as string | undefined) ?? 'Attachment'}]`;
  return (details?.content as string | undefined) ?? '';
}
export function extractCopyableContent(message: ChatMessage): string {
  if (message.messageItems?.length) return message.messageItems.map(extractMessageItemText).join(', ');
  return message.content || '';
}
export interface ResolvedConversation {
  readonly uuid?: string;
  readonly createdConversation?: { readonly id?: string | number; readonly uuid?: string };
}
/** Creates the conversation FIRST when one doesn't exist yet, deriving `conversation_uuid` from its result — baseline: "await onSend(...) first, derive conversationUuid, then emit chat_predict if conversationUuid" (`ChatBox.jsx:841-934`). */
export async function resolveConversationForSend(deps: ChatBoxHandlerDeps, question: string): Promise<ResolvedConversation> {
  if (deps.conversationUuid) return { uuid: deps.conversationUuid };
  if (!deps.createConversation) return {};
  const created = await deps.createConversation(question);
  if (!created) return {};
  return { ...(created.uuid !== undefined ? { uuid: created.uuid } : {}), createdConversation: created };
}
export const UPLOAD_FAILED = Symbol('upload-failed');
/** Uploads attachments once the target conversation is known, returning references to thread into the payload — baseline: `onPredictStream`'s attachment-upload-then-send flow (`ChatBox.jsx:867-904`). Returns `UPLOAD_FAILED` on failure (caller aborts the send). */
export async function uploadPendingAttachments(
  deps: ChatBoxHandlerDeps,
  attachments: readonly File[] | undefined,
  uploadConversationId: string | number | undefined,
): Promise<readonly unknown[] | undefined | typeof UPLOAD_FAILED> {
  if (!attachments || attachments.length === 0) return attachments;
  if (uploadConversationId === undefined || !deps.uploadAttachments) return attachments;
  const outcome = await deps.uploadAttachments(uploadConversationId, attachments);
  if (!outcome.success) return UPLOAD_FAILED;
  return outcome.uploaded.map((uploaded) => ({ filepath: uploaded.filepath, name: uploaded.sanitizedName }));
}
export function buildOptimisticUserMessage(questionId: string, question: string, userParticipant: { id?: string; name?: string; avatar?: string } | undefined, participantId: string | number | undefined): ChatMessage {
  return {
    id: questionId,
    role: ROLES.User,
    name: userParticipant?.name ?? '',
    content: question,
    createdAt: new Date().toISOString(),
    ...(userParticipant?.avatar !== undefined ? { avatar: userParticipant.avatar } : {}),
    ...(userParticipant?.id !== undefined ? { userId: userParticipant.id } : {}),
    ...(participantId !== undefined ? { participantId: String(participantId) } : {}),
  };
}
/**
 * The identifier this send's attachments are UPLOADED under: the
 * conversation's UUID, and never its numeric id.
 *
 * WHY THE UUID, when every sibling conversation route accepts either. The
 * upload endpoint keys the stored object `{this value}/{filename}`
 * (`finalizeAttachment`, services/elitea-main/internal/api/v2/conversations/
 * attachments.go), and admission then REFUSES any attachment reference whose
 * name is not prefixed by the conversation's UUID
 * (internal/application/agentexecution/attachments.go, `currentTurnAttachments`
 * — "this file was uploaded to this conversation"). That check is an
 * authorisation check, not a filing convention, so the client is the half that
 * has to agree with it.
 *
 * Sending the numeric id was not a cosmetic mismatch: the upload answered 201,
 * the start that followed was refused 400 BEFORE `admissions.Submit` ran, and
 * the whole turn — the user's question included — was lost, while the bytes sat
 * in the bucket until retention expired them. The server now refuses a numeric
 * id at the upload itself rather than storing an object no turn can use.
 *
 * BOTH PATHS, one rule. A conversation created by this very send carries its
 * uuid on the create response (`createdConversation.uuid`); a pre-existing one
 * has it on `deps.conversationUuid`, which the chat page fills from the
 * conversation-details query. Reading the created conversation's `id` — which
 * is what this used to do — silently produced the numeric key on exactly the
 * path a first attachment takes.
 */
export const resolveUploadConversationId = (createdConversation: { readonly id?: string | number; readonly uuid?: string } | undefined, fallbackUuid: string | undefined): string | undefined => createdConversation?.uuid ?? fallbackUuid;
export const buildSendResult = (createdConversation: { readonly id?: string | number; readonly uuid?: string } | undefined, success = true): SendResult =>
  createdConversation ? { success, createdConversation } : { success };
/** `chatHistory.find` for the question a given answer replies to. */
export function findQuestionForAnswer(chatHistory: readonly ChatMessage[], answer: ChatMessage | undefined): ChatMessage | undefined {
  if (answer?.questionId === undefined) return undefined;
  return chatHistory.find((item) => item.id === answer.questionId);
}
export const regeneratingPatch = (item: ChatMessage): ChatMessage => ({
  ...item, content: '', toolActions: [], exception: undefined, references: [], isLoading: true, isStreaming: true, createdAt: new Date().toISOString(),
});
export function buildRegeneratePayload(deps: ChatBoxHandlerDeps, messageId: string, questionMessage: ChatMessage | undefined, updatedItems: readonly UpdatedMessageItem[] | undefined): Record<string, unknown> {
  return {
    projectId: toProjectIdString(deps.projectId),
    id: messageId,
    message_id: messageId,
    stream_id: messageId,
    question: questionMessage?.content ?? '',
    question_id: questionMessage?.id,
    conversation_uuid: deps.conversationUuid ?? '',
    ...(deps.socketId !== undefined ? { sid: deps.socketId } : {}),
    ...(updatedItems?.length ? { updated_items: updatedItems } : {}),
  };
}
export function maybeSetStreamingInfo(setStreamingInfo: (id: string) => void, id: string | undefined): void {
  if (id !== undefined) setStreamingInfo(id);
}
