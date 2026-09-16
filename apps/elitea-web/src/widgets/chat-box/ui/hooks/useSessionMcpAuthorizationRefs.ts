/** Session-only delegated-authorization decisions and declined-server state. */
import type { RefObject } from 'react';
import { useEffect, useRef } from 'react';

import type { McpAuthorizationBatch } from './useChatBoxHandlers.authorization.types';

export interface SessionMcpAuthorizationRefs {
  readonly sessionDeclinedMcpServersRef: RefObject<Map<string, Record<string, unknown>>>;
  readonly sessionMcpAuthorizationBatchesRef: RefObject<Map<string, McpAuthorizationBatch>>;
}

export function useSessionMcpAuthorizationRefs(
  conversationUuid: string | undefined,
): SessionMcpAuthorizationRefs {
  const declined = useRef<Map<string, Record<string, unknown>>>(new Map());
  const batches = useRef<Map<string, McpAuthorizationBatch>>(new Map());
  useEffect(() => {
    declined.current = new Map();
    batches.current = new Map();
  }, [conversationUuid]);
  return {
    sessionDeclinedMcpServersRef: declined,
    sessionMcpAuthorizationBatchesRef: batches,
  };
}
