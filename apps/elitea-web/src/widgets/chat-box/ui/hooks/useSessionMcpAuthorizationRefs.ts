/**
 * Session-only declined-server state. Authorization decisions themselves live
 * in `model/hitlDecisionQueue`, which survives a reload.
 *
 * When the conversation on screen CHANGES, the page-life memory behind the
 * queues (values too large to persist, held authorization cards) is dropped
 * too. Not on mount: the hydrating seed has just re-held the restored cards.
 * A logout is a full navigation, which ends that memory with the document,
 * and `clearNamespace()` sweeps the persisted half.
 */
import type { RefObject } from 'react';
import { useEffect, useRef } from 'react';

import { clearHitlDecisionMemory } from '../../model/hitlDecisionQueue';
import { clearHeldAuthorizationCards } from './useChatBoxHandlers.authorization';

export interface SessionMcpAuthorizationRefs {
  readonly sessionDeclinedMcpServersRef: RefObject<Map<string, Record<string, unknown>>>;
}

export function useSessionMcpAuthorizationRefs(
  conversationUuid: string | undefined,
): SessionMcpAuthorizationRefs {
  const declined = useRef<Map<string, Record<string, unknown>>>(new Map());
  const previous = useRef(conversationUuid);
  useEffect(() => {
    declined.current = new Map();
    if (previous.current === conversationUuid) return;
    previous.current = conversationUuid;
    clearHitlDecisionMemory();
    clearHeldAuthorizationCards();
  }, [conversationUuid]);
  return { sessionDeclinedMcpServersRef: declined };
}
