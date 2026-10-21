import type { InfiniteData, QueryClient } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { ConversationPage } from '@/api/queries/conversations';
import { queryKeys } from '@/api/queries/keys';
import { setConversationRowUnread } from '@/db/repo';
import type { Db } from '@/db/sql';
import { log } from '@/lib/log';

/**
 * Read markers (client contract 1.12, `markConversationRead`): the CALLER's marker of a
 * conversation, which the list rows' `unread` compares the newest answer with. The thread marks
 * itself read when it opens and when an answer lands while it is on screen
 * (src/features/chat/useMarkConversationRead.ts).
 *
 * The read does not re-deliver the row in the `changes_since` delta, so the device writes the
 * new `unread` into its own copies: the loaded list pages at once (before the request), and the
 * offline store's row once the workspace answered.
 */

/** Moves the caller's marker up to now; resolves the row's `unread` after it. Throws `ApiError`. */
export async function markConversationRead(projectId: number, conversationId: number, client: ApiClient = sessionApiClient()): Promise<{ unread: boolean }> {
  const state = await unwrap(
    client.PUT('/elitea_core/conversation_read/prompt_lib/{project_id}/{conversation_id}', {
      params: { path: { project_id: String(projectId), conversation_id: String(conversationId) } },
    }),
  );
  return { unread: state?.unread === true };
}

/** Sets `unread` on the conversation's row in the loaded Chats and Local work pages. */
export function patchConversationUnread(client: QueryClient, projectId: number, conversationId: number, unread: boolean): void {
  for (const key of [queryKeys.conversations(projectId), queryKeys.localWork(projectId)]) {
    client.setQueryData<InfiniteData<ConversationPage, number>>(key, (data) => {
      if (!data || !data.pages.some((page) => page.rows.some((row) => row.id === conversationId && row.unread !== unread))) return data;
      return {
        ...data,
        pages: data.pages.map((page) => ({ ...page, rows: page.rows.map((row) => (row.id === conversationId ? { ...row, unread } : row)) })),
      };
    });
  }
}

export interface MarkReadDeps {
  queryClient: QueryClient;
  getDb: () => Db | null;
  client?: ApiClient;
}

/**
 * Marks the conversation read: the row turns read in the list at once, then the workspace is
 * asked; its answer is written to the list and the offline copy. When the request fails the
 * list is read again, so the row shows the workspace's truth. Never throws; true when the
 * workspace took the read.
 */
export async function runMarkConversationRead(projectId: number, conversationId: number, deps: MarkReadDeps): Promise<boolean> {
  patchConversationUnread(deps.queryClient, projectId, conversationId, false);
  try {
    const { unread } = await markConversationRead(projectId, conversationId, deps.client);
    if (unread) patchConversationUnread(deps.queryClient, projectId, conversationId, true);
    const db = deps.getDb();
    if (db !== null) await setConversationRowUnread(db, projectId, conversationId, unread);
    return true;
  } catch (error) {
    log.info('mark conversation read failed', { error });
    void deps.queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
    void deps.queryClient.invalidateQueries({ queryKey: queryKeys.localWork(projectId) });
    return false;
  }
}
