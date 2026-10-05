import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useSyncExternalStore } from 'react';

import { queryKeys } from '@/api/queries/keys';
import { TurnInputError, prepareTurn, type PreparedTurn, type TurnModel } from '@/api/queries/sendMessage';
import { log } from '@/lib/log';
import { handOffToOutbox, mustQueueBehindOutbox, queueSend, shouldQueue } from '@/sync/appOutbox';
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

export interface UseTurn {
  live: LiveTurn | null;
  /** Starts (or queues) a new turn. Resolves to an error text when the input cannot be sent. */
  send: (text: string) => Promise<string | null>;
  /** Re-sends the same body (same question_id) and replays the stream. */
  retry: () => void;
  reconnect: () => void;
  /** Forget a failed or settled turn. */
  dismiss: () => void;
  busy: boolean;
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
      void queryClient.invalidateQueries({ queryKey: queryKeys.conversation(projectId, conversationRef) });
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
    async (text: string): Promise<string | null> => {
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
            questionId: newQuestionId(),
          });
        } catch (error) {
          if (error instanceof TurnInputError) {
            return error.reason === 'too_large' ? 'This message is too long.' : 'This message cannot be sent.';
          }
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
      const refetch = () =>
        queryClient.invalidateQueries({ queryKey: queryKeys.conversation(projectId, conversationRef) });
      let next: TurnController;
      try {
        next = TurnController.create(
          { projectId, conversationUuid, participantId, model, userInput: text },
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
        if (error instanceof TurnInputError) {
          return error.reason === 'too_large' ? 'This message is too long.' : 'This message cannot be sent.';
        }
        throw error;
      }
      // The effect above starts it: a new turn is in phase 'sending'.
      setLiveTurn(key, next);
      return null;
    },
    [projectId, conversationUuid, participantId, model, key, queryClient, conversationRef],
  );

  const retry = useCallback(() => void controller?.reattach(), [controller]);
  const reconnect = useCallback(() => controller?.reconnect(), [controller]);
  const dismiss = useCallback(() => {
    if (key) setLiveTurn(key, null);
  }, [key]);

  const busy = live !== null && (live.turn.phase === 'sending' || live.turn.phase === 'streaming');
  return { live, send, retry, reconnect, dismiss, busy };
}
