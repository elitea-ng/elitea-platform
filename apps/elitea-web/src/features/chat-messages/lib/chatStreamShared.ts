/**
 * lib/chatStreamShared.ts — the primitives every chat-stream frame family needs.
 *
 * This file exists because of the §3.5 file-length budget, and for no deeper
 * reason: `chatStreamReducer.ts` was one 1,181-line module whose switch covered
 * eight frame families, and the gate has no warning tier. The split is by frame
 * FAMILY (see the sibling `chatStreamXxxFrames.ts` modules); what lands here is
 * what more than one family reads — message lookup, message creation, the
 * immutable `replaceAt`, and the tool-action accessors the tool and thinking
 * families share. Nothing here changes behaviour; every function is the one that
 * stood above the switch, moved verbatim with its comment.
 */
import { ROLES } from '@/shared/lib/enums';

import type { MessageParticipantWire } from '@/entities/message/lib/wire';

import type { ToolAction } from './chatStreamToolAction';
import type { ChatMessage } from './convertMessagesToChatHistory';
import type { ChatStreamFrame } from './chatStreamFrame';

/** Caller-supplied identity for messages this reducer has to create. */
export interface ChatStreamContext {
  /** Participant the answer is attributed to, when the frame does not say. */
  readonly participantId?: string | undefined;
  /** Display name for a newly created assistant message. */
  readonly name?: string | undefined;
  /** Avatar for a newly created assistant message. */
  readonly avatar?: string | undefined;
  /** Injectable clock so tests do not depend on wall time. */
  readonly now?: () => string;
  /**
   * Whether the surface is in single-agent ("mono") chat.
   *
   * The baseline reads `isMonoChattingRef.current` when a turn stops on the
   * token limit: in mono chat the message keeps streaming (the continue button
   * resumes the same bubble), while in a multi-participant conversation it must
   * stop so the next participant can take the floor. A pure reducer has no
   * refs, so the caller supplies it.
   */
  readonly isMonoChatting?: boolean | undefined;
  /**
   * The conversation's participants, for `chat_user_message` to attribute an
   * echoed question to its author.
   *
   * The baseline reads `participantsRef.current`. A pure reducer has no refs,
   * and this is the only case that needs the roster — every other frame either
   * carries its own identity or falls back to `context.name`.
   */
  readonly participants?: readonly MessageParticipantWire[] | undefined;
}

export function nowIso(context: ChatStreamContext): string {
  return context.now ? context.now() : new Date().toISOString();
}

/**
 * Resolve the assistant message a frame belongs to.
 *
 * By `message_id` first, then by `questionId` — the baseline's `getMessage`
 * does the same (hooks.js:391-404) because the two identifiers arrive in
 * different frames: `start_task` names the question it answers before the
 * assistant message has an id of its own.
 */
export function findTarget(history: readonly ChatMessage[], frame: ChatStreamFrame): number {
  const messageId = frame.message_id;
  if (messageId) {
    const byId = history.findIndex((message) => message.id === messageId);
    if (byId !== -1) return byId;
  }
  const questionId = frame.question_id;
  if (questionId) {
    return history.findIndex(
      (message) => message.questionId === questionId && message.role === ROLES.Assistant,
    );
  }
  return -1;
}

/** A fresh assistant message for a frame that names one we have never seen. */
export function createAssistantMessage(frame: ChatStreamFrame, context: ChatStreamContext): ChatMessage {
  return {
    id: frame.message_id ?? crypto.randomUUID(),
    role: ROLES.Assistant,
    name: context.name ?? '',
    content: '',
    createdAt: nowIso(context),
    isStreaming: true,
    isLoading: true,
    ...(frame.execution_generation ? { executionGeneration: frame.execution_generation } : {}),
    ...(frame.question_id !== undefined ? { questionId: frame.question_id } : {}),
    ...(context.participantId !== undefined ? { participantId: context.participantId } : {}),
    ...(context.avatar !== undefined ? { avatar: context.avatar } : {}),
  };
}

export function replaceAt(
  history: readonly ChatMessage[],
  index: number,
  update: Partial<ChatMessage>,
): readonly ChatMessage[] {
  return history.map((message, position) => (position === index ? { ...message, ...update } : message));
}

export function threadIdOf(frame: ChatStreamFrame): string | undefined {
  const metadata = frame.response_metadata;
  const nested = metadata?.metadata?.thread_id;
  return typeof nested === 'string' ? nested : typeof metadata?.thread_id === 'string' ? metadata.thread_id : undefined;
}


export type { ToolAction } from './chatStreamToolAction';

/**
 * The metadata a tool frame carries, merged the way the baseline merges it:
 * `tool_meta.metadata` wins over `response_metadata.metadata`, because the
 * LangChain tool's own metadata is the more specific of the two.
 */
export function toolMetadata(frame: ChatStreamFrame): Record<string, unknown> {
  const responseMetadata = frame.response_metadata;
  return {
    ...responseMetadata?.metadata,
    ...responseMetadata?.tool_meta?.metadata,
  };
}

export function findToolAction(message: ChatMessage, runId: string | undefined): ToolAction | undefined {
  if (!runId) return undefined;
  const actions = message.toolActions as readonly ToolAction[] | undefined;
  return actions?.find((action) => action.id === runId);
}

/** Replace one tool action by id, preserving order and leaving the rest untouched. */
export function replaceToolAction(
  message: ChatMessage,
  runId: string,
  update: (action: ToolAction) => ToolAction,
): readonly ToolAction[] {
  const actions = (message.toolActions ?? []) as readonly ToolAction[];
  return actions.map((action) => (action.id === runId ? update(action) : action));
}
