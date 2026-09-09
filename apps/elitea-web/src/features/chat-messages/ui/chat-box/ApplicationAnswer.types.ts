import type { AnswerCanvasSelection } from './AnswerContent';
import type { HitlResumePayload } from '../chat-hitl-actions/ChatHitlActions';
import type { CanvasEditPayload, CodeBlockInfo } from '../canvas/Canvas';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

/**
 * Type module for `ApplicationAnswer.tsx`, split out purely to keep that
 * file under the §3.5 file-length budget — same rationale as
 * `NewChatInput.types.ts`/`UserInput.types.ts`. `ApplicationAnswerProps` is
 * the only one of these re-exported through `features/chat-messages`'
 * public barrel; the per-group interfaces below it exist only to keep
 * `ApplicationAnswerProps` itself under the §3.5 component-props budget
 * (see each group's own doc comment) and have no consumer outside
 * `ApplicationAnswer.tsx`.
 */

/** Loading/streaming/regenerating status flags, grouped to stay under the component-props budget. */
export interface ApplicationAnswerStatus {
  readonly isLoading?: boolean;
  readonly isStreaming?: boolean;
  readonly isRegenerating?: boolean;
}

/** Copy/delete/regenerate action handlers, grouped to stay under the component-props budget. */
export interface ApplicationAnswerActionHandlers {
  readonly onCopy?: (() => void) | undefined;
  readonly onDelete?: (() => void) | undefined;
  readonly onRegenerate?: (() => void) | undefined;
  readonly shouldDisableRegenerate?: boolean;
  /**
   * Opens the canvas editor for a `canvas_message` item in this answer — the
   * opener issue 853 is about. It rides in this group rather than as its own
   * prop because the component is at its §3.5 props ceiling, and it belongs
   * with the other per-message actions.
   */
  readonly onEditCanvas?: ((payload: CanvasEditPayload) => void) | undefined;
  /** The canvas block currently open in the editor, so this answer's copy of it shows a placeholder instead. */
  readonly selectedCodeBlockInfo?: CodeBlockInfo | undefined;
  /** Carves a canvas out of a range the reader HIGHLIGHTED in this answer — see `./AnswerContent`, which stamps this row's group onto it. */
  readonly onCreateCanvasFromSelection?: ((payload: AnswerCanvasSelection) => void) | undefined;
}

/** Read-aloud (TTS) props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerTts {
  readonly onAutoSpeak?: ((text: string, messageId: string) => void) | undefined;
  readonly speakingMessageId?: string;
  /** Not yet consumed, see module doc. */
  readonly speakingSegments?: readonly unknown[];
  /** The word being read aloud, as an offset range — see module doc for the `speakingMessageId` gate. */
  readonly spokenRange?: { readonly start: number; readonly end: number };
}

/** MCP-auth / token-limit continue-execution props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerContinuation {
  readonly onContinueMcpExecution?: ((messageId: string, addToIgnoreList?: boolean) => void) | undefined;
  readonly onContinueTokenLimitExecution?: ((messageId: string) => void) | undefined;
  readonly hideContinueButton?: boolean;
}

/** Message feedback (#880) props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerFeedback {
  /**
   * Absent (no thumbs control at all) when the caller has not resolved a
   * project id — matches `MessageAttachmentList`'s own `projectId?:
   * string` convention for the same reason: a control that needs a project
   * scope to work has nothing legal to call without one.
   */
  readonly projectId?: string | undefined;
  /** False for the welcome message and any message still loading/streaming — same gate `shouldDisableRegenerate` uses. */
  readonly enabled?: boolean;
}

/** HITL interrupt/resume props, grouped to stay under the component-props budget. */
export interface ApplicationAnswerHitl {
  /** HITL interrupt for resume (single-pause shape). */
  readonly hitlInterrupt?: unknown;
  /** HITL interrupts for resume (parallel-fan-out shape). */
  readonly hitlInterrupts?: readonly unknown[] | undefined;
  readonly onHitlResume?: ((payload: HitlResumePayload) => void) | undefined;
}

/** Caption-line identity: who answered, and whether this row is a sub-agent's. Grouped to stay under the §3.5 component-props budget. */
export interface ApplicationAnswerAuthor {
  /**
   * The answering participant's display name, shown in the caption line
   * (`<mark> Elitea to Message`). Supplied by the list, which is where the
   * conversation's participants are known — `entities/message`'s assistant
   * normaliser drops the participant, so the row cannot resolve it alone.
   */
  readonly participantName?: string | undefined;
  /** Whether this is a swarm child message. */
  readonly isSwarmChild?: boolean;
  /** Display name of the swarm agent. */
  readonly swarmAgentName?: string;
}

/** @public Props for `ApplicationAnswer`. */
export interface ApplicationAnswerProps {
  /** The AI answer message to render. */
  readonly answer: ChatMessage;
  /** Message ID for tracking. */
  readonly messageId: string;
  /** Tool actions for this answer (thinking steps, tool calls, swarm children). */
  readonly toolActions?: readonly SubAgentGroupable[] | undefined;
  /** Whether auto-speak mode is active. */
  readonly isSpeakingMode?: boolean;
  /** Whether this is the last message. */
  readonly isLastMessage?: boolean;
  /** Who the row is captioned as, grouped to stay under the component-props budget. */
  readonly author?: ApplicationAnswerAuthor;
  readonly status?: ApplicationAnswerStatus;
  readonly actions?: ApplicationAnswerActionHandlers;
  readonly tts?: ApplicationAnswerTts;
  readonly continuation?: ApplicationAnswerContinuation;
  readonly hitl?: ApplicationAnswerHitl;
  readonly feedback?: ApplicationAnswerFeedback;
}
