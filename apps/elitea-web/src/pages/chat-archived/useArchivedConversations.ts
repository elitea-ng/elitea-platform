/**
 * The project's archived conversations (`listConversations` with
 * `archived=only`, client contract 1.10), page by page, and the two things
 * the page does to a row: unarchive it (it returns to the chat lists) and
 * delete it.
 *
 * The pages share the conversations list key's URL prefix, so the archive
 * hook's refresh (`useArchiveConversation`, which every list calls) reaches
 * them; a row acted on here leaves the screen at once and stays gone while
 * the refetch runs.
 *
 * On the desktop, an unarchived conversation that was a Local work thread
 * (archived from the sidebar) shows under its folder again
 * (`unarchiveLocalThread`).
 */
import { useCallback, useMemo, useState } from 'react';

import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query';

import { conversationApi } from '@/entities/conversation';
import { useArchiveConversation } from '@/features/chat-conversation-list';
import { getListConversationsQueryKey, listConversations } from '@/shared/api/generated/chat/chat';
import type { ConversationListing, ConversationSummary } from '@/shared/api/generated/model';
import { LOCAL_THREADS_KEY, unarchiveLocalThread } from '@/shared/desktop/archivedThreads';

const ARCHIVED_PAGE_SIZE = 50;

export interface ArchivedConversations {
  readonly rows: readonly ConversationSummary[];
  readonly total: number;
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly hasMore: boolean;
  readonly isFetchingMore: boolean;
  readonly loadMore: () => void;
  readonly unarchive: (row: ConversationSummary) => void;
  readonly remove: (row: ConversationSummary) => Promise<boolean>;
  readonly error: string | undefined;
  readonly dismissError: () => void;
}

export function useArchivedConversations(projectId: string | undefined): ArchivedConversations {
  const [gone, setGone] = useState<ReadonlySet<string>>(new Set());
  const [error, setError] = useState<string | undefined>(undefined);
  const queryClient = useQueryClient();
  const params = { archived: 'only', limit: ARCHIVED_PAGE_SIZE } as const;
  const query = useInfiniteQuery({
    queryKey: [...getListConversationsQueryKey(projectId ?? '', params), 'infinite'],
    queryFn: async ({ pageParam }) => (await listConversations(projectId ?? '', { ...params, offset: pageParam })).data as ConversationListing,
    initialPageParam: 0,
    getNextPageParam: (last, pages) => {
      const loaded = pages.reduce((sum, page) => sum + page.rows.length, 0);
      return last.rows.length > 0 && loaded < last.total ? loaded : undefined;
    },
    enabled: projectId !== undefined,
  });

  const hide = useCallback((id: string) => setGone((current) => new Set(current).add(id)), []);
  const toastError = useCallback((message: string) => setError(message), []);
  const { unarchive: unarchiveRequest } = useArchiveConversation({ projectId, removeRow: hide, toastError });

  const unarchive = useCallback(
    (row: ConversationSummary) => {
      const id = String(row.id);
      hide(id);
      void unarchiveRequest({ id }).then((ok) => {
        if (!ok) setGone((current) => new Set([...current].filter((entry) => entry !== id)));
        else if (unarchiveLocalThread(id)) void queryClient.invalidateQueries({ queryKey: LOCAL_THREADS_KEY });
      });
    },
    [hide, queryClient, unarchiveRequest],
  );

  const remove = useCallback(
    async (row: ConversationSummary): Promise<boolean> => {
      if (projectId === undefined) return false;
      try {
        await conversationApi.remove({ projectId, id: row.id });
        hide(String(row.id));
        return true;
      } catch {
        toastError('Failed to delete the conversation');
        return false;
      }
    },
    [hide, projectId, toastError],
  );

  const listed = useMemo(() => (query.data?.pages ?? []).flatMap((page) => page.rows), [query.data]);
  const rows = useMemo(() => listed.filter((row) => !gone.has(String(row.id))), [listed, gone]);
  // The server's count, less the rows acted on here that its last answer still listed.
  const total = Math.max(0, (query.data?.pages.at(-1)?.total ?? 0) - (listed.length - rows.length));

  return {
    rows,
    total,
    isLoading: query.isPending && projectId !== undefined,
    isError: query.isError,
    hasMore: query.hasNextPage,
    isFetchingMore: query.isFetchingNextPage,
    loadMore: () => void query.fetchNextPage(),
    unarchive,
    remove,
    error,
    dismissError: () => setError(undefined),
  };
}
