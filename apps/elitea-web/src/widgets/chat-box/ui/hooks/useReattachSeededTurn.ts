/**
 * Reopen the stream of a turn that was in flight when this conversation was
 * loaded (#6654). One attempt per conversation and execution: the transport
 * replays the whole durable log, so a second attempt would only repeat it.
 */
import { useEffect, useMemo, useRef } from 'react';

import { findReattachableTurn } from '@/features/chat-messages';
import type { ChatMessage, ReattachableTurn } from '@/features/chat-messages';

export interface UseReattachSeededTurnParams {
  readonly messages: readonly ChatMessage[];
  readonly conversationUuid: string | undefined;
  readonly reattach: (turn: ReattachableTurn) => boolean;
}

export function useReattachSeededTurn({ messages, conversationUuid, reattach }: UseReattachSeededTurnParams): void {
  const attempted = useRef<Set<string>>(new Set());
  const turn = useMemo(() => findReattachableTurn(messages), [messages]);
  const executionId = turn?.executionId;
  const messageId = turn?.messageId;
  const questionId = turn?.questionId;

  useEffect(() => {
    if (conversationUuid === undefined || executionId === undefined || messageId === undefined) return;
    const key = `${conversationUuid}:${executionId}`;
    if (attempted.current.has(key)) return;
    attempted.current.add(key);
    reattach({ executionId, messageId, ...(questionId !== undefined ? { questionId } : {}) });
  }, [conversationUuid, executionId, messageId, questionId, reattach]);
}
