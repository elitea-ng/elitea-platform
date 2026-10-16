/**
 * Session-only declined-server state. Authorization decisions themselves live
 * in `model/hitlDecisionQueue`, which survives a reload.
 */
import type { RefObject } from 'react';
import { useEffect, useRef } from 'react';

export interface SessionMcpAuthorizationRefs {
  readonly sessionDeclinedMcpServersRef: RefObject<Map<string, Record<string, unknown>>>;
}

export function useSessionMcpAuthorizationRefs(
  conversationUuid: string | undefined,
): SessionMcpAuthorizationRefs {
  const declined = useRef<Map<string, Record<string, unknown>>>(new Map());
  useEffect(() => {
    declined.current = new Map();
  }, [conversationUuid]);
  return { sessionDeclinedMcpServersRef: declined };
}
