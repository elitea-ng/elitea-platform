/**
 * `canCreate` (`lib/createGate.ts`) bound to the live inputs: the person's
 * personal project (the router context's `auth.getUser()`, the seam the
 * bell and the tokens table read) and whether a new chat is being created
 * (`useChatSessionStore`).
 */
import { useCallback } from 'react';

import { useRouteContext } from '@tanstack/react-router';

import { useChatSessionStore } from '@/entities/conversation';

import { canCreate } from '../lib/createGate';

interface PersonalProjectIdContext {
  readonly auth?: { readonly getUser?: () => { readonly personal_project_id?: string | number } | undefined };
}

function selectPersonalProjectId(context: unknown): string | undefined {
  if (typeof context !== 'object' || context === null) return undefined;
  const id = (context as PersonalProjectIdContext).auth?.getUser?.()?.personal_project_id;
  return id === undefined ? undefined : String(id);
}

/** Whether the row's "+" (or the new chat) is offered in the selected project. */
export function useCreateGate(permissions: ReadonlySet<string>, projectId: string | undefined): (value: string) => boolean {
  const routeContext: unknown = useRouteContext({ strict: false });
  const personalProjectId = selectPersonalProjectId(routeContext);
  const creatingChat = useChatSessionStore((state) => state.isCreatingNewConversation);
  return useCallback((value: string) => canCreate(value, { permissions, projectId, personalProjectId, creatingChat }), [permissions, projectId, personalProjectId, creatingChat]);
}
