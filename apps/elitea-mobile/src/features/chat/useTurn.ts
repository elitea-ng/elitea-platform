import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useSyncExternalStore } from 'react';

import { queryKeys } from '@/api/queries/keys';
import type { components } from '@/api/generated/elitea.gen';
import {
  TurnInputError,
  prepareTurn,
  type ContinueContract,
  type PreparedTurn,
  type TurnAttachment,
  type TurnMentions,
  type TurnModel,
} from '@/api/queries/sendMessage';
import { stopErrorText } from '@/api/queries/stopTurn';
import { answerRegenerated } from '@/features/chat/messageModel';
import { log } from '@/lib/log';
import { network } from '@/lib/network';
import { handOffToOutbox, mustQueueBehindOutbox, queueSend, shouldQueue } from '@/sync/appOutbox';
import { isOfflineCopy } from '@/sync/offlineReads';
import {
  TurnController,
  newQuestionId,
  getLiveTurn,
  setLiveTurn,
  subscribeLiveTurns,
  turnKey,
  type LiveTurn,
} from '@/features/chat/turnController';

const noopSubscribe = () => () => {};
const nullSnapshot = () => null;

export interface UseTurnArgs {
  projectId: number | null;
  /** Route reference the conversation query is keyed by. */
  conversationRef: string;
  /** The conversation's uuid (from the read), needed for sending. */
  conversationUuid: string | null;
  /**
   * The addressed participant: the single agent, or the model participant (0
   * when the conversation has none); null when sending is not possible.
   */
  participantId: number | null;
  /** The model of a plain model conversation (adhoc turn); null for an agent. */
  model?: TurnModel | null;
}

/** What a message carries besides its text. */
export interface SendExtras {
  mentions?: TurnMentions | null;
  attachments?: TurnAttachment[] | null;
  /** The question_id to use (minted earlier, e.g. before uploading its files). */
  questionId?: string;
}

/** The composer's text for an input refusal. */
export function inputErrorText(error: TurnInputError): string {
  switch (error.reason) {
    case 'too_large':
      return 'This message is too long.';
    case 'mentions':
      return 'This message mentions too many people.';
    case 'attachments':
      return 'The attached files could not be added to this message.';
    default:
      return 'This message cannot be sent.';
  }
}

export interface UseTurn {
  live: LiveTurn | null;
  /** Starts (or queues) a new turn. Resolves to an error text when the input cannot be sent. */
  send: (text: string, extras?: SendExtras) => Promise<string | null>;
  /**
   * Answers the paused answer and streams the rest of it. Online only and
   * never queued; resolves to an error text when it cannot start.
   */
  continueWith: (contract: ContinueContract, body: components['schemas']['ChatContinueRequest']) => Promise<string | null>;
  /** Re-sends the same body (same question_id) and replays the stream. */
  retry: () => void;
  reconnect: () => void;
  /** Forget a failed or settled turn. */
  dismiss: () => void;
  busy: boolean;
  /**
   * Stops the streaming answer on the server (client contract 1.3; the
   * caller checks the contract). See `StopResult`.
   */
  stop: () => Promise<StopResult>;
  /** The answer streams and is admitted, so it can be stopped now. */
  canStop: boolean;
  /**
   * Whether the live turn is question `questionId` and still sending or
   * streaming (read now, not from the last render): false for a send that
   * was queued for the outbox or handed off to it.
   */
  isRunning: (questionId: string) => boolean;
  /**
   * Regenerates the newest answer in place and streams the new one. Online
   * only, never queued; resolves to an error text when it cannot start.
   */
  regenerate: (target: RegenerateTarget) => Promise<string | null>;
}

/**
 * What a Stop came to: `problem` is an error text, or null; `restoreText` is
 * the user's message when the server removed it with the stopped answer (a
 * run stopped before any output is removed together with its question,
 * cancelChatExecution), so the composer can hand it back instead of the
 * message silently disappearing.
 */
export interface StopResult {
  problem: string | null;
  restoreText: string | null;
  /**
   * The names of the files that went with a removed question. The screen
   * puts back the ones the tray still holds (`restoredAttachments`) and
   * leaves here only those the composer must ask to attach again. Empty
   * otherwise.
   */
  lostAttachments: string[];
  /**
   * The names of the files the screen put back in the tray with the removed
   * question (it kept their local copies until the answer settled, #29);
   * `lostAttachments` then names only the ones it could not.
   */
  restoredAttachments?: string[];
}

/** The text a message with files and no words is sent with (the server needs some). */
export const ATTACHMENT_ONLY_TEXT = '(attachment)';

/** The persisted answer to regenerate and the question it answers. */
export interface RegenerateTarget {
  answerUuid: string;
  questionUuid: string;
  questionText: string;
  /** The answer's author participant; 0 lets the server use the answer's own. */
  participantId: number;
  /**
   * After the new answer settled and the transcript was read again: true when
   * the persisted answer is the regenerated one (`answerRegenerated`), false
   * when the workspace's fresh transcript still holds the old one, null when
   * the transcript could not be read again (nothing is known).
   */
  onSettled?: (replaced: boolean | null) => void;
}

/**
 * Reads a conversation again once a turn has settled: its messages, and the
 * persisted tool calls. The tool history is keyed by message group, and a
 * continuation (an approval, an answer, a sign-in skip, "continue") adds calls
 * to the group the paused answer already had, so without this the answer
 * keeps showing the calls read while it was paused.
 */
export async function refreshTranscript(queryClient: QueryClient, projectId: number, conversationRef: string): Promise<void> {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: queryKeys.conversation(projectId, conversationRef) }),
    queryClient.invalidateQueries({ queryKey: queryKeys.traces(projectId) }),
  ]);
}

/**
 * The conversation as the WORKSPACE answered it at or after `since`, or null
 * when the cache cannot say: the refetch failed (React Query keeps the old
 * data and never throws from invalidateQueries), did not run (no observer),
 * or answered with the encrypted device copy (offline fallback). Anything
 * concluded from what the transcript lacks ("the server removed the
 * question", "the answer was not replaced") must come from this, never from
 * whatever happens to be cached.
 */
export function freshConversation<T>(queryClient: QueryClient, projectId: number, conversationRef: string, since: number): T | null {
  const state = queryClient.getQueryState(queryKeys.conversation(projectId, conversationRef));
  if (!state || state.status !== 'success' || state.dataUpdatedAt < since) return null;
  if (state.data === undefined || state.data === null || isOfflineCopy(state.data)) return null;
  return state.data as T;
}

/**
 * The thread's live turn, kept in the live-turn registry (memory). Mounting
 * the thread starts or re-attaches an unfinished turn (re-POST of the same
 * body, stream replayed from the start); unmounting stops its network work
 * but keeps the turn for the next mount.
 */
export function useTurn({ projectId, conversationRef, conversationUuid, participantId, model = null }: UseTurnArgs): UseTurn {
  const queryClient = useQueryClient();
  const key = projectId !== null && conversationUuid ? turnKey(projectId, conversationUuid) : null;

  const getController = useCallback(() => (key ? (getLiveTurn(key) ?? null) : null), [key]);
  const controller = useSyncExternalStore(subscribeLiveTurns, getController, getController);

  useEffect(() => {
    if (!controller || key === null || projectId === null) return;
    const snapshot = controller.getSnapshot();
    const phase = snapshot.turn.phase;
    if (snapshot.settled || phase === 'done' || phase === 'paused') {
      // Ended while the thread was away: the persisted copy replaces it.
      void refreshTranscript(queryClient, projectId, conversationRef);
      setLiveTurn(key, null);
      return;
    }
    if (phase === 'sending' || phase === 'streaming') void controller.reattach();
    return () => controller.detach();
    // Runs per controller: a new turn, or the thread (re)mounting.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [controller]);

  const live = useSyncExternalStore(
    controller ? controller.subscribe : noopSubscribe,
    controller ? controller.getSnapshot : nullSnapshot,
    controller ? controller.getSnapshot : nullSnapshot,
  );

  const send = useCallback(
    async (text: string, extras: SendExtras = {}): Promise<string | null> => {
      const fields = {
        mentions: extras.mentions && (extras.mentions.userIds.length > 0 || extras.mentions.everyone) ? extras.mentions : null,
        attachments: extras.attachments && extras.attachments.length > 0 ? extras.attachments : null,
      };
      if (projectId === null || !conversationUuid || participantId === null || key === null) {
        return 'This conversation cannot be messaged from mobile.';
      }
      if (shouldQueue() || (await mustQueueBehindOutbox(projectId, conversationUuid))) {
        // Offline with the encrypted store open, or an earlier message of this
        // conversation still waits in the outbox (strict order: a live send
        // would overtake it): queue it (question_id minted now, exact body
        // stored); the drain sends it after the earlier ones.
        let prepared: PreparedTurn;
        try {
          prepared = prepareTurn({
            projectId,
            conversationUuid,
            participantId,
            model,
            userInput: text,
            ...fields,
            questionId: extras.questionId ?? newQuestionId(),
          });
        } catch (error) {
          if (error instanceof TurnInputError) return inputErrorText(error);
          throw error;
        }
        try {
          await queueSend(prepared, conversationRef);
        } catch (error) {
          // Nothing was stored: say so, so the Composer keeps the text.
          log.warn('could not queue the message', { error });
          return 'This message could not be saved to send later. Try again.';
        }
        return null;
      }
      const refetch = () => refreshTranscript(queryClient, projectId, conversationRef);
      let next: TurnController;
      try {
        next = TurnController.create(
          { projectId, conversationUuid, participantId, model, userInput: text, ...fields, questionId: extras.questionId },
          {
            onSettle: refetch,
            onRefreshNeeded: () => void refetch(),
            // The workspace cannot be reached: the outbox shows and retries it.
            handOff: async (prepared, hint) => {
              const queued = await handOffToOutbox(prepared, conversationRef, hint);
              if (queued && getLiveTurn(key) === next) setLiveTurn(key, null);
              return queued;
            },
          },
        );
      } catch (error) {
        if (error instanceof TurnInputError) return inputErrorText(error);
        throw error;
      }
      // The effect above starts it: a new turn is in phase 'sending'.
      setLiveTurn(key, next);
      return null;
    },
    [projectId, conversationUuid, participantId, model, key, queryClient, conversationRef],
  );

  const continueWith = useCallback(
    async (contract: ContinueContract, body: components['schemas']['ChatContinueRequest']): Promise<string | null> => {
      if (projectId === null || !conversationUuid || key === null) return 'This conversation cannot be continued from mobile.';
      if (!network.isOnline()) return "You're offline.";
      const current = getLiveTurn(key);
      if (current?.isActive) return 'Wait for the current answer to finish.';
      const refetch = () => refreshTranscript(queryClient, projectId, conversationRef);
      const next = TurnController.continuation({ projectId, conversationUuid }, contract, body, {
        onSettle: refetch,
        onRefreshNeeded: () => void refetch(),
        // Someone answered it first: the transcript shows what happened.
        onAlreadyResolved: () => void refetch(),
      });
      setLiveTurn(key, next);
      return null;
    },
    [projectId, conversationUuid, key, queryClient, conversationRef],
  );

  const regenerate = useCallback(
    async (target: RegenerateTarget): Promise<string | null> => {
      if (projectId === null || !conversationUuid || key === null) return 'This answer cannot be regenerated from mobile.';
      if (!network.isOnline()) return "You're offline.";
      if (getLiveTurn(key)?.isActive) return 'Wait for the current answer to finish.';
      const refetch = () => refreshTranscript(queryClient, projectId, conversationRef);
      const { onSettled, ...request } = target;
      let next: TurnController;
      try {
        next = TurnController.regeneration(
          { projectId, conversationUuid, model, ...request },
          {
            onSettle: async () => {
              const since = Date.now();
              await refetch();
              const detail = freshConversation<Parameters<typeof answerRegenerated>[0]>(queryClient, projectId, conversationRef, since);
              const regeneration = next.getSnapshot().regeneration;
              if (regeneration) onSettled?.(detail === null ? null : answerRegenerated(detail, regeneration.answerUuid, regeneration.regenerationId));
            },
            onRefreshNeeded: () => void refetch(),
          },
        );
      } catch (error) {
        if (error instanceof TurnInputError) return 'This answer cannot be regenerated from mobile.';
        throw error;
      }
      setLiveTurn(key, next);
      return null;
    },
    [projectId, conversationUuid, key, model, queryClient, conversationRef],
  );

  const retry = useCallback(() => void controller?.reattach(), [controller]);
  const reconnect = useCallback(() => controller?.reconnect(), [controller]);
  const dismiss = useCallback(() => {
    if (key) setLiveTurn(key, null);
  }, [key]);

  const stop = useCallback(async (): Promise<StopResult> => {
    const result = (problem: string | null, restoreText: string | null = null, lostAttachments: string[] = []): StopResult => ({
      problem,
      restoreText,
      lostAttachments,
    });
    if (!controller) return result(null);
    if (!network.isOnline()) return result("You're offline. The answer keeps running on the workspace.");
    const before = controller.getSnapshot();
    const since = Date.now();
    try {
      const outcome = await controller.stop();
      if (outcome === 'not_ready') return result('The answer has not started yet. Try again in a moment.');
      if (outcome !== 'stopped' || projectId === null) return result(null);
      // The transcript was read again by the stop: is the question still there?
      // Only the workspace's fresh answer can say it is gone.
      const files = before.attachmentNames ?? [];
      const ownMessage = !before.continuation && before.regeneration === null && (before.userText.trim() !== '' || files.length > 0);
      if (!ownMessage) return result(null);
      const detail = freshConversation<{ message_groups?: unknown }>(queryClient, projectId, conversationRef, since);
      const groups = Array.isArray(detail?.message_groups) ? (detail.message_groups as { uuid?: unknown }[]) : null;
      const removed = groups !== null && !groups.some((group) => group?.uuid === before.questionId);
      if (!removed) return result(null);
      const text = files.length > 0 && before.userText === ATTACHMENT_ONLY_TEXT ? '' : before.userText;
      return result(null, text, [...files]);
    } catch (error) {
      log.info('stop failed', { error });
      return result(stopErrorText(error));
    }
  }, [controller, projectId, queryClient, conversationRef]);

  const busy = live !== null && (live.turn.phase === 'sending' || live.turn.phase === 'streaming');
  // A regeneration stopped before its first output is removed on the server
  // with its question, which deletes the original exchange (the question and
  // the old answer): it can be stopped only once it has written something.
  const regenerationUnstoppable = live !== null && live.regeneration !== null && live.turn.content === '';
  const canStop =
    live !== null &&
    !live.queued &&
    live.turn.phase === 'streaming' &&
    (live.execution?.responseMessageId ?? null) !== null &&
    !regenerationUnstoppable;
  const isRunning = useCallback(
    (questionId: string) => {
      const current = key ? getLiveTurn(key) : undefined;
      if (!current?.isActive) return false;
      return current.getSnapshot().questionId === questionId;
    },
    [key],
  );
  return { live, send, continueWith, retry, reconnect, dismiss, busy, stop, canStop, isRunning, regenerate };
}
