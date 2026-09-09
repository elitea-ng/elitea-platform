import type { ReactNode } from 'react';

import type { AnswerCanvasSelection } from './AnswerContent';
import type { UserMessageUpdatedItem } from './UserMessage';
import type { HitlResumePayload } from '../chat-hitl-actions/ChatHitlActions';
import type { CanvasEditPayload, CodeBlockInfo } from '../canvas/Canvas';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

/**
 * Type module for `ChatMessageList.tsx`, split out purely to keep that file
 * under the §3.5 file-length budget — same rationale as
 * `ApplicationAnswer.types.ts`/`NewChatInput.types.ts`.
 * `ChatMessageListProps`/`ChatMessageListCanvas` are re-exported through
 * `features/chat-messages`' public barrel (`widgets/chat-box`'s
 * `ChatBox.helpers.ts` consumes `ChatMessageListCanvas` directly); the
 * other per-group interfaces below exist only to keep `ChatMessageListProps`
 * itself under the §3.5 component-props budget and have no consumer outside
 * `ChatMessageList.tsx`.
 */

/** Per-message action callbacks, grouped to stay under the component-props budget. */
export interface ChatMessageListActions {
  /** Called when a message is copied to clipboard. */
  readonly onCopyToClipboard?: ((message: ChatMessage) => void) | undefined;
  /** Called when an AI answer is deleted. */
  readonly onDeleteAnswer?: ((messageId: string) => void) | undefined;
  /** Called when an AI answer is regenerated. */
  readonly onRegenerateAnswer?: ((messageId: string) => void) | undefined;
  /**
   * Called with `(messageId, updatedItems)` when the single eligible user
   * message (see `getOnSubmit` gating below) is edited and resubmitted.
   */
  readonly onSubmitEditedMessage?:
    | ((messageId: string, updatedItems: readonly UserMessageUpdatedItem[]) => void)
    | undefined;
}

/** The canvas opener (issue 853), grouped to stay under the component-props budget: the handler that opens a stored canvas and the block already open. */
export interface ChatMessageListCanvas {
  readonly onEdit?: ((payload: CanvasEditPayload) => void) | undefined;
  readonly selected?: CodeBlockInfo | undefined;
  /** Carves a canvas out of a range the reader highlighted in an answer. */
  readonly onCreateFromSelection?: ((payload: AnswerCanvasSelection) => void) | undefined;
  /** Opens a text-like message attachment in the canvas editor (issue #878) — forwarded to every `UserMessage` row. */
  readonly onOpenFile?: ((source: { readonly bucket: string; readonly name: string }) => void) | undefined;
}

/** Read-aloud (TTS) props, grouped to stay under the component-props budget. */
export interface ChatMessageListTts {
  /** Whether auto-speak mode is active. */
  readonly autoSpeak?: boolean;
  /** Called for auto-speak (TTS) — threaded straight to each `ApplicationAnswer`. */
  readonly onAutoSpeak?: ((text: string, messageId: string) => void) | undefined;
  /** Currently speaking message ID. */
  readonly speakingMessageId?: string | undefined;
  /** TTS speaking segments. */
  readonly speakingSegments?: readonly unknown[] | undefined;
  /** TTS spoken range. */
  readonly spokenRange?: { readonly start: number; readonly end: number } | undefined;
}

/** MCP-auth / token-limit continue-execution and HITL props, grouped to stay under the component-props budget. */
export interface ChatMessageListContinuation {
  /** Called when the user continues a paused MCP-auth-required execution — only offered on the last message. */
  readonly onContinueMcpExecution?: ((messageId: string, addToIgnoreList?: boolean) => void) | undefined;
  /** Called when the user continues a token-limit-paused execution — only offered on the last message. */
  readonly onContinueTokenLimitExecution?: ((messageId: string) => void) | undefined;
  /** Called when a HITL interrupt is resumed — only offered on the last message. */
  readonly onHitlResume?: ((payload: HitlResumePayload) => void) | undefined;
  /** Hides the token-limit continue prompt even when a message is paused for it. */
  readonly hideContinueButton?: boolean;
  /** Hides HITL approval cards even when a message carries a pending interrupt. */
  readonly hideHitlActions?: boolean;
}

/** Older-messages pagination props, grouped to stay under the component-props budget. */
export interface ChatMessageListPagination {
  /** Whether older messages are being fetched (renders a loading skeleton above the list). */
  readonly isLoadingMore?: boolean;
  /** Called when the list is scrolled near the top — a future pagination consumer's trigger. */
  readonly onScrollToTop?: (() => void) | undefined;
}

/** @public Props for `ChatMessageList`. */
export interface ChatMessageListProps {
  /** The list of messages to render. */
  readonly chatHistory: readonly ChatMessage[];
  /** Whether a message is currently streaming. */
  readonly isStreaming?: boolean;
  /** The user ID for identifying the current user. */
  readonly userId?: string;
  /**
   * The project the conversation lives in — threaded to each `UserMessage`'s
   * attachment cards, whose artifact-storage download path refuses without it
   * (`NormalAttachment`'s own doc: "Required to download an
   * artifact-storage-backed attachment").
   */
  readonly projectId?: string | undefined;
  readonly messageActions?: ChatMessageListActions;
  /** Opens a `canvas_message` block in this transcript — supplied by the layer that mounts the canvas editor. Omitted, canvas blocks render with no open control. */
  readonly canvas?: ChatMessageListCanvas;
  readonly tts?: ChatMessageListTts;
  readonly continuation?: ChatMessageListContinuation;
  readonly pagination?: ChatMessageListPagination;
  /**
   * What to render instead of the transcript when `chatHistory` is empty.
   * The caller owns this because the empty branch is not just different
   * copy — `ChatBox` centres the greeting and the composer together as one
   * block (see `ChatBox.layout.ts`), and only the caller knows the user's
   * name to greet. Omitted, the plain fallback line below is used.
   */
  readonly emptyState?: ReactNode;
  /**
   * The answering participant's display name, captioned on every assistant
   * row (`<mark> Elitea to Message`). Supplied by `ChatBox`, which resolves
   * it from the conversation's active participant — `entities/message`'s
   * assistant normaliser keeps no participant, so the row cannot find it.
   */
  readonly assistantName?: string | undefined;
}
