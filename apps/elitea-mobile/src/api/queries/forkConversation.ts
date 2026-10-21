import type { InfiniteData, QueryClient } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { isApiError, unwrap } from '@/api/errors';
import type { ConversationPage, ConversationSummary } from '@/api/queries/conversations';
import { queryKeys } from '@/api/queries/keys';
import { isOfflineError } from '@/features/chats/offlineEmpty';
import { network } from '@/lib/network';
import { syncManager } from '@/sync/appSync';

/**
 * Fork a conversation into a new one (client contract 1.11, `forkConversation`), as the web app
 * does: the whole history ("Fork" on a Chats row), or the history up to and including one
 * message ("Fork from here" on a settled message). The server copies the history in one
 * transaction and answers the new list row (201); its default name is the source's with
 * " (fork)" added.
 *
 * A fork needs the workspace: it is never queued offline (the copy is the server's, and the
 * screen opens the new chat at once). Offline, nothing is sent and the screen says so.
 */

export const FORK_COPY = {
  offline: "You're offline. Forking a chat needs a connection to your workspace.",
  unsettled: 'Wait for the reply to finish, then fork again.',
  notSupported: "This chat can't be forked.",
  notFound: 'This conversation or message is no longer available.',
  forbidden: "You can't start chats in this project.",
  failed: "Couldn't fork the chat. Try again.",
} as const;

export interface ForkOptions {
  /** The fork point: a message group's UUID or numeric id. Absent forks the whole conversation. */
  messageId?: string;
  name?: string;
}

/** Sends the fork; throws `ApiError` on a refusal or no answer. */
export async function forkConversation(
  projectId: number,
  conversationId: number | string,
  options: ForkOptions = {},
  client: ApiClient = sessionApiClient(),
): Promise<ConversationSummary> {
  const body = {
    ...(options.messageId !== undefined ? { message_id: options.messageId } : {}),
    ...(options.name !== undefined ? { name: options.name } : {}),
  };
  const row = await unwrap(
    client.POST('/elitea_core/conversation_fork/prompt_lib/{project_id}/{conversation_id}', {
      params: { path: { project_id: String(projectId), conversation_id: String(conversationId) } },
      body,
    }),
  );
  return row as ConversationSummary;
}

/** What to tell the user when a fork did not go through. */
export function forkErrorText(error: unknown): string {
  if (isOfflineError(error)) return FORK_COPY.offline;
  if (!isApiError(error) || error.kind !== 'http') return FORK_COPY.failed;
  if (error.status === 409) return error.code === 'fork_not_supported' ? FORK_COPY.notSupported : FORK_COPY.unsettled;
  if (error.status === 404) return FORK_COPY.notFound;
  if (error.status === 403) return FORK_COPY.forbidden;
  return FORK_COPY.failed;
}

export type ForkResult = { ok: true; row: ConversationSummary } | { ok: false; message: string };

/**
 * Forks and brings the Chats list in line: the new row goes on top of the loaded first page
 * (the list shows it before the refetch lands), and the offline copy is asked to sync. Offline
 * (the device knows it), nothing is sent. Never throws.
 */
export async function runFork(
  client: QueryClient,
  projectId: number,
  conversationId: number | string,
  options: ForkOptions = {},
  api?: ApiClient,
  isOnline: () => boolean = network.isOnline,
): Promise<ForkResult> {
  if (!isOnline()) return { ok: false, message: FORK_COPY.offline };
  let row: ConversationSummary;
  try {
    row = await forkConversation(projectId, conversationId, options, api);
  } catch (error) {
    return { ok: false, message: forkErrorText(error) };
  }
  if (!Number.isSafeInteger(row?.id) || row.id <= 0) return { ok: false, message: FORK_COPY.failed };
  client.setQueryData<InfiniteData<ConversationPage, number>>(queryKeys.conversations(projectId), (data) => {
    if (!data || data.pages.length === 0 || data.pages.some((page) => page.rows.some((r) => r.id === row.id))) return data;
    const [first, ...rest] = data.pages;
    return { ...data, pages: [{ ...first, rows: [row, ...first.rows], total: first.total + 1 }, ...rest] };
  });
  void client.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
  syncManager.requestSync(projectId);
  return { ok: true, row };
}
