/**
 * Marks the cached grouped listing (`['folder', 'list']`) stale, after a
 * create, rename, visibility change or delete of a conversation.
 *
 * The web list patches its own rows, but another list over the same cache
 * (the desktop sidebar's chats) and a remount read the cache, so the cache is
 * read again. `useDuplicateConversation` restates the same key.
 */
import { useCallback } from 'react';

import { useQueryClient } from '@tanstack/react-query';

const FOLDER_LIST_QUERY_KEY = ['folder', 'list'] as const;

export function useInvalidateFolderListing(): () => void {
  const queryClient = useQueryClient();
  return useCallback(() => void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_QUERY_KEY }), [queryClient]);
}
