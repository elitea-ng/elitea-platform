import type { MessageClass } from '@elitea/client-core/conversation';
import type { CloudSubAgentLane } from '@elitea/client-core/stream';

import type { GoalCheckView, RunBriefView } from '@/entities/run-brief';
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
  /** Persisted `meta.skipped_capabilities`: what this turn was configured with but could not serve. Live turns carry it in `responseMetadata`. */
  readonly skippedCapabilities?: unknown;
  /** Persisted `meta.grounding` (ADR-0043 decision 5): the grounding indicator of a citing agent's answer. Live turns carry it in `responseMetadata`. */
  readonly grounding?: unknown;
  /**
   * Client contract 1.47 (ADR-0050 decision 4): the temporary sub-agents a
   * cloud answer started, one lane each. Live turns fold `agent_subagent`
   * frames into it; a reload reads `meta.subagents_v1`.
   */
  readonly subAgents?: readonly CloudSubAgentLane[] | undefined;
  readonly resultChunk?: unknown;
  readonly assembledResult?: string | undefined;
  readonly continuedResultPrefix?: string | undefined;
  readonly persistedTrace?: unknown;
  readonly id: string;
  readonly role: string;
  /** The persisted assistant author name, independent of the composer selection. A persona's `display_name` wins over the agent's name. */
  readonly authorName?: string | undefined;
  /** The answering participant (`author_participant_id`), as a number; the key of its avatar state. */
  readonly authorParticipantId?: number | undefined;
  /** Who the row is addressed to, by name (`addressed_to`), for a side-talk label (ADR-0050 decision 7e). */
  readonly addresseeNames?: readonly string[] | undefined;
  /** The answer's rendering class (ADR-0050 decision 7e, `classifyMessage`). */
  readonly messageClass?: MessageClass | undefined;
  /** The author's persona `role` (client contract 1.38), shown beside the name. */
  readonly authorRole?: string | undefined;
  /** The author's icon URL (`icon_meta.url`); absent shows the brand mark. */
  readonly authorIconUrl?: string | undefined;
  /** The author's avatar pack (ADR-0050 decision 6), when the participant carries one. */
  readonly authorAvatarPack?: unknown;
  /** The answering agent's application id, for its memory routes (ADR-0050 decision 1b). */
  readonly authorApplicationId?: number | undefined;
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
  /** The run cap a CAP_REACHED answer stopped at (`meta.cap`, or the failure frame's `cap`). */
  readonly failureCap?: string;
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
  /** The run parked with open per-interrupt cards (`fanout_parked_v1`): not streaming, waiting for decisions. */
  readonly fanoutParked?: boolean | undefined;
  /**
   * Ledger keys this view saw resolved (decided here or in another tab). A
   * fan-out re-announces its open cards when it restores, before it applies
   * the decisions it fetched; a resolved key is never shown again.
   */
  readonly resolvedLedgerKeys?: readonly string[] | undefined;
  /**
   * The continue-button prompt shown when a turn stopped on the model's token
   * limit rather than on a completed answer. `ApplicationAnswer` reads it to
   * decide whether to render `ChatContinue`.
   */
  readonly requiresConfirmation?: { readonly message: string; readonly buttonText: string } | undefined;
  readonly threadId?: string | undefined;
  readonly taskId?: string | undefined;
  /**
   * The run a LIVE answer streams from, stamped by the transport (#1598) so
   * the chat can find the run's cloud browser. Kept apart from `taskId`,
   * which only the read rows set: chain follow reads a read row's `taskId`
   * differing from the live one as "this answer runs again".
   */
  readonly executionId?: string | undefined;
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
  /** Persisted `meta.continued_automatically` (ADR-0045): Main started this turn by itself in an unattended run chain. On an answer, set only when its question has no mark. */
  readonly continuedAutomatically?: boolean | undefined;
  /** The continuation's place in its chain (`meta.unattended.continuation_index`, 1 for the first), when known. */
  readonly continuationIndex?: number | undefined;
  /** Persisted `meta.step_limit_reached`: the answer stopped at its step limit in an unattended run. Live answers carry it in `responseMetadata`. */
  readonly stepLimitReached?: boolean | undefined;
  /** Persisted `meta.run_brief` (ADR-0045 decision 11): the chain's run brief, on the chain root's answer. */
  readonly runBrief?: RunBriefView | undefined;
  /** Persisted `meta.goal_check`: the goal check of this answer's run. */
  readonly goalCheck?: GoalCheckView | undefined;
}
