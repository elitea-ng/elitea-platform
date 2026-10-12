/**
 * Small helpers lifted out of `useConversationSidebar.ts`, which is at the
 * §3.5 400-line file budget. None has any React in it.
 */
import { getConfig } from '@/shared/config';
import { isLocalWorkConversation } from '@/shared/lib/localWork';

/**
 * The SPA's mount point, for the absolute share link `ConversationItem` copies
 * to the clipboard. Without it, `ConversationItem` fell back to `basename: ''`
 * and Share copied `https://host/{projectId}/chat/{id}?...` — a hard 404 for the
 * recipient in every deployment where `vite_base_uri` is not `/`, because only
 * `/app/**` is served by the SPA. Dev hid it: `import.meta.env.DEV` resolves the
 * basename to `''` there.
 *
 * Resolved locally against `shared/config` rather than imported from
 * `app/providers/basename.ts`: `.dependency-cruiser.cjs`'s
 * `no-upward-from-processes` forbids `processes/` -> `app/`. Four other slices
 * carry the same 3-line copy for the same reason (`features/agents/lib/
 * basename.ts`, `features/notifications/lib/routes.ts`, `features/mcps/lib/
 * oauthFlow.ts`, `routes/$projectId.$.tsx`).
 *
 * The trailing slash is trimmed: `vite_base_uri` is `/app/` and
 * `ConversationItem` already adds a leading `/`, so keeping it produces
 * `https://host/app//5/chat/...`, whose doubled slash survives the router's
 * basepath strip.
 */
export function chatBasename(): string {
  if (import.meta.env.DEV) return '';
  const result = getConfig();
  return result.status === 'ok' ? result.config.vite_base_uri.replace(/\/$/, '') : '';
}

/**
 * A locally-unique id for the not-yet-persisted draft folder the "Create
 * folder" button pushes into `folders`. `crypto.randomUUID` is the baseline's
 * `uuidv4()` (`NewChat.jsx:1231`) without the dependency; it is available in
 * every browser this app supports and in jsdom under Node 20+.
 */
export function draftFolderId(): string {
  return `draft-${crypto.randomUUID()}`;
}


/**
 * In the desktop build, the folder on this computer that ran a Local work
 * thread, so a click on its Chats row opens it there (where it can be
 * continued) instead of the read-only chat page. `null` everywhere else: in
 * the web build (the `MODE` check is a build-time literal, so the dynamic
 * import is dropped), for an ordinary chat, and when no folder here matches.
 */
export async function desktopLocalThreadFor(conversation: { readonly id: string; readonly source?: string }): Promise<string | null> {
  if (import.meta.env.MODE !== 'desktop' || !isLocalWorkConversation(conversation)) return null;
  const { findLocalWorkThread } = await import('@/pages/workspace/localThreadLookup');
  return findLocalWorkThread(conversation.id).catch(() => null);
}
