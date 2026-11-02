/**
 * `canCreate` (`lib/createGate.ts`) bound to the live inputs: the person's
 * personal project (the router context's `auth.getUser()`, the seam the
 * bell and the tokens table read) and whether a new chat is being created
 * (`useChatSessionStore`).
 */
import { useCallback } from 'react';

import { useRouteContext } from '@tanstack/react-router';

import { useChatSessionStore } from '@/entities/conversation';
import { selectPersonalProjectId } from '@/shared/lib/personalProject';

import { canCreate } from '../lib/createGate';

/** Whether the row's "+" (or the new chat) is offered in the selected project. */
export function useCreateGate(permissions: ReadonlySet<string>, projectId: string | undefined): (value: string) => boolean {
  const routeContext: unknown = useRouteContext({ strict: false });
  const personalProjectId = selectPersonalProjectId(routeContext);
  const creatingChat = useChatSessionStore((state) => state.isCreatingNewConversation);
  return useCallback((value: string) => canCreate(value, { permissions, projectId, personalProjectId, creatingChat }), [permissions, projectId, personalProjectId, creatingChat]);
}
