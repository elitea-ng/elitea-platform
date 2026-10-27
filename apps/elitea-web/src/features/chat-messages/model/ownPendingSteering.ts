/**
 * The steering messages this tab sent into a running turn of the conversation that still wait for the agent
 * (`pending`), oldest first: the rows of the composer's queue (`widgets/chat-box`'s `ChatBoxComposerQueue`), with
 * Send now, Edit and Remove. Only this tab's own messages (`own`); someone else's show under the answer
 * (`SteeringMessages`). None while no turn of the chat runs: the end-of-turn rule decides about the rest.
 */
import { useMemo } from 'react';

import { STEERING_STATE, type SteeringItem } from '@elitea/client-core/stream';

import { useChatSteeringStore } from './chatSteering.store';

const NONE: readonly SteeringItem[] = [];

export function useOwnPendingSteering(conversationKey: string, isStreaming: boolean): readonly SteeringItem[] {
  const own = useChatSteeringStore((state) => state.own);
  const items = useChatSteeringStore((state) => state.items);
  return useMemo(() => {
    if (!isStreaming) return NONE;
    const pending = Object.entries(own)
      .filter(([, record]) => record.conversationKey === conversationKey)
      .flatMap(([responseId, record]) => (items[responseId] ?? []).filter((item) => item.state === STEERING_STATE.Pending && record.ids.includes(item.id)));
    return pending.length === 0 ? NONE : pending;
  }, [own, items, conversationKey, isStreaming]);
}
