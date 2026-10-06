import type { NodeRecoveryBinding } from '@/shared/lib/nodeRecovery';
import type { StaticPauseBinding } from './staticPipelinePause.types';
import type { MessageItemWire } from '@/entities/message/lib/wire';
import type { SubAgentGroupable } from '@/entities/message/lib/subAgentGrouping';

/**
 * Unified chat message returned by `convertMessagesToChatHistory`.
 * Combines the `UserMessage` and `AssistantMessage` entity types plus
 * list-level fields (`questionId`, `replyToId`, `originalId`) that
 * `convertMessagesToChatHistory` adds at the conversation level.
 */
export interface ChatMessage {
  readonly nodeRecoveryRequired?: NodeRecoveryBinding | undefined;
  readonly staticPause?: StaticPauseBinding | undefined;
  readonly executionGeneration?: string | undefined;
  /** Final result metadata delivered after the success progress markers. */
  readonly responseMetadata?: Readonly<Record<string, unknown>> | undefined;
  readonly resultChunk?: unknown;
  readonly assembledResult?: string | undefined;
  readonly continuedResultPrefix?: string | undefined;
  readonly persistedTrace?: unknown;
  readonly id: string;
  readonly role: string;
  readonly name: string;
  readonly avatar?: string | undefined;
  readonly content: string;
  readonly createdAt: string;
  /** When the row was last REWRITTEN, present only when it has been (issue 975): a regeneration rewrites the answer in place and keeps `createdAt`, so this — preferred by the renderer, `createdAt` as the fallback — is when the text on screen actually arrived. */ readonly updatedAt?: string | undefined;
  readonly messageItems?: readonly MessageItemWire[] | undefined;
  readonly userId?: string | undefined;
  readonly participantId?: string | undefined;
  readonly sentTo?: unknown;
  readonly likes?: number | undefined;
  readonly interactionUuid?: string | undefined;
  readonly toolActions?: readonly SubAgentGroupable[] | undefined;
  readonly exception?: unknown;
  readonly failureCode?: string;
  readonly isStreaming?: boolean | undefined;
  readonly isLoading?: boolean | undefined;
  /**
   * Set alongside `isStreaming`/`isLoading` by the stream reducer's interrupt
   * slice. A pause has to CLEAR it: `isMessageInFlight` treats it as in-flight,
   * so a message that paused mid-regenerate would keep the composer disabled
   * and suppress the live thinking view that hosts the approval card.
   */
  readonly isRegenerating?: boolean | undefined;
  readonly questionId?: string | undefined;
  readonly replyToId?: string | undefined;
  readonly references?: readonly unknown[] | undefined;
  readonly isSummarized?: boolean | undefined;
  readonly hitlInterrupt?: unknown;
  readonly hitlInterrupts?: readonly unknown[] | undefined;
  /**
   * The continue-button prompt shown when a turn stopped on the model's token
   * limit rather than on a completed answer. `ApplicationAnswer` reads it to
   * decide whether to render `ChatContinue`.
   */
  readonly requiresConfirmation?: { readonly message: string; readonly buttonText: string } | undefined;
  readonly threadId?: string | undefined;
  readonly taskId?: string | undefined;
  readonly originalId?: string | number | undefined;
  /** How many persistent memories (#870) this turn's recall used — see `AssistantMessage.memoriesUsed`'s own comment. */
  readonly memoriesUsed?: number | undefined;
  /**
   * A17: this question was typed while a previous turn was still running and
   * was delivered from the "Waiting messages" queue once that turn settled
   * (ELITEA-2870). Not a wire field — the store has nowhere to put it — so it
   * is stamped on by `widgets/chat-box`'s queue model from the question ids it
   * recorded, which is why it survives a reload: the id is the client's own
   * `question_id`, persisted as the question row's uuid.
   */
  readonly interjected?: boolean | undefined;
}
