/**
 * model/usePendingReplay.ts — the seeded message a reattach replays into (#6654).
 *
 * Split out of useChatStreamTransport (file-length budget). The seeded turn is
 * NOT cleared when the reattach starts. It is cleared in the same update that
 * applies the first replayed frame. A stream that never delivers one (403 for
 * a viewer without the chat permission, 404, a pruned log) then leaves the
 * seeded row as it was: still seed-owned, so the settled refetch can replace
 * it.
 */
import { useCallback, useMemo, useRef } from 'react';

import type { AgentExecutionStart } from '@/entities/conversation/api/conversationApi';

import { executionEventsPath } from '../lib/chatStreamReattach';

import type { ChatStreamReattachParams } from './useChatStreamTransport.types';

interface PendingReplay {
  /** Replay the next frame into this seeded message. */
  readonly arm: (messageId: string) => void;
  /** Take the target, once: the first frame of a reattach. */
  readonly take: () => string | undefined;
  /** Forget the target (the transport detached). */
  readonly clear: () => void;
  /**
   * Whether a reattach is still waiting for its first frame. A stream that
   * fails before it opens while this is true is a reattach this viewer cannot
   * stream (403) or one that is gone (404); a run this page started is not.
   */
  readonly isArmed: () => boolean;
}

export function usePendingReplay(): PendingReplay {
  const ref = useRef<string | undefined>(undefined);
  const arm = useCallback((messageId: string) => {
    ref.current = messageId;
  }, []);
  const take = useCallback((): string | undefined => {
    const messageId = ref.current;
    ref.current = undefined;
    return messageId;
  }, []);
  const clear = useCallback(() => {
    ref.current = undefined;
  }, []);
  const isArmed = useCallback(() => ref.current !== undefined, []);
  return useMemo(() => ({ arm, take, clear, isArmed }), [arm, take, clear, isArmed]);
}

type SubscribeToRun = (
  accepted: AgentExecutionStart,
  runConversationUuid: string,
  projectId: string | number,
  questionId?: string,
) => boolean;

/**
 * #6654: a reload mid-turn. The run exists and is durable, so observe it again
 * from cursor 0 into the seeded message; never start anything.
 */
export function useReattach(
  ownsRun: () => boolean,
  subscribeToRun: SubscribeToRun,
  pendingReplay: PendingReplay,
): (target: ChatStreamReattachParams) => boolean {
  return useCallback((target: ChatStreamReattachParams): boolean => {
    if (ownsRun()) return false;
    const eventsUrl = executionEventsPath(target.projectId, target.executionId);
    if (eventsUrl === undefined) return false;
    const subscribed = subscribeToRun({
      events_url: eventsUrl,
      response_message_id: target.responseMessageId,
    }, target.conversationUuid, target.projectId, target.questionId);
    // Only a run the transport now owns replays into the seeded row.
    if (subscribed && ownsRun()) pendingReplay.arm(target.responseMessageId);
    return subscribed;
  }, [ownsRun, subscribeToRun, pendingReplay]);
}
