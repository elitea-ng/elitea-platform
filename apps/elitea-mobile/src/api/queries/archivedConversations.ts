import { useInfiniteQuery } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import { CONVERSATIONS_PAGE_SIZE, nextConversationOffset, type ConversationPage, type ConversationSummary } from '@/api/queries/conversations';
import { queryKeys } from '@/api/queries/keys';

/**
 * The Archived chats screen (client contract 1.10): `listConversations` with `archived=only`,
 * the caller's own chats (`mine=true`, as the Chats list) that the project archived. Online
 * only: archived chats are not kept in the offline store (its list is the default one, which
 * leaves them out), so offline the screen says it needs a connection.
 */
export async function fetchArchivedPage(projectId: number, offset: number, client: ApiClient = sessionApiClient(), signal?: AbortSignal): Promise<ConversationPage> {
  const page = await unwrap(
    client.GET('/elitea_core/conversations/prompt_lib/{project_id}', {
      params: { path: { project_id: String(projectId) }, query: { mine: true, archived: 'only', limit: CONVERSATIONS_PAGE_SIZE, offset } },
      signal,
    }),
  );
  const rows: ConversationSummary[] = Array.isArray(page?.rows) ? page.rows : [];
  return { rows, total: typeof page?.total === 'number' ? page.total : rows.length, offset };
}

export function useArchivedConversations(projectId: number | null, enabled = true) {
  return useInfiniteQuery({
    queryKey: queryKeys.archived(projectId ?? 0),
    enabled: enabled && projectId !== null,
    retry: false,
    initialPageParam: 0,
    queryFn: ({ pageParam, signal }) => fetchArchivedPage(projectId as number, pageParam, sessionApiClient(), signal),
    getNextPageParam: nextConversationOffset,
  });
}
