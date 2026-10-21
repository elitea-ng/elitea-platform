import type { QueryClient } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { isApiError, unwrap } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';
import { setRowJsonField } from '@/db/repo';
import type { Db } from '@/db/sql';
import { log } from '@/lib/log';
import { patchConversationPages } from '@/sync/conversationPages';

/**
 * Read markers (client contract 1.12, `markConversationRead`): the CALLER's marker of a
 * conversation, which the list rows' `unread` compares the newest answer with. The thread marks
 * itself read when it opens and when an answer lands while it is on screen
 * (src/features/chat/useMarkConversationRead.ts).
 *
 * The read does not re-deliver the row in the `changes_since` delta, so the device writes the
 * new `unread` into its own copies: the loaded list pages at once (before the request), and both
 * the pages and the offline store's row once the workspace answered.
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

/**
 * How a read ended:
 * - `read`: the workspace took it; `unread` is what it says now (true when an answer landed
 *   after the marker, or the read landed before the answer was stored);
 * - `refused`: it never will for this conversation (403, 404, 410, 426, another 4xx, the
 *   session gone): the thread stops asking;
 * - `failed`: try again later (no answer, a timeout, 408, 425, 429, 5xx).
 */
export type MarkReadOutcome = { kind: 'read'; unread: boolean } | { kind: 'refused' } | { kind: 'failed' };

/** True when sending the read again cannot succeed (it is refused for good, or the app must update). */
export function isPermanentReadFailure(error: unknown): boolean {
  if (!isApiError(error)) return false;
  switch (error.kind) {
    case 'http': {
      const status = error.status ?? 0;
      return status >= 400 && status < 500 && status !== 408 && status !== 425 && status !== 429;
    }
    case 'upgrade_required':
    case 'session_invalid':
    case 'origin_mismatch':
      return true;
    default:
      return false;
  }
}

/** A transient failure the lists are read again for (a cancelled request, or a session ending, is not). */
function refetchAfter(error: unknown): boolean {
  return !isApiError(error) || error.kind === 'network' || error.kind === 'timeout' || error.kind === 'http';
}

/** How long failed reads wait before the lists are read again, so a burst of failures reads them once. */
export const MARK_READ_REFETCH_DEBOUNCE_MS = 2000;

const pendingRefetch = new WeakMap<QueryClient, Set<number>>();

/** Reads the Chats and Local work lists again, once for every failure within the debounce. */
function refetchListsSoon(client: QueryClient, projectId: number): void {
  const pending = pendingRefetch.get(client) ?? new Set<number>();
  pendingRefetch.set(client, pending);
  if (pending.has(projectId)) return;
  pending.add(projectId);
  setTimeout(() => {
    pending.delete(projectId);
    void client.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
    void client.invalidateQueries({ queryKey: queryKeys.localWork(projectId) });
  }, MARK_READ_REFETCH_DEBOUNCE_MS);
}

export interface MarkReadDeps {
  queryClient: QueryClient;
  getDb: () => Db | null;
  client?: ApiClient;
}

/**
 * Marks the conversation read: a list read in flight is cancelled (its answer, from before the
 * read, would turn the row unread again), the row turns read in the list at once, then the
 * workspace is asked; its answer is written to the list and the offline copy, and a cancelled
 * list read is sent again. A transient failure reads the lists again (once per burst), so the
 * row shows the workspace's truth; a refusal does not (nothing changed). Never throws.
 */
export async function runMarkConversationRead(projectId: number, conversationId: number, deps: MarkReadDeps): Promise<MarkReadOutcome> {
  const { queryClient } = deps;
  const lists = [queryKeys.conversations(projectId), queryKeys.localWork(projectId)];
  const cancelled = lists.filter((queryKey) => queryClient.isFetching({ queryKey }) > 0);
  await Promise.all(cancelled.map((queryKey) => queryClient.cancelQueries({ queryKey })));
  patchConversationPages(queryClient, projectId, conversationId, { kind: 'unread', unread: false });
  let unread: boolean;
  try {
    ({ unread } = await markConversationRead(projectId, conversationId, deps.client));
  } catch (error) {
    log.info('mark conversation read failed', { error });
    const permanent = isPermanentReadFailure(error);
    if (!permanent && refetchAfter(error)) refetchListsSoon(queryClient, projectId);
    // A list read cancelled for nothing is sent again.
    else for (const queryKey of cancelled) void queryClient.invalidateQueries({ queryKey });
    return { kind: permanent ? 'refused' : 'failed' };
  }
  // Always the workspace's answer: a list read that landed meanwhile may say otherwise.
  patchConversationPages(queryClient, projectId, conversationId, { kind: 'unread', unread });
  for (const queryKey of cancelled) void queryClient.invalidateQueries({ queryKey });
  const db = deps.getDb();
  try {
    if (db !== null) await setRowJsonField(db, projectId, conversationId, 'unread', unread);
  } catch (error) {
    log.warn('offline copy: read marker not saved', { error });
  }
  return { kind: 'read', unread };
}
