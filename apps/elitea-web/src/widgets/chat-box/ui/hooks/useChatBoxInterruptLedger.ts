/**
 * hooks/useChatBoxInterruptLedger.ts — the chat box's half of live
 * per-interrupt resolution (`features/chat-messages` `useInterruptLedger`):
 * a woken fan-out execution is attached to the SAME response row, appended
 * rather than replayed, with the parked state cleared first.
 */
import { useCallback } from 'react';

import { useInterruptLedger } from '@/features/chat-messages';
import type { ChatMessage, ReattachableTurn } from '@/features/chat-messages';

export interface UseChatBoxInterruptLedgerParams {
  readonly projectId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly messages: readonly ChatMessage[];
  readonly setChatHistory: (update: (previous: readonly ChatMessage[]) => readonly ChatMessage[]) => void;
  readonly reattach: (turn: ReattachableTurn) => boolean;
}

export function useChatBoxInterruptLedger({ reattach, setChatHistory, ...params }: UseChatBoxInterruptLedgerParams): void {
  const attachWake = useCallback(({ responseMessageId, executionId }: { readonly responseMessageId: string; readonly executionId: string }): boolean => {
    const attached = reattach({ messageId: responseMessageId, executionId, replay: 'append' });
    if (attached) {
      // The woken execution has its own generation: the parked one's must not
      // filter its frames out, and the row streams again.
      setChatHistory((previous) => previous.map((message) => (message.id === responseMessageId
        ? { ...message, fanoutParked: false, executionGeneration: undefined, isStreaming: true, isLoading: true }
        : message)));
    }
    return attached;
  }, [reattach, setChatHistory]);
  useInterruptLedger({ ...params, setChatHistory, attachWake });
}
